package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"path"
	"strings"
	"sync"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Plugin est le pont vers LeaSH : il n'exécute rien lui-même (il tourne
// dans l'image distroless d'Automata), tout part en HTTP vers le serveur
// d'exécution.
type Plugin struct {
	proto.UnimplementedAutomataPluginServer

	leash *LeashClient

	// host sert à fabriquer les liens de téléchargement : le plugin ne
	// signe rien lui-même, il demande à l'hôte. Renseigné à l'Initialize.
	mu   sync.Mutex
	host pluginsdk.HostClient
}

func newPlugin(leash *LeashClient) *Plugin {
	return &Plugin{leash: leash}
}

// SetHostClient implémente pluginsdk.HostClientSetter.
func (p *Plugin) SetHostClient(client pluginsdk.HostClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = client
}

func (p *Plugin) hostClient() pluginsdk.HostClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

// Describe implements proto.AutomataPluginServer.
//
// La description du sous-agent est ce que LIT L'ORCHESTRATEUR pour décider
// de déléguer : une capacité absente de ce texte n'existe pas pour lui, si
// bien qu'il répond « je n'ai pas d'outil pour ça » sans jamais appeler le
// spécialiste. Elle doit donc suivre les outils réellement montés — d'où
// la mention conditionnelle du téléchargement.
func (p *Plugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	description := "Atelier de fichiers : retouche de vidéos et d'images (ffmpeg, imagemagick), édition et conversion de documents (pandoc, LibreOffice) dans un bac à sable isolé."
	// La description est ce que l'ORCHESTRATEUR lit avant de déléguer : ce
	// qui n'y figure pas n'existe pas pour lui. Elle dit donc aussi que les
	// fichiers survivent d'un tour à l'autre et repartent vers
	// l'utilisateur — sans quoi, à « envoie-moi le fichier », il répond
	// qu'il n'a aucun outil pour cela alors que le spécialiste l'a.
	subAgentDescription := "edits videos, images and documents in a sandboxed workspace, and SENDS files back to the user " +
		"— as an attachment, or as a temporary download link when the file is too large for messaging. " +
		"Videos and images with ffmpeg and imagemagick (crop, trim, resize, remove a watermark, convert a format, extract audio); " +
		"documents with pandoc and LibreOffice (read or edit a docx or odt, write a report, produce a PDF, convert between formats). " +
		"Its workspace PERSISTS between messages: a file produced or downloaded earlier is still there, " +
		"so \"send me that file\", \"give me the video back\" or \"make it smaller and send it\" are all for this agent"

	if p.leash.fetchConfigured() {
		description += " Peut aussi télécharger une vidéo publique depuis les sites autorisés par l'exploitant."
		subAgentDescription += ". It can also DOWNLOAD a public video from a URL into the workspace before editing it — " +
			"allowed sites: " + strings.Join(downloadDomains(), ", ") +
			" — so a request like \"download this video and crop it\" is for this agent, not something to refuse"
	}

	return &proto.PluginDescriptor{
		Name:             "workspace",
		Version:          "0.1.0",
		Description:      description,
		PermissionDomain: "workspace",
		SupportsFiles:    true,
		SubAgent: &proto.SubAgentDescriptor{
			Description:  subAgentDescription,
			SystemPrompt: workspaceSystemPrompt,
			// Le travail sur média est exploratoire : inspecter, regarder
			// une trame, essayer un filtre, vérifier la taille du résultat.
			// Un plafond serré épuise le budget avant la commande finale et
			// l'agent rend un rapport au lieu d'un fichier.
			MaxSequentialToolCalls: 25,
		},
	}, nil
}

// workspaceSystemPrompt part vers le modèle : anglais uniquement.
const workspaceSystemPrompt = "You are a shell expert working inside an isolated sandbox with ffmpeg, imagemagick, pandoc and LibreOffice available. " +
	"You have no network access and no tools other than the ones listed.\n\n" +
	"When download_video is available, you can also fetch a public video by URL into the workspace before editing it; " +
	"only the sites listed in that tool's description are reachable, and nothing else on the network is.\n\n" +
	"Your workspace persists between messages for about a day: files you imported or produced earlier are still there. " +
	"For anything meant to last longer there is the locker, permanent storage the user keeps across months. " +
	"When the user asks you to keep, store or hold on to a document, call locker_save — the workspace alone would lose it. " +
	"When they refer to something they kept earlier, call locker_list, then locker_get to bring it back into the workspace. " +
	"Always call list_files first to see what you already have, and only call import_attachment for a file that is not there yet. " +
	"import_attachment can bring in any file the user sent in this conversation, not only the one attached to the message you are answering: " +
	"the files available to you are listed in your instructions. " +
	"Only ask the user to send a file again when it appears neither in list_files nor in that list.\n\n" +
	"Workflow for every request:\n" +
	"1. Call list_files. If the file you need is missing, call import_attachment with its exact filename.\n" +
	"2. Inspect it first (ffprobe for video and audio, identify for images) before deciding on any filter — never guess dimensions, duration or codecs.\n" +
	"3. If the request depends on what the media looks like — locating a logo or a watermark, checking a result — use view_file. " +
	"For a video, extract a frame first (ffmpeg -y -i input.mp4 -ss 1 -frames:v 1 frame.png) then look at that frame. " +
	"Never try to see an image by dumping pixels, histograms or text renderings of it: that wastes your budget and tells you nothing.\n" +
	"4. Run the transformation with run_command. Paths are relative to the workspace; write the result to a new file, never overwrite the input.\n" +
	"5. Prefer an output under about 15 MB so it can travel as an attachment. " +
	"For video prefer libx264 with -crf 28 and -preset veryfast, scale down if needed, and check the resulting size with ls -l before attaching.\n" +
	"6. Call attach_file with the path of the result. If it is refused for being too large, call share_file on the SAME path " +
	"and give the user the link it returns — never tell them you cannot send a file. " +
	"Do not describe the file afterwards: state briefly what you did.\n\n" +
	"When the task matches a skill from your catalog, load it first and follow it instead of improvising.\n\n" +
	"Attach as soon as you have a usable result, before checking it: a file left in the workspace is worth nothing to the user. " +
	"You can look at it and refine it afterwards if time allows.\n" +
	"Never alternate between running a command and looking at the result: one command, one check at most. " +
	"That cycle is what eats an entire budget and delivers nothing.\n\n" +
	"Produce the file: finishing the job matters more than perfecting it. If you are running out of steps, " +
	"apply the best transformation you have and attach the result rather than reporting what you would have done. " +
	"If a command fails, read the error, fix the command and retry once or twice. " +
	"If the request is impossible with these tools, say so plainly instead of pretending it worked."

// ListTools implements proto.AutomataPluginServer.
//
// DÉCISION DE SÉCURITÉ — les outils d'exécution sont déclarés read_only à
// DESSEIN. Une commande lancée dans le bac à sable — réseau coupé, seul le
// workspace du membre monté en écriture — n'a d'effet que sur les fichiers
// de ce membre. La frontière de sécurité est le sandbox, pas la
// confirmation ; exiger « confirmer » à chaque commande ffmpeg rendrait
// l'agent inutilisable. L'invariant « tout outil d'écriture passe par
// ProposedAction » reste vrai : rien ici n'écrit hors du bac à sable.
// Corollaire : le réseau ne doit JAMAIS être ouvert dans la policy de ce
// sandbox sans revenir sur cette décision.
func (p *Plugin) ListTools(context.Context, *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
	out := &proto.ListToolsOutput{
		Tools: []*proto.ToolDescriptor{
			{
				Name: "run_command",
				Description: "Run a shell script in your sandboxed workspace. ffmpeg, ffprobe and imagemagick (convert, identify, mogrify) " +
					"are available, along with the usual file commands. There is no network access. Returns stdout, stderr and the exit code.",
				InputSchemaJson: `{"type":"object","properties":{"script":{"type":"string","description":"Shell script to run. Paths are relative to the workspace."}},"required":["script"]}`,
				ReadOnly:        true,
				// Au-dessus du max_duration de la policy LeaSH (300 s) :
				// l'hôte doit laisser le serveur d'exécution rendre son
				// verdict plutôt que de couper avant lui.
				TimeoutSeconds: 330,
			},
			{
				Name:            "list_files",
				Description:     "List the files currently in your workspace, with their sizes in bytes.",
				InputSchemaJson: `{"type":"object","properties":{}}`,
				ReadOnly:        true,
				TimeoutSeconds:  60,
			},
		},
	}

	if p.leash.fetchConfigured() {
		out.Tools = append(out.Tools, downloadTool(downloadDomains()))
	}

	// share_file n'existe que si l'hôte peut réellement fabriquer un lien :
	// proposer au modèle un outil qui répondra « non câblé » lui ferait
	// perdre un appel et une explication.
	if p.hostClient() != nil {
		out.Tools = append(out.Tools, shareTool())
		// Le casier a besoin du magasin d'objets de l'hôte, comme
		// share_file a besoin de sa fabrique de liens.
		out.Tools = append(out.Tools, lockerTools()...)
	}

	return out, nil
}

// shareTool décrit share_file : la seconde voie de sortie, pour ce qui ne
// tient pas dans une pièce jointe.
func shareTool() *proto.ToolDescriptor {
	return &proto.ToolDescriptor{
		Name: "share_file",
		Description: "Give the user a temporary download link to a file of your workspace. " +
			"Use this when attach_file refuses a file for being too large — never tell the user you cannot send it. " +
			"The link works for 24 hours and needs no account; give it to them as it is.",
		InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string","description":"Path of the file in your workspace, as returned by list_files."}},"required":["path"]}`,
		// read_only : le lien ne modifie rien, il expose au membre son
		// propre fichier, dans la conversation où il vient de le demander.
		ReadOnly:       true,
		TimeoutSeconds: 60,
	}
}

// downloadTool décrit download_video. Monté seulement quand l'exploitant a
// configuré la clé de la policy réseau : sans elle, l'outil n'existe pas
// pour le modèle, plutôt que d'échouer à l'usage.
func downloadTool(domains []string) *proto.ToolDescriptor {
	return &proto.ToolDescriptor{
		Name: "download_video",
		Description: "Download a public video into your workspace so you can edit it. " +
			"Only these sites are allowed: " + strings.Join(domains, ", ") + ". " +
			"Playlists are not downloaded, only a single video, capped in size and duration.",
		InputSchemaJson: `{"type":"object","properties":{"url":{"type":"string","description":"Public URL of the video page."},"name":{"type":"string","description":"Optional output name without extension (letters, digits, dashes)."}},"required":["url"]}`,
		// read_only comme run_command, et pour la même raison : la
		// frontière est le bac à sable. Celui-ci n'expose QUE fetch-video,
		// et n'écrit que dans le workspace du membre.
		ReadOnly:       true,
		TimeoutSeconds: 630,
	}
}

// CallTool implements proto.AutomataPluginServer.
//
// Un échec métier (commande refusée par la policy, serveur injoignable)
// ressort en IsError, jamais en erreur Go : le modèle doit pouvoir
// l'expliquer à l'utilisateur au lieu de faire échouer le tour.
func (p *Plugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	if in.Ctx == nil || in.Ctx.OrgId == "" || in.Ctx.MemberId == "" {
		return errorOutput("the workspace could not be identified"), nil
	}

	switch in.Name {
	case "run_command":
		return p.runCommand(ctx, in)
	case "list_files":
		return p.listFiles(ctx, in)
	case "download_video":
		return p.downloadVideo(ctx, in)
	case "share_file":
		return p.shareFile(ctx, in)
	case "locker_list":
		return p.lockerList(ctx, in)
	case "locker_save":
		return p.lockerSave(ctx, in, false)
	case "locker_replace":
		return p.lockerSave(ctx, in, true)
	case "locker_get":
		return p.lockerGet(ctx, in)
	case "locker_delete":
		return p.lockerDelete(ctx, in)
	default:
		return errorOutput(fmt.Sprintf("unknown tool %q", in.Name)), nil
	}
}

func (p *Plugin) runCommand(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	var args struct {
		Script string `json:"script"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return errorOutput("invalid parameters"), nil
		}
	}
	if strings.TrimSpace(args.Script) == "" {
		return errorOutput("parameter 'script' is required"), nil
	}

	text, isError, err := p.leash.Execute(ctx, in.Ctx.OrgId, in.Ctx.MemberId, args.Script)
	if err != nil {
		// Journaux : identifiants et compteurs seulement, jamais le script.
		slog.WarnContext(ctx, "workspace: exécution en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the command could not be run: %v", err)), nil
	}

	slog.InfoContext(ctx, "workspace: commande exécutée", "org_id", in.Ctx.OrgId, "is_error", isError)

	return &proto.CallToolOutput{ResultText: text, IsError: isError}, nil
}

// downloadVideo télécharge une vidéo publique dans le workspace, via le
// bac à sable réseau (policy « fetch », clé distincte). L'URL est validée
// ici contre la liste blanche de l'exploitant AVANT d'atteindre LeaSH.
func (p *Plugin) downloadVideo(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	// Le schéma nomme le paramètre « url », mais un modèle en invente
	// volontiers une variante (video_url, link…) et l'outil paraît alors
	// cassé alors qu'il n'a rien reçu. On accepte les synonymes plutôt que
	// de renvoyer une erreur que le modèle interprète comme une panne.
	var args struct {
		URL      string `json:"url"`
		VideoURL string `json:"video_url"`
		Link     string `json:"link"`
		Name     string `json:"name"`
		Output   string `json:"output"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return errorOutput("invalid parameters: pass {\"url\": \"https://...\"}"), nil
		}
	}

	rawURL := cmp.Or(args.URL, args.VideoURL, args.Link)
	if rawURL == "" {
		return errorOutput("the 'url' parameter is required: pass {\"url\": \"https://...\"}"), nil
	}
	if args.Name == "" {
		args.Name = args.Output
	}

	target, err := validateDownloadURL(rawURL, downloadDomains())
	if err != nil {
		// Journaliser le REFUS, pas l'URL (contenu utilisateur) : sans
		// cette trace, un téléchargement refusé est indiscernable d'un
		// téléchargement en panne côté exploitation.
		slog.InfoContext(ctx, "workspace: téléchargement refusé",
			"org_id", in.Ctx.OrgId, "reason", err.Error())
		return errorOutput(err.Error()), nil
	}
	name, err := validateOutputName(args.Name)
	if err != nil {
		return errorOutput(err.Error()), nil
	}

	text, isError, err := p.leash.Fetch(ctx, in.Ctx.OrgId, in.Ctx.MemberId, target, name)
	if err != nil {
		// Journaux : jamais l'URL, qui est du contenu utilisateur.
		slog.WarnContext(ctx, "workspace: téléchargement en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the video could not be downloaded: %v", err)), nil
	}

	slog.InfoContext(ctx, "workspace: vidéo téléchargée", "org_id", in.Ctx.OrgId, "is_error", isError)

	if isError {
		// Un échec de yt-dlp se lit mal : « This video is not available »
		// désigne aussi bien une vidéo privée qu'une panne de notre côté.
		// Sans consigne, l'agent rejoue la même vidéo sous toutes ses
		// formes d'URL puis livre à l'utilisateur un diagnostic inventé.
		if advice := downloadFailureAdvice(text); advice != "" {
			text += "\n\n" + advice
		}
		return &proto.CallToolOutput{ResultText: text, IsError: true}, nil
	}
	return &proto.CallToolOutput{ResultText: text +
		"\nThe file is now in your workspace: call list_files to see its exact name before working on it."}, nil
}

// shareFile fabrique un lien de téléchargement temporaire vers un fichier
// du workspace. L'existence du fichier est vérifiée ICI : l'hôte, lui, ne
// sait pas lire le workspace, et un lien vers un fichier absent ne se
// découvrirait qu'à l'ouverture, chez l'utilisateur.
func (p *Plugin) shareFile(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil {
		return errorOutput("sharing links is not available on this instance."), nil
	}

	var args struct {
		Path string `json:"path"`
		File string `json:"file"`
	}
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return errorOutput("invalid parameters"), nil
		}
	}

	target := strings.TrimSpace(cmp.Or(args.Path, args.File))
	if target == "" {
		return errorOutput("parameter 'path' is required"), nil
	}

	entries, err := p.leash.ListFiles(ctx, in.Ctx.OrgId, in.Ctx.MemberId)
	if err != nil {
		return errorOutput(fmt.Sprintf("the workspace could not be listed: %v", err)), nil
	}

	var found bool
	var size int64
	for _, entry := range entries {
		if entry.Path == target {
			found, size = true, entry.Size
			break
		}
	}
	if !found {
		return errorOutput("no file named " + target + " in your workspace. Call list_files to see the exact names."), nil
	}

	url, expiresAt, err := host.ShareFile(ctx, in.Ctx.OrgId, in.Ctx.MemberId, target)
	if err != nil {
		slog.WarnContext(ctx, "workspace: fabrication du lien en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the download link could not be created: %v", err)), nil
	}

	slog.InfoContext(ctx, "workspace: lien de téléchargement créé",
		"org_id", in.Ctx.OrgId, "bytes", size)

	return &proto.CallToolOutput{ResultText: fmt.Sprintf(
		"Download link (valid until %s, no account needed):\n%s\n\nGive this link to the user as it is.",
		expiresAt, url)}, nil
}

func (p *Plugin) listFiles(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	entries, err := p.leash.ListFiles(ctx, in.Ctx.OrgId, in.Ctx.MemberId)
	if err != nil {
		slog.WarnContext(ctx, "workspace: listage en échec", "org_id", in.Ctx.OrgId, "error", err)
		return errorOutput(fmt.Sprintf("the workspace could not be listed: %v", err)), nil
	}

	if len(entries) == 0 {
		return &proto.CallToolOutput{ResultText: "The workspace is empty."}, nil
	}

	var sb strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&sb, "%s\t%d bytes\n", entry.Path, entry.Size)
	}

	return &proto.CallToolOutput{ResultText: sb.String()}, nil
}

// PutFile implements proto.AutomataPluginServer: the host pushes a file the
// user attached to their message into the member's workspace.
func (p *Plugin) PutFile(stream proto.AutomataPlugin_PutFileServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("workspace: réception des métadonnées: %w", err)
	}

	meta, ok := first.Payload.(*proto.PutFileChunk_Metadata)
	if !ok || meta.Metadata == nil {
		return errors.New("workspace: le premier fragment doit porter les métadonnées")
	}
	callCtx := meta.Metadata.Ctx
	if callCtx == nil || callCtx.OrgId == "" || callCtx.MemberId == "" {
		return errors.New("workspace: contexte d'appel incomplet")
	}

	name := safeFilename(meta.Metadata.Filename, meta.Metadata.MimeType)

	// Le flux gRPC entrant alimente directement le corps de la requête
	// vers LeaSH : les octets traversent, ils ne s'accumulent nulle part.
	var received int64
	body := pluginsdk.RecvFile(func() ([]byte, error) {
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			return nil, recvErr
		}
		payload, isData := chunk.Payload.(*proto.PutFileChunk_Data)
		if !isData {
			return nil, errors.New("workspace: fragment inattendu après les métadonnées")
		}
		received += int64(len(payload.Data))
		return payload.Data, nil
	})

	stored, err := p.leash.PutFile(stream.Context(), callCtx.OrgId, callCtx.MemberId, name, meta.Metadata.MimeType, body)
	if err != nil {
		slog.WarnContext(stream.Context(), "workspace: dépôt de fichier en échec",
			"org_id", callCtx.OrgId, "bytes", received, "error", err)
		return stream.SendAndClose(&proto.PutFileResult{
			IsError:   true,
			ErrorText: fmt.Sprintf("the file could not be stored in the workspace: %v", err),
		})
	}

	slog.InfoContext(stream.Context(), "workspace: fichier déposé", "org_id", callCtx.OrgId, "bytes", received)

	return stream.SendAndClose(&proto.PutFileResult{Path: stored})
}

// GetFile implements proto.AutomataPluginServer: the host pulls a file the
// sub-agent produced, to attach it to the reply.
func (p *Plugin) GetFile(req *proto.GetFileRequest, stream proto.AutomataPlugin_GetFileServer) error {
	if req.Ctx == nil || req.Ctx.OrgId == "" || req.Ctx.MemberId == "" {
		return errors.New("workspace: contexte d'appel incomplet")
	}
	if req.Path == "" {
		return errors.New("workspace: chemin requis")
	}

	body, contentType, size, err := p.leash.OpenFile(stream.Context(), req.Ctx.OrgId, req.Ctx.MemberId, req.Path)
	if err != nil {
		slog.WarnContext(stream.Context(), "workspace: récupération de fichier en échec",
			"org_id", req.Ctx.OrgId, "error", err)
		return fmt.Errorf("workspace: récupération du fichier: %w", err)
	}
	defer func() { _ = body.Close() }()

	mimeType := normalizeContentType(contentType)
	if mimeType == "" {
		mimeType = mimeFromExtension(req.Path)
	}

	metaErr := stream.Send(&proto.FileChunk{Payload: &proto.FileChunk_Metadata{Metadata: &proto.FileMetadata{
		Filename: path.Base(req.Path),
		MimeType: mimeType,
		Size:     uint64(max(size, 0)),
	}}})
	if metaErr != nil {
		return fmt.Errorf("workspace: envoi des métadonnées: %w", metaErr)
	}

	// Relais tranche par tranche : c'est ce qui rend possible le partage
	// d'un fichier de plusieurs centaines de mégaoctets.
	if err := pluginsdk.SendFile(func(data []byte) error {
		return stream.Send(&proto.FileChunk{Payload: &proto.FileChunk_Data{Data: data}})
	}, body); err != nil {
		return fmt.Errorf("workspace: envoi d'une tranche: %w", err)
	}

	slog.InfoContext(stream.Context(), "workspace: fichier récupéré",
		"org_id", req.Ctx.OrgId, "announced_bytes", size)

	return nil
}

// safeFilename ramène un nom venu de l'utilisateur à un nom de fichier
// simple, déposé à la racine du workspace. Le confinement des chemins est
// réappliqué côté LeaSH, mais rien ne justifie de lui envoyer un chemin
// douteux.
func safeFilename(name, mimeType string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "/") {
		name = ""
	}
	if name == "" {
		return "input" + extensionFor(mimeType)
	}

	return name
}

// usualExtensions fixe l'extension des types que ce plugin manipule.
// mime.ExtensionsByType retourne une liste triée alphabétiquement dont la
// première entrée est souvent marginale : « .f4v » pour video/mp4, comme
// « .jfif » pour image/jpeg côté hôte. ffmpeg se fie à l'extension du
// fichier de sortie pour choisir son conteneur : se tromper ici casse la
// commande, pas seulement l'affichage.
var usualExtensions = map[string]string{
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
}

// extensionFor choisit l'extension d'un type MIME.
func extensionFor(mimeType string) string {
	if extension, found := usualExtensions[strings.ToLower(strings.TrimSpace(mimeType))]; found {
		return extension
	}
	if extensions, err := mime.ExtensionsByType(mimeType); err == nil && len(extensions) > 0 {
		return extensions[0]
	}

	return ""
}

// normalizeContentType retire les paramètres d'un Content-Type.
func normalizeContentType(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	if parsed == "application/octet-stream" {
		return ""
	}

	return parsed
}

// mimeFromExtension devine le type à partir de l'extension quand le
// serveur n'en annonce pas d'utile.
func mimeFromExtension(p string) string {
	if guessed := mime.TypeByExtension(path.Ext(p)); guessed != "" {
		parsed, _, err := mime.ParseMediaType(guessed)
		if err == nil {
			return parsed
		}
	}

	return "application/octet-stream"
}

func errorOutput(text string) *proto.CallToolOutput {
	return &proto.CallToolOutput{ResultText: text, IsError: true}
}
