package agent

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/resource"
)

// calendarIDParam est le nom de champ conventionnel utilisé par le serveur
// MCP google-calendar (réel ou fake, voir agenda_test.go) pour désigner
// l'identifiant de calendrier ciblé par un appel d'outil. Aucun véritable
// serveur MCP Google Calendar n'est disponible dans ce dépôt (voir
// AGENTS.md, PLAN.md Phase 13) : cette convention de nommage est celle
// choisie pour le serveur fake construit pour les tests, faute de référence
// externe. Quelle que soit la valeur fournie par le modèle sous ce nom,
// l'application l'écrase systématiquement (PLAN.md §9.2).
const calendarIDParam = resource.CalendarIDParam

// calendarReadPrefixes / calendarWritePrefixes classent les outils du
// serveur MCP google-calendar par convention de nommage plutôt que par une
// annotation protocolaire.
//
// Décision documentée (PLAN.md Phase 13) : le SDK MCP
// (modelcontextprotocol/go-sdk, via genai/mcp/common.Client) expose bien des
// annotations d'outil readOnlyHint côté protocole (goMCP.Tool.Annotations)
// et genai/mcp/common.Client les consulte pour un filtre optionnel côté
// connexion (common.WithReadOnlyHint, qui exclut purement et simplement les
// outils non readOnlyHint de GetTools). Mais cette information n'est PAS
// transportée jusqu'à llm.Tool pour les outils conservés :
// common.Client.toTool (github.com/bornholm/genai, mcp/common/client.go) ne
// transporte que le nom, la description, le schéma et une fonction
// d'exécution — jamais l'annotation elle-même. Une fois les outils obtenus
// via mcp.Manager.GetTools (internal/mcp/manager.go), aucune classification
// lecture/écriture native n'est donc disponible côté automata.
//
// La classification ci-dessous est donc un choix explicite par convention
// de nommage, assumé et documenté ici plutôt que dissimulé : les préfixes
// "list_"/"get_"/"search_"/"find_" sont des lectures exécutées directement
// (PLAN.md §10.1, "les recherches et lectures peuvent être exécutées
// directement") ; tout le reste, y compris un préfixe inconnu, est traité
// en écriture nécessitant confirmation — position prudente par défaut,
// jamais l'inverse.
var calendarReadPrefixes = []string{"list_", "get_", "search_", "find_"}

// isCalendarWriteTool implémente la classification documentée ci-dessus.
func isCalendarWriteTool(name string) bool {
	for _, prefix := range calendarReadPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// calendarDateLikeParams énumère les noms de paramètres traités comme des
// dates/heures devant respecter RFC3339 avec fuseau horaire explicite
// (PLAN.md Phase 13, "gestion des dates"). Convention pragmatique choisie
// en l'absence de schéma de serveur MCP Google Calendar réel : les noms
// usuels d'un outil de création/modification d'événement.
var calendarDateLikeParams = map[string]bool{
	"start":      true,
	"end":        true,
	"start_time": true,
	"end_time":   true,
	"starts_at":  true,
	"ends_at":    true,
	"start_date": true,
	"end_date":   true,
	"date":       true,
	"datetime":   true,
}

// validateCalendarDates refuse args si l'un des paramètres reconnus comme
// une date (calendarDateLikeParams) n'est pas une chaîne au format
// RFC3339/ISO8601 avec fuseau horaire explicite. Aucune tentative n'est
// faite pour deviner une date ambiguë ou sans fuseau (PLAN.md Phase 13,
// "jamais de tentative de deviner une date") : une erreur claire est
// retournée à la place.
//
// La détection de conflits d'agenda (chevauchement d'événements) n'est
// délibérément PAS implémentée ici : aucun calendrier réel n'est
// disponible dans ce dépôt pour la valider, ce serait spéculatif. Le
// system_prompt configuré pour l'agent agenda DOIT demander explicitement
// au modèle de consulter list_events avant de proposer une création,
// lorsque cet outil est disponible (voir le message de proposition retourné
// par wrapCalendarWriteTool, qui le rappelle également au modèle).
func validateCalendarDates(args map[string]any) error {
	for key, raw := range args {
		if !calendarDateLikeParams[strings.ToLower(key)] {
			continue
		}

		value, ok := raw.(string)
		if !ok {
			return fmt.Errorf("le paramètre %q doit être une chaîne de date ISO8601/RFC3339 avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00)", key)
		}

		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("le paramètre %q (%q) n'est pas une date ISO8601/RFC3339 valide avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00): %w", key, value, err)
		}
	}

	return nil
}

// wrapCalendarReadTool enveloppe un outil de lecture (list_events,
// get_event, ...) pour réécrire systématiquement calendar_id vers
// calendarID, quelle que soit la valeur fournie par le modèle (PLAN.md
// §9.2). Aucune confirmation n'est requise (PLAN.md §10.1).
func wrapCalendarReadTool(tool llm.Tool, calendarID string) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params)+1)
		maps.Copy(args, params)
		args[calendarIDParam] = calendarID

		return tool.Execute(ctx, args)
	}

	return llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)
}

// wrapCalendarWriteTool enveloppe un outil d'écriture (create_event,
// update_event, delete_event, ...) de sorte qu'il n'exécute JAMAIS l'appel
// MCP réel : il enregistre une action à confirmer dans collector, qui
// remonte à l'orchestrateur puis devient un plan persisté
// (internal/action.Engine), confirmable par les commandes conversationnelles
// "confirmer"/"annuler" (PLAN.md §10, Phase 15).
//
// calendar_id est retiré des arguments proposés plutôt qu'y être figé : la
// ressource est résolue à nouveau au moment de la confirmation, à partir de
// la portée du plan (PLAN.md §10.5 point 6). Une valeur fournie par le
// modèle sous ce nom est donc systématiquement écartée (§9.2), et un plan
// confirmé ne peut pas écrire dans un calendrier qui aurait cessé d'être
// celui de sa portée entre-temps.
func wrapCalendarWriteTool(tool llm.Tool, collector *proposalCollector, agentName string, scope model.Scope, scopeID model.ScopeID) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params))
		for k, v := range params {
			// Jamais l'identifiant fourni par le modèle : voir ci-dessus.
			if k == calendarIDParam {
				continue
			}
			args[k] = v
		}

		if err := validateCalendarDates(args); err != nil {
			return llm.NewToolResult(fmt.Sprintf(
				"erreur: %v. Fournis une date précise au format ISO8601/RFC3339 avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00) ; aucune action n'a été proposée.",
				err,
			)), nil
		}

		collector.add(delegation.ProposedAction{
			Summary:            summarizeCalendarAction(tool.Name(), args),
			AgentID:            agentName,
			MCPServer:          calendarMCPServerName,
			ToolName:           tool.Name(),
			Arguments:          args,
			RequiredPermission: fmt.Sprintf("calendar.%s.write", scope),
			Scope:              scope,
			ScopeID:            scopeID,
		})

		return llm.NewToolResult(
			"Action enregistrée, en attente de la confirmation de l'utilisateur : elle n'a PAS encore été exécutée. " +
				"Ne la propose pas une seconde fois et n'essaie pas de la confirmer toi-même. " +
				"Si un outil de lecture des événements existants est disponible, consulte-le pour signaler un éventuel chevauchement.",
		), nil
	}

	return llm.NewFuncTool(
		tool.Name(),
		tool.Description()+" (l'action est soumise à la confirmation de l'utilisateur avant toute exécution réelle)",
		tool.Parameters(),
		execute,
	)
}

// summarizeCalendarAction produit la description humaine affichée dans la
// liste des actions à confirmer. Elle reprend les champs usuels d'un
// événement lorsqu'ils sont présents, et se rabat sur le nom de l'outil
// sinon : c'est ce texte que l'utilisateur lit avant de confirmer, il ne doit
// jamais se réduire à un identifiant technique.
func summarizeCalendarAction(toolName string, args map[string]any) string {
	title := stringParam(args, "title")
	if title == "" {
		title = stringParam(args, "summary")
	}

	start := stringParam(args, "start")
	if start == "" {
		start = stringParam(args, "start_time")
	}

	switch {
	case title != "" && start != "":
		return fmt.Sprintf("%s : %s (%s)", toolName, title, start)
	case title != "":
		return fmt.Sprintf("%s : %s", toolName, title)
	default:
		return fmt.Sprintf("%s sur l'agenda de la portée courante", toolName)
	}
}

// newCalendarToolsRewriter construit un ToolsRewriterFunc (voir
// mcp_tool_agent.go) qui : résout calendarID pour la portée de la
// conversation courante (jamais une portée fournie par le modèle, PLAN.md
// §9.2) via resource.ResolveCalendarID, puis enveloppe chaque outil du
// serveur MCP google-calendar selon sa classification lecture/écriture
// (isCalendarWriteTool).
//
// Si aucune ressource "calendar" n'est configurée pour la portée courante,
// la résolution échoue et le rewriter retourne une erreur avant tout appel
// au modèle — même principe que l'échec de connexion à un serveur MCP
// (MCPToolAgent.Execute, Phase 12) : ne jamais deviner une ressource
// absente de la configuration.
//
// La résolution a lieu même lorsque seuls des outils d'écriture seront
// utilisés, bien que ceux-ci ne figent plus l'identifiant : c'est ce qui
// garantit qu'une portée sans calendrier configuré est refusée d'emblée,
// plutôt qu'au moment de la confirmation d'un plan déjà annoncé à
// l'utilisateur.
func newCalendarToolsRewriter(cfg *config.Config, agentName string) ToolsRewriterFunc {
	return func(ctx context.Context, req Request, tools []llm.Tool, collector *proposalCollector) ([]llm.Tool, error) {
		calendarID, err := resource.ResolveCalendarID(cfg, req.Conversation.Scope, req.Conversation.ScopeID)
		if err != nil {
			return nil, err
		}

		wrapped := make([]llm.Tool, len(tools))
		for i, tool := range tools {
			if isCalendarWriteTool(tool.Name()) {
				wrapped[i] = wrapCalendarWriteTool(tool, collector, agentName, req.Conversation.Scope, req.Conversation.ScopeID)
			} else {
				wrapped[i] = wrapCalendarReadTool(tool, calendarID)
			}
		}

		return wrapped, nil
	}
}

// NewAgendaToolAgent construit le spécialiste agenda : un MCPToolAgent
// (mcp_tool_agent.go) enrichi de la résolution de ressource et de la
// transformation des écritures en actions à confirmer décrites ci-dessus
// (PLAN.md §9.2, §10.1, Phase 13), plutôt qu'un nouveau type dupliquant
// runToolLoop/la récupération des outils mcp — voir ToolsRewriterFunc.
func NewAgendaToolAgent(
	client llm.ChatCompletionClient,
	systemPrompt, agentName, orgDisplayName string,
	mcpManager *mcp.Manager,
	mcpServerNames []string,
	mcpLimits mcp.Limits,
	maxSequentialToolCalls int,
	cfg *config.Config,
) *MCPToolAgent {
	base := NewMCPToolAgent(client, systemPrompt, agentName, orgDisplayName, mcpManager, mcpServerNames, mcpLimits, maxSequentialToolCalls)

	return base.WithToolsRewriter(newCalendarToolsRewriter(cfg, agentName))
}
