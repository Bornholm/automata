package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path"
	"strings"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Plugin est le pont vers LeaSH : il n'exécute rien lui-même (il tourne
// dans l'image distroless d'Automata), tout part en HTTP vers le serveur
// d'exécution.
type Plugin struct {
	proto.UnimplementedAutomataPluginServer

	leash *LeashClient
}

func newPlugin(leash *LeashClient) *Plugin {
	return &Plugin{leash: leash}
}

// maxImportBytes borne un fichier importé depuis l'hôte. L'hôte applique
// déjà attachments.max_tool_size ; cette borne est la ceinture du plugin.
const maxImportBytes = 32 << 20

// fileChunkBytes est la taille des tranches renvoyées à l'hôte, bien sous
// la limite de message gRPC.
const fileChunkBytes = 1 << 20

// Describe implements proto.AutomataPluginServer.
func (p *Plugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:             "workspace",
		Version:          "0.1.0",
		Description:      "Atelier de fichiers : retouche de vidéos et d'images (ffmpeg, imagemagick), édition et conversion de documents (pandoc, LibreOffice) dans un bac à sable isolé.",
		PermissionDomain: "workspace",
		SupportsFiles:    true,
		SubAgent: &proto.SubAgentDescriptor{
			Description: "edits videos, images and documents in a sandboxed workspace. " +
				"Videos and images with ffmpeg and imagemagick (crop, trim, resize, remove a watermark, convert a format, extract audio); " +
				"documents with pandoc and LibreOffice (read or edit a docx or odt, write a report, produce a PDF, convert between formats)",
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
	"Your workspace persists between messages for about a day: files you imported or produced earlier are still there. " +
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
	"5. Always re-encode the output so it stays under about 15 MB — messaging platforms reject anything larger. " +
	"For video prefer libx264 with -crf 28 and -preset veryfast, scale down if needed, and check the resulting size with ls -l before attaching.\n" +
	"6. Call attach_file with the path of the result. Do not describe the file afterwards: state briefly what you did.\n\n" +
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
	return &proto.ListToolsOutput{
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
	}, nil
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

	var data []byte
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("workspace: réception d'une tranche: %w", recvErr)
		}

		payload, isData := chunk.Payload.(*proto.PutFileChunk_Data)
		if !isData {
			return errors.New("workspace: fragment inattendu après les métadonnées")
		}

		data = append(data, payload.Data...)
		if len(data) > maxImportBytes {
			return stream.SendAndClose(&proto.PutFileResult{
				IsError:   true,
				ErrorText: fmt.Sprintf("the file exceeds %d bytes", maxImportBytes),
			})
		}
	}

	name := safeFilename(meta.Metadata.Filename, meta.Metadata.MimeType)

	stored, err := p.leash.PutFile(stream.Context(), callCtx.OrgId, callCtx.MemberId, name, meta.Metadata.MimeType, data)
	if err != nil {
		slog.WarnContext(stream.Context(), "workspace: dépôt de fichier en échec",
			"org_id", callCtx.OrgId, "bytes", len(data), "error", err)
		return stream.SendAndClose(&proto.PutFileResult{
			IsError:   true,
			ErrorText: fmt.Sprintf("the file could not be stored in the workspace: %v", err),
		})
	}

	slog.InfoContext(stream.Context(), "workspace: fichier déposé", "org_id", callCtx.OrgId, "bytes", len(data))

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

	data, contentType, err := p.leash.GetFile(stream.Context(), req.Ctx.OrgId, req.Ctx.MemberId, req.Path, maxImportBytes)
	if err != nil {
		slog.WarnContext(stream.Context(), "workspace: récupération de fichier en échec",
			"org_id", req.Ctx.OrgId, "error", err)
		return fmt.Errorf("workspace: récupération du fichier: %w", err)
	}

	mimeType := normalizeContentType(contentType)
	if mimeType == "" {
		mimeType = mimeFromExtension(req.Path)
	}

	metaErr := stream.Send(&proto.FileChunk{Payload: &proto.FileChunk_Metadata{Metadata: &proto.FileMetadata{
		Filename: path.Base(req.Path),
		MimeType: mimeType,
		Size:     uint64(len(data)),
	}}})
	if metaErr != nil {
		return fmt.Errorf("workspace: envoi des métadonnées: %w", metaErr)
	}

	for offset := 0; offset < len(data); offset += fileChunkBytes {
		end := min(offset+fileChunkBytes, len(data))
		if err := stream.Send(&proto.FileChunk{Payload: &proto.FileChunk_Data{Data: data[offset:end]}}); err != nil {
			return fmt.Errorf("workspace: envoi d'une tranche: %w", err)
		}
	}

	slog.InfoContext(stream.Context(), "workspace: fichier récupéré", "org_id", req.Ctx.OrgId, "bytes", len(data))

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
