package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

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

// subAgentBudget borne la durée de la BOUCLE d'une délégation à un plugin.
// La conclusion dispose ensuite de son propre budget (conclusionBudget),
// qui s'y ajoute.
//
// Le calcul : deux délégations au plus par tour
// (maxSameAgentDelegations), soit 2 × (90 s + 45 s) = 270 s, plus les tours
// de l'orchestrateur — le tout devant tenir dans les 5 minutes du pipeline
// d'ingress.
const subAgentBudget = 90 * time.Second

// PluginSubAgent exécute une délégation avec les outils d'un plugin.
// Implémente delegation.Specialist.
type PluginSubAgent struct {
	spec         PluginSubAgentSpec
	client       llm.ChatCompletionClient
	caller       PluginToolCaller
	files        PluginFileTransfer
	maxFileBytes int64
	vision       llm.ChatCompletionClient
	// textOnly indique que le client du sous-agent refuse les images en
	// entrée. Il ne reçoit alors AUCUNE pièce jointe : un fournisseur
	// texte-seul rejette la requête entière — pas seulement l'image — avec
	// un « no endpoints found that support image input ».
	textOnly            bool
	maxToolContextBytes int64
	// skills fournit le catalogue des compétences et l'outil load_skill.
	// Le nom d'agent vu par la bibliothèque est celui du plugin : une
	// compétence ciblée `agents: [workspace]` n'apparaît que là.
	skills SkillsProvider
	logger *slog.Logger
	// binding et visionBinding permettent de servir un modèle différent
	// selon l'organisation, pour le sous-agent lui-même et pour le regard
	// que porte view_file sur les images.
	binding       clientBinding
	visionBinding clientBinding
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

// WithTextOnly déclare que le client du sous-agent n'accepte pas les
// images. Les pièces jointes du tour lui sont alors décrites en texte au
// lieu de lui être transmises : c'est sans perte pour un sous-agent qui
// travaille sur des fichiers par leur chemin, et cela évite de faire
// échouer tout le tour.
func (a *PluginSubAgent) WithTextOnly(textOnly bool) *PluginSubAgent {
	a.textOnly = textOnly
	return a
}

// WithClientResolver permet de servir à ce sous-agent, et à son regard sur
// les images, des modèles différents selon l'organisation. Les rôles sont
// llmclients.RolePlugins et llmclients.RolePluginsVision. Retourne a pour
// permettre le chaînage.
func (a *PluginSubAgent) WithClientResolver(resolver ClientResolver, role, visionRole string, logger *slog.Logger) *PluginSubAgent {
	a.binding.bind(resolver, role, logger)
	a.visionBinding.bind(resolver, visionRole, logger)
	return a
}

// WithVisionClient branche le client multimodal qui sert l'outil view_file.
// Le nom se distingue de WithVision(bool) des autres agents, qui déclare
// tout autre chose : la capacité du client PRINCIPAL à recevoir des images.
// Le
// modèle du sous-agent est texte-seul : sans ce client, un agent qui
// manipule des images travaille en aveugle et gaspille son budget d'outils
// à sonder des histogrammes. Sans cet appel, view_file n'est pas monté.
func (a *PluginSubAgent) WithVisionClient(client llm.ChatCompletionClient) *PluginSubAgent {
	a.vision = client
	return a
}

// WithSkills branche la bibliothèque de compétences. Nil-safe : sans
// provider, ni catalogue ni load_skill ne sont exposés.
func (a *PluginSubAgent) WithSkills(provider SkillsProvider) *PluginSubAgent {
	a.skills = provider
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

	// Modèles du tour : ceux que l'organisation a choisis si elle en a
	// choisi, ceux de la configuration sinon. Le client de vision est résolu
	// séparément — il a son propre rôle au catalogue, et rien n'impose que
	// les deux organisations le règlent de la même façon.
	client, modelTextOnly := a.client, a.textOnly
	if resolved, ok := a.binding.resolve(ctx, req.Identity.OrgID); ok {
		client, modelTextOnly = resolved.Client, !resolved.SupportsVision
	}

	visionClient := a.vision
	if resolved, ok := a.visionBinding.resolve(ctx, req.Identity.OrgID); ok {
		visionClient = resolved.Client
	}

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
		if visionClient != nil {
			tools = append(tools, a.newViewFileTool(req, visionClient))
		}
	}

	// Le protocole de confirmation est dicté par l'HÔTE, jamais laissé au
	// prompt d'un plugin : un plugin peut l'oublier, et un modèle qui ne
	// le lit nulle part invente le sien. Vu en production : le sous-agent
	// a répondu « réponds "oui, supprime" et c'est exécuté » sans jamais
	// appeler delete_space, puis a affirmé la suppression faite.
	basePrompt := a.spec.SystemPrompt
	if a.hasWriteTools() {
		basePrompt += "\n\n" + confirmationProtocolPrompt
	}

	// Le catalogue et load_skill se montent INCONDITIONNELLEMENT dès que
	// la bibliothèque a quelque chose pour ce plugin — contrairement aux
	// outils fichiers ci-dessus, qui dépendent de ce que l'hôte sait
	// servir. Charger une compétence ne coûte ni appel LLM ni réseau.
	systemPrompt, tools := appendSkills(ctx, a.skills, agentName, basePrompt, tools)

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	// Un sous-agent à outils fichiers ne reçoit AUCUNE pièce jointe dans
	// son contexte : il en reçoit la liste nommée et va chercher les octets
	// lui-même (import_attachment, view_file). Ce n'est pas une précaution
	// de coût mais de justesse — le client des plugins peut être un modèle
	// texte-seul, qui rejette alors la requête ENTIÈRE (« no endpoints
	// found that support image input »), et une photo jointe ferait échouer
	// un tour qui n'avait besoin que de son nom.
	//
	// Les fichiers sont annoncés dans l'entrée plutôt qu'en pièces jointes :
	// buildChatMessages décrirait les fichiers antérieurs comme « joints à
	// ce message », ce qu'ils ne sont pas.
	input := buildDelegationInput(req)
	attachmentsForModel := req.Attachments
	textOnly := modelTextOnly

	if a.SupportsFiles() {
		attachmentsForModel = nil
		textOnly = true
		input += media.AttachedFilesNotice(req.Attachments) + media.EarlierFilesNotice(req.RecentAttachments)
	}

	messages := buildChatMessages(systemPrompt, agentName, textOnly, "", Request{
		Identity:    req.Identity,
		Input:       input,
		Attachments: attachmentsForModel,
		// History volontairement omis : garantie d'isolation des délégués.
	})

	maxIterations := a.spec.MaxToolCalls
	if maxIterations <= 0 {
		maxIterations = 5
	}

	// Budget de temps du sous-agent. Le plafond d'appels ne suffit pas :
	// regarder une image coûte plusieurs secondes, et 25 appels tiennent
	// largement plus que le délai du pipeline. Sans cette borne, un tour
	// laborieux consomme tout le budget de la conversation et l'utilisateur
	// n'obtient qu'un échec après plusieurs minutes d'attente.
	//
	// La deadline n'est posée que si elle resserre celle de l'appelant.
	loopCtx := withMediaCollector(ctx, mediaCollector)
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > subAgentBudget {
		var cancel context.CancelFunc
		loopCtx, cancel = context.WithTimeout(loopCtx, subAgentBudget)
		defer cancel()
	}

	loopResult, err := runToolLoop(loopCtx, client, messages, tools, maxIterations, a.maxToolContextBytes, ErrMaxToolCallsReached, a.logger, "plugin:"+agentName)
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

		// addIfNew : les modèles rappellent volontiers un outil dont le
		// résultat dit « en attente de confirmation » — observé en
		// production avec publish_space, proposé trois fois dans le même
		// tour. Le doublon n'entre pas dans le plan, et le résultat le dit
		// sans ambiguïté.
		added := collector.addIfNew(delegation.ProposedAction{
			Summary:            summarizeAction(spec.Name, args),
			AgentID:            "plugin:" + a.spec.PluginName,
			MCPServer:          "plugin:" + a.spec.PluginName,
			ToolName:           spec.Name,
			Arguments:          args,
			RequiredPermission: fmt.Sprintf("%s.%s.write", a.spec.PermissionDomain, identity.Scope),
			Scope:              identity.Scope,
			ScopeID:            identity.ScopeID,
		})
		if !added {
			return llm.NewToolResult(
				"This exact action is ALREADY recorded and awaiting the user's confirmation. " +
					"Calling the tool again does nothing and will not speed it up. " +
					"Finish your turn now and tell the user the action awaits their confirmation.",
			), nil
		}

		return llm.NewToolResult(
			"Action recorded, awaiting the user's confirmation: it has NOT been executed yet, and you cannot execute or confirm it yourself. " +
				"Do not call this tool again for the same action. " +
				"You may record other, different actions in the same turn; they will be confirmed together. " +
				"When done, tell the user what awaits their confirmation.",
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
				// Nommer les fichiers réellement disponibles coupe court
				// aux devinettes : observé en production, douze noms
				// essayés avant le bon.
				names := attachmentNames(attachments)
				if len(names) == 0 {
					return llm.NewToolResult(
						"error: no file named " + filename + " was found, and NO file is attached to this conversation's recent messages. " +
							"Do not guess other names. Check list_files: it may already be in your workspace. " +
							"Otherwise ask the user to send it again.",
					), nil
				}
				return llm.NewToolResult(
					"error: no file named " + filename + " was found in this conversation. " +
						"The files available for import are, by exact name: " + strings.Join(names, ", ") + ". " +
						"Use one of these names verbatim — do not guess variations. " +
						"If none is the right file, check list_files (it may already be in your workspace) or ask the user to send it again.",
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

	// La taille maximale est CHIFFRÉE au modèle, ici et dans l'échec :
	// « trop gros, réencode plus petit » ne dit pas quelle cible viser, et
	// un agent qui ne connaît pas la borne réessaie au hasard — ou renonce
	// en disant à l'utilisateur qu'il ne sait pas envoyer de fichier.
	limit := "a few megabytes"
	if a.maxFileBytes > 0 {
		limit = fmt.Sprintf("%d MB", a.maxFileBytes/(1024*1024))
	}

	return llm.NewFuncTool(
		"attach_file",
		"Attach a file from your workspace to the reply sent to the user. This is how you give a file back to them. "+
			"Maximum size: "+limit+" — re-encode or trim anything larger before attaching it. "+
			"Do NOT describe the file afterwards, just confirm briefly what you produced.",
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
					"error: the file could not be attached — it is missing, or larger than " + limit + ". " +
						"If it is too large, re-encode it below that size (lower the resolution or the bitrate, " +
						"or cut a shorter extract) and attach the result. Do not give up on sending it.",
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

// hasWriteTools indique si le plugin expose au moins un outil soumis à
// confirmation.
func (a *PluginSubAgent) hasWriteTools() bool {
	for _, spec := range a.spec.Tools {
		if !spec.ReadOnly {
			return true
		}
	}
	return false
}

// confirmationProtocolPrompt part vers le modèle : anglais uniquement.
// Cette consigne est ajoutée par l'HÔTE au prompt de tout plugin exposant
// des outils d'écriture — aucun plugin ne peut la retirer.
const confirmationProtocolPrompt = "CONFIRMATION PROTOCOL — this is enforced by the host and you cannot change it:\n" +
	"- To make ANY change, you must CALL the corresponding tool. Describing a change in text does nothing at all.\n" +
	"- Tools that modify or delete something are never executed during your turn: calling one records a proposal, " +
	"and the host itself asks the user to reply \"confirmer\". That message is sent by the host, not by you.\n" +
	"- NEVER invent your own confirmation phrase (\"reply yes\", \"say delete\", \"answer OK\"): such an answer executes NOTHING, " +
	"and the user is left believing something happened. Only the host's \"confirmer\" executes a recorded proposal.\n" +
	"- NEVER state or imply that a change has been made, deleted, published or saved. You cannot know that: " +
	"execution happens after your turn, once the user confirms. Say what you have PROPOSED and that it awaits their confirmation.\n" +
	"- If the user asks for a destructive change, call the tool anyway: the confirmation step is the host's job, not a reason to hesitate."

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
// vision est le client résolu pour ce tour, passé en paramètre plutôt que
// relu du champ : l'organisation peut avoir choisi un autre modèle.
func (a *PluginSubAgent) newViewFileTool(req delegation.Request, vision llm.ChatCompletionClient) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("path", "Path of the image in your workspace. Extract a frame with ffmpeg first to look at a video.", "string").
		RequiredProperty("question", "What you need to know about the image, e.g. \"where is the logo, and how big is it?\".", "string")

	callCtx := pluginCallContext(req.Identity)

	// Un service de vision en panne l'est pour tout le tour : le disjoncteur
	// du client reste ouvert. Sans le dire clairement, l'agent rappelle
	// l'outil à chaque étape et épuise son budget sans rien produire — c'est
	// ce qui s'est vu en production, six appels perdus d'affilée.
	var visionFailed atomic.Bool

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

			if visionFailed.Load() {
				return llm.NewToolResult(
					"error: looking at images is unavailable for this task and will not recover. " +
						"Do not call view_file again: carry on without it, and tell the user what you could not verify.",
				), nil
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

			resp, err := vision.ChatCompletion(ctx,
				llm.WithMessages(
					llm.NewMessage(llm.RoleSystem, visionSystemPrompt),
					llm.NewMessageWithAttachments(llm.RoleUser, question, attachment),
				),
			)
			if err != nil {
				a.logger.WarnContext(ctx, "agent: appel du modèle de vision en échec",
					"plugin", a.spec.PluginName, "bytes", len(data), "error", err)
				visionFailed.Store(true)

				return llm.NewToolResult(
					"error: looking at images is unavailable for this task and will not recover. " +
						"Do not call view_file again: carry on without it, and tell the user what you could not verify.",
				), nil
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
// attachmentNames retourne les noms de fichiers importables, dédoublonnés
// dans l'ordre d'arrivée (tour courant d'abord), bornés pour tenir dans un
// résultat d'outil. Les noms sont déjà exposés au modèle par les notices
// de pièces jointes : les répéter ici ne divulgue rien de neuf.
func attachmentNames(attachments []media.Media) []string {
	const maxNames = 15

	seen := map[string]struct{}{}
	names := make([]string, 0, min(len(attachments), maxNames))
	for _, m := range attachments {
		if m.Filename == "" {
			continue
		}
		if _, done := seen[m.Filename]; done {
			continue
		}
		seen[m.Filename] = struct{}{}
		names = append(names, m.Filename)
		if len(names) == maxNames {
			break
		}
	}
	return names
}

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
