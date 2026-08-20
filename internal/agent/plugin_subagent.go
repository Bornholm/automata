package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

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
	CallPluginTool(ctx context.Context, pluginName, toolName string, callCtx PluginCallContext, argsJSON string) (result string, isError bool, err error)
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
	Tools            []PluginToolSpec
}

// PluginToolSpec décrit un outil du plugin. ReadOnly faux = écriture :
// l'outil n'est jamais exécuté pendant le tour, seulement proposé à la
// confirmation humaine — même position prudente qu'isWriteTool.
type PluginToolSpec struct {
	Name        string
	Description string
	SchemaJSON  string
	ReadOnly    bool
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
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	messages := buildChatMessages(a.spec.SystemPrompt, agentName, false, "", Request{
		Identity:    req.Identity,
		Input:       buildDelegationInput(req),
		Attachments: req.Attachments,
		// History volontairement omis : garantie d'isolation des délégués.
	})

	maxIterations := a.spec.MaxToolCalls
	if maxIterations <= 0 {
		maxIterations = 5
	}

	mediaCollector := newMediaCollector()

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

			result, isError, err := a.caller.CallPluginTool(ctx, a.spec.PluginName, spec.Name, callCtx, string(argsJSON))
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

var _ delegation.Specialist = &PluginSubAgent{}
