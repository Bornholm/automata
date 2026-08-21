package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

// Sous-agents fournis par des plugins. Le plugin déclare un prompt système,
// une description et des outils (Describe) ; l'hôte exécute la boucle LLM
// avec ses propres clients — comptabilité d'usage, débit de crédits et
// autorisations restent dans le socle, le plugin ne voit jamais un modèle.
//
// Les types de ce fichier doublent volontairement ceux d'internal/plugin :
// agent ne doit pas importer plugin (le registre câble les deux), et
// plugin ne doit jamais importer agent.

// PluginToolCaller exécute un outil d'un plugin. Implémenté par
// internal/plugin.Manager, qui y ajoute timeout, troncature et métriques.
type PluginToolCaller interface {
	// CallPluginTool retourne le texte de résultat et isError (échec
	// métier, à relayer au modèle sans avorter le tour).
	CallPluginTool(ctx context.Context, pluginName, toolName string, callCtx PluginCallContext, argsJSON string, timeoutSeconds int) (result string, isError bool, err error)
}

// PluginFileTransfer déplace des octets entre l'hôte et un plugin, hors du
// canal des résultats d'outils : ceux-ci sont textuels et bornés à 48 Ko,
// une vidéo n'y a rien à faire. Implémenté par internal/plugin.Manager.
type PluginFileTransfer interface {
	PutPluginFile(ctx context.Context, pluginName string, callCtx PluginCallContext, filename, mimeType string, data []byte) (path string, isError bool, errText string, err error)
	GetPluginFile(ctx context.Context, pluginName string, callCtx PluginCallContext, path string, maxBytes int64) (filename, mimeType string, data []byte, err error)
}

// PluginCallContext est l'identité transmise au plugin. Toujours
// construite par l'hôte. MemberID est un identifiant de confiance côté
// serveur : il part au plugin, jamais dans un texte destiné au modèle.
type PluginCallContext struct {
	OrgID          string
	MemberID       string
	Scope          string
	ScopeID        string
	IdempotencyKey string
}

// PluginSubAgentSpec décrit un sous-agent prêt à être monté comme
// spécialiste délégué.
type PluginSubAgentSpec struct {
	PluginName       string
	SystemPrompt     string
	Description      string
	PermissionDomain string
	MaxToolCalls     int
	// SupportsFiles monte les outils fichiers de l'hôte sur ce sous-agent.
	SupportsFiles bool
	Tools         []PluginToolSpec
}

// PluginToolSpec décrit un outil du plugin. ReadOnly faux = écriture :
// l'outil n'est jamais exécuté pendant le tour, seulement proposé à la
// confirmation humaine — même position prudente qu'isWriteTool.
type PluginToolSpec struct {
	Name        string
	Description string
	SchemaJSON  string
	ReadOnly    bool
	// TimeoutSeconds est le timeout demandé par le plugin pour cet outil.
	// 0 : défaut de l'hôte. L'hôte le plafonne quoi qu'il arrive.
	TimeoutSeconds int
}

// PluginSpecialistProvider fournit, par tour, les sous-agents des plugins
// actifs pour l'organisation de l'identité. Implémenté dans
// internal/registry ; nil-safe côté orchestrateur.
type PluginSpecialistProvider interface {
	SpecialistsFor(ctx context.Context, identity model.ExecutionIdentity) (map[string]delegation.Specialist, map[string]string)
}

// PluginSubAgent exécute une délégation avec les outils d'un plugin.
// Implémente delegation.Specialist.
type PluginSubAgent struct {
	spec                PluginSubAgentSpec
	client              llm.ChatCompletionClient
	caller              PluginToolCaller
	files               PluginFileTransfer
	maxFileBytes        int64
	vision              llm.ChatCompletionClient
	maxToolContextBytes int64
	logger              *slog.Logger
}

// NewPluginSubAgent construit le sous-agent d'un plugin.
func NewPluginSubAgent(spec PluginSubAgentSpec, client llm.ChatCompletionClient, caller PluginToolCaller, maxToolContextBytes int64, logger *slog.Logger) *PluginSubAgent {
	if logger == nil {
		logger = slog.Default()
	}
	return &PluginSubAgent{
		spec:                spec,
		client:              client,
		caller:              caller,
		maxToolContextBytes: maxToolContextBytes,
		logger:              logger,
	}
}

// WithFiles branche le transfert de fichiers hôte ↔ plugin et la taille
// maximale d'un fichier échangé. Sans cet appel, les outils fichiers ne
// sont pas montés, même si le plugin les déclare : l'hôte ne propose que
// ce qu'il sait réellement servir.
func (a *PluginSubAgent) WithFiles(files PluginFileTransfer, maxFileBytes int64) *PluginSubAgent {
	a.files = files
	a.maxFileBytes = maxFileBytes
	return a
}

// WithVision branche le client multimodal qui sert l'outil view_file. Le
// modèle du sous-agent est texte-seul : sans ce client, un agent qui
// manipule des images travaille en aveugle et gaspille son budget d'outils
// à sonder des histogrammes. Sans cet appel, view_file n'est pas monté.
func (a *PluginSubAgent) WithVision(client llm.ChatCompletionClient) *PluginSubAgent {
	a.vision = client
	return a
}

// SupportsFiles implémente delegation.FileCapable : c'est ce qui décide
// si l'orchestrateur transmet à ce sous-agent les fichiers déjà reçus dans
// la conversation. La capacité est déclarée par le plugin ET servie par
// l'hôte — un plugin qui la déclare sans que WithFiles ait été appelé
// n'obtient rien.
func (a *PluginSubAgent) SupportsFiles() bool {
	return a.spec.SupportsFiles && a.files != nil
}

// Execute implémente delegation.Specialist. Comme AgentSpecialist, seuls
// Goal/RelevantInput/Constraints et les pièces jointes du tour composent
// l'entrée — jamais l'historique de la conversation principale.
func (a *PluginSubAgent) Execute(ctx context.Context, req delegation.Request) (delegation.Result, error) {
	agentName := a.spec.PluginName
	ctx = withUsageAttribution(ctx, req.Identity, "plugin:"+agentName)

	collector := newProposalCollector()

	tools := make([]llm.Tool, 0, len(a.spec.Tools))
	for _, spec := range a.spec.Tools {
		tool, err := a.buildTool(spec, req.Identity, collector)
		if err != nil {
			return delegation.Result{}, fmt.Errorf("agent: outil %q du plugin %q: %w", spec.Name, agentName, err)
		}
		tools = append(tools, tool)
	}
	mediaCollector := newMediaCollector()

	if a.spec.SupportsFiles && a.files != nil {
		tools = append(tools,
			a.newImportAttachmentTool(req),
			a.newAttachFileTool(req, mediaCollector),
		)
		if a.vision != nil {
			tools = append(tools, a.newViewFileTool(req))
		}
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	messages := buildChatMessages(a.spec.SystemPrompt, agentName, false, "", Request{
		Identity: req.Identity,
		// Les fichiers antérieurs sont annoncés dans l'entrée, pas ajoutés
		// aux pièces jointes : buildChatMessages les décrirait comme
		// « jointes à ce message », ce qu'elles ne sont pas.
		Input:       buildDelegationInput(req) + media.EarlierToolOnlyNotice(req.RecentAttachments),
		Attachments: req.Attachments,
		// History volontairement omis : garantie d'isolation des délégués.
	})

	maxIterations := a.spec.MaxToolCalls
	if maxIterations <= 0 {
		maxIterations = 5
	}

	loopResult, err := runToolLoop(withMediaCollector(ctx, mediaCollector), a.client, messages, tools, maxIterations, a.maxToolContextBytes, ErrMaxToolCallsReached, a.logger, "plugin:"+agentName)
	if err != nil {
		return delegation.Result{}, err
	}

	return delegation.Result{
		Summary:         loopResult.Text,
		ProposedActions: collector.take(),
		Attachments:     append(append([]media.Media(nil), loopResult.Attachments...), mediaCollector.take()...),
	}, nil
}

// buildTool transforme un descripteur d'outil de plugin en llm.Tool.
// L'identité est capturée en closure — jamais décidée par le modèle.
func (a *PluginSubAgent) buildTool(spec PluginToolSpec, identity model.ExecutionIdentity, collector *proposalCollector) (llm.Tool, error) {
	var schema map[string]any
	if spec.SchemaJSON != "" {
		if err := json.Unmarshal([]byte(spec.SchemaJSON), &schema); err != nil {
			return nil, fmt.Errorf("schéma illisible: %w", err)
		}
	}
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}

	callCtx := PluginCallContext{
		OrgID:    string(identity.OrgID),
		MemberID: string(identity.PrincipalID),
		Scope:    string(identity.Scope),
		ScopeID:  string(identity.ScopeID),
	}

	if spec.ReadOnly {
		execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			argsJSON, err := json.Marshal(params)
			if err != nil {
				return llm.NewToolResult("erreur: arguments non sérialisables."), nil
			}

			result, isError, err := a.caller.CallPluginTool(ctx, a.spec.PluginName, spec.Name, callCtx, string(argsJSON), spec.TimeoutSeconds)
			if err != nil {
				// Convention du dépôt : un échec d'outil est relayé au
				// modèle, jamais transformé en erreur qui avorte le tour.
				return llm.NewToolResult(fmt.Sprintf("erreur: l'outil %q est indisponible.", spec.Name)), nil
			}
			if isError {
				return llm.NewToolResult("erreur: " + result), nil
			}
			return llm.NewToolResult(result), nil
		}

		return llm.NewFuncTool(spec.Name, spec.Description, schema, execute).WithReadOnlyHint(true), nil
	}

	// Écriture : JAMAIS exécutée pendant le tour. L'appel devient une
	// action proposée, confirmée par un « confirmer » littéral de
	// l'utilisateur puis rejouée par l'exécuteur du plugin
	// (internal/plugin.ActionExecutor). Aucun réglage — du membre, de
	// l'administrateur ou du plugin — ne peut débrayer ce passage.
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params))
		for k, v := range params {
			args[k] = v
		}

		collector.add(delegation.ProposedAction{
			Summary:            summarizeAction(spec.Name, args),
			AgentID:            "plugin:" + a.spec.PluginName,
			MCPServer:          "plugin:" + a.spec.PluginName,
			ToolName:           spec.Name,
			Arguments:          args,
			RequiredPermission: fmt.Sprintf("%s.%s.write", a.spec.PermissionDomain, identity.Scope),
			Scope:              identity.Scope,
			ScopeID:            identity.ScopeID,
		})

		return llm.NewToolResult(
			"Action enregistrée, en attente de la confirmation de l'utilisateur : elle n'a PAS encore été exécutée. " +
				"Tu peux en enregistrer d'autres dans le même tour, elles seront confirmées ensemble. " +
				"N'essaie pas de les confirmer toi-même.",
		), nil
	}

	return llm.NewFuncTool(
		spec.Name,
		spec.Description+" (the action requires the user's confirmation before anything is executed)",
		schema,
		execute,
	), nil
}

// pluginCallContext construit l'identité d'appel du tour. MemberID est un
// identifiant de confiance : il part au plugin, jamais au modèle.
func pluginCallContext(identity model.ExecutionIdentity) PluginCallContext {
	return PluginCallContext{
		OrgID:    string(identity.OrgID),
		MemberID: string(identity.PrincipalID),
		Scope:    string(identity.Scope),
		ScopeID:  string(identity.ScopeID),
	}
}

// newImportAttachmentTool expose import_attachment : il pousse une pièce
// jointe du TOUR COURANT vers l'espace de travail du plugin et rend le
// chemin obtenu. Les octets ne traversent jamais la conversation, seul le
// chemin remonte au modèle.
//
// La recherche couvre les pièces du tour courant PUIS celles des messages
// précédents (RecentAttachments) : un membre envoie une vidéo, puis demande
// sa transformation au message suivant — c'est le geste naturel, il doit
// fonctionner. Une pièce absente donne un résultat d'outil qui oriente
// l'agent vers list_files, bien plus utile qu'un « fichier introuvable ».
func (a *PluginSubAgent) newImportAttachmentTool(req delegation.Request) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("filename", "Exact name of the file attached to the user's message, as listed to you.", "string")

	callCtx := pluginCallContext(req.Identity)
	// Les pièces du tour d'abord : à noms égaux, c'est le fichier que
	// l'utilisateur vient d'envoyer qui l'emporte sur son homonyme ancien.
	attachments := append(append([]media.Media(nil), req.Attachments...), req.RecentAttachments...)

	return llm.NewFuncTool(
		"import_attachment",
		"Import a file the user sent — with this message or earlier in the conversation — into your workspace so the other tools can work on it. Returns the path to use.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			filename, _ := params["filename"].(string)
			if filename == "" {
				return llm.NewToolResult("error: 'filename' is required."), nil
			}

			found, ok := findAttachment(attachments, filename)
			if !ok {
				return llm.NewToolResult(
					"error: no file named " + filename + " was found in this conversation. " +
						"Check list_files: it may already be in your workspace. " +
						"Otherwise ask the user to send it again.",
				), nil
			}

			path, isError, errText, err := a.files.PutPluginFile(ctx, a.spec.PluginName, callCtx, found.Filename, found.MimeType, found.Data)
			if err != nil {
				a.logger.WarnContext(ctx, "agent: import de pièce jointe en échec",
					"plugin", a.spec.PluginName, "bytes", len(found.Data), "error", err)
				return llm.NewToolResult("error: the file could not be imported into the workspace."), nil
			}
			if isError {
				return llm.NewToolResult("error: " + errText), nil
			}

			return llm.NewToolResult("File imported. Path in the workspace: " + path), nil
		},
	).WithReadOnlyHint(true)
}

// newAttachFileTool expose attach_file : il récupère un fichier de
// l'espace de travail du plugin et le verse au collecteur de médias, d'où
// il repart en pièce jointe de la réponse (Result.Attachments), sans
// jamais repasser par le modèle.
func (a *PluginSubAgent) newAttachFileTool(req delegation.Request, collector *mediaCollector) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("path", "Path of the file in your workspace, as returned by your other tools.", "string")

	callCtx := pluginCallContext(req.Identity)

	return llm.NewFuncTool(
		"attach_file",
		"Attach a file from your workspace to the reply sent to the user. Do NOT describe the file afterwards, just confirm briefly what you produced.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			path, _ := params["path"].(string)
			if path == "" {
				return llm.NewToolResult("error: 'path' is required."), nil
			}

			filename, mimeType, data, err := a.files.GetPluginFile(ctx, a.spec.PluginName, callCtx, path, a.maxFileBytes)
			if err != nil {
				a.logger.WarnContext(ctx, "agent: récupération de fichier en échec",
					"plugin", a.spec.PluginName, "error", err)
				return llm.NewToolResult(
					"error: the file could not be attached (it may be missing, or too large to send: " +
						"re-encode it smaller and try again).",
				), nil
			}
			if len(data) == 0 {
				return llm.NewToolResult("error: the file is empty."), nil
			}

			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			kind, ok := media.KindFromMIME(mimeType)
			if !ok {
				kind = media.KindDocument
			}
			if filename == "" {
				filename = media.DefaultFilename(mimeType)
			}

			collector.add(media.Media{
				Kind:     kind,
				MimeType: mimeType,
				Filename: filename,
				Data:     data,
			})

			return llm.NewToolResult(
				"File attached to the reply. Confirm briefly what you produced; do not describe its content.",
			), nil
		},
	).WithReadOnlyHint(true)
}

// visionSystemPrompt part vers le modèle : anglais uniquement. Il vise la
// réponse dont l'agent a besoin — des coordonnées exploitables par une
// commande — plutôt qu'une description d'image.
const visionSystemPrompt = "You are the eyes of an agent that edits images and videos with ffmpeg and imagemagick. " +
	"Answer its question about the image precisely and briefly. " +
	"Whenever you locate something in the image, give its position and size in pixels as x, y, width, height, " +
	"with the origin at the top-left corner, and state the image dimensions you based them on. " +
	"If you cannot see what is asked, say so plainly instead of guessing."

// newViewFileTool expose view_file : il va chercher une image dans
// l'espace de travail du plugin et la soumet au client multimodal, dont la
// réponse textuelle revient au sous-agent.
//
// Les octets ne traversent jamais la conversation ni le modèle du
// sous-agent (texte-seul) : seule la description remonte.
func (a *PluginSubAgent) newViewFileTool(req delegation.Request) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("path", "Path of the image in your workspace. Extract a frame with ffmpeg first to look at a video.", "string").
		RequiredProperty("question", "What you need to know about the image, e.g. \"where is the logo, and how big is it?\".", "string")

	callCtx := pluginCallContext(req.Identity)

	return llm.NewFuncTool(
		"view_file",
		"Look at an image in your workspace and ask a question about it. Use this instead of probing pixels or histograms: "+
			"to locate something in a video, extract a frame with ffmpeg, then look at that frame.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			path, _ := params["path"].(string)
			question, _ := params["question"].(string)
			if path == "" || question == "" {
				return llm.NewToolResult("error: 'path' and 'question' are required."), nil
			}

			filename, mimeType, data, err := a.files.GetPluginFile(ctx, a.spec.PluginName, callCtx, path, a.maxFileBytes)
			if err != nil {
				a.logger.WarnContext(ctx, "agent: lecture de fichier pour la vision en échec",
					"plugin", a.spec.PluginName, "error", err)
				return llm.NewToolResult("error: the file could not be read (it may be missing, or too large to look at)."), nil
			}
			if len(data) == 0 {
				return llm.NewToolResult("error: the file is empty."), nil
			}

			// Seules les images sont soumises : un fichier vidéo entier
			// coûterait cher et serait refusé par la plupart des
			// fournisseurs. Le message dit à l'agent quoi faire à la place.
			if kind, ok := media.KindFromMIME(mimeType); !ok || kind != media.KindImage {
				return llm.NewToolResult(
					"error: view_file only accepts images. Extract a frame first, " +
						"for example: ffmpeg -y -i input.mp4 -ss 1 -frames:v 1 frame.png",
				), nil
			}

			attachment, err := media.ToLLM(media.Media{
				Kind:     media.KindImage,
				MimeType: mimeType,
				Filename: filename,
				Data:     data,
			})
			if err != nil {
				return llm.NewToolResult("error: this image could not be read."), nil
			}

			resp, err := a.vision.ChatCompletion(ctx,
				llm.WithMessages(
					llm.NewMessage(llm.RoleSystem, visionSystemPrompt),
					llm.NewMessageWithAttachments(llm.RoleUser, question, attachment),
				),
			)
			if err != nil {
				a.logger.WarnContext(ctx, "agent: appel du modèle de vision en échec",
					"plugin", a.spec.PluginName, "bytes", len(data), "error", err)
				return llm.NewToolResult("error: the image could not be looked at right now."), nil
			}

			text := ""
			if msg := resp.Message(); msg != nil {
				text = cleanReply(msg.Content())
			}
			if text == "" {
				return llm.NewToolResult("error: nothing could be read from this image."), nil
			}

			return llm.NewToolResult(text), nil
		},
	).WithReadOnlyHint(true)
}

// findAttachment retrouve une pièce jointe du tour par son nom. La
// comparaison insensible à la casse rattrape les modèles qui recopient
// approximativement le nom listé.
func findAttachment(attachments []media.Media, filename string) (media.Media, bool) {
	for _, m := range attachments {
		if m.Filename == filename {
			return m, true
		}
	}
	for _, m := range attachments {
		if strings.EqualFold(m.Filename, filename) {
			return m, true
		}
	}

	return media.Media{}, false
}

var _ delegation.Specialist = &PluginSubAgent{}
