package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/resource"
)

// todoListIDParam est le nom de champ conventionnel utilisé par le serveur
// MCP "todo" (réel ou fake, voir todo_test.go) pour désigner l'identifiant
// de liste de tâches ciblé par un appel d'outil. Aucun véritable serveur MCP
// de gestion de tâches n'est disponible dans ce dépôt (comme pour
// google-calendar en Phase 13) : cette convention est celle choisie pour le
// serveur fake construit pour les tests, faute de référence externe. Quelle
// que soit la valeur fournie par le modèle sous ce nom, l'application
// l'écrase systématiquement (PLAN.md §9.2).
const todoListIDParam = resource.TodoListIDParam

// todoReadPrefixes / isTodoWriteTool : même classification par préfixe de
// nom que l'agenda (agenda.go, calendarReadPrefixes) — voir le commentaire
// détaillé associé, qui documente pourquoi aucune classification native
// (readOnlyHint MCP) n'est disponible côté automata pour l'instant.
var todoReadPrefixes = []string{"list_", "get_", "search_", "find_"}

func isTodoWriteTool(name string) bool {
	for _, prefix := range todoReadPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// todoDedupeKey calcule une clé de déduplication triviale pour une action de
// création de tâche : ses arguments sérialisés en JSON (encoding/json trie
// les clés d'une map), puis normalisés par un simple TrimSpace + passage en
// minuscules. L'identifiant de liste n'en fait pas partie, puisqu'il est
// résolu plus tard, à la confirmation — toutes les actions d'un même tour
// partagent de toute façon la même portée.
//
// Aucune similarité floue n'est tentée : deux créations dont le contenu
// diffère ne serait-ce que par la ponctuation ne sont pas détectées comme
// doublons, ce serait spéculatif.
func todoDedupeKey(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(raw)))
}

// wrapTodoReadTool enveloppe un outil de lecture (list_tasks, get_task,
// search_tasks, ...) pour réécrire systématiquement list_id vers listID,
// quelle que soit la valeur fournie par le modèle (PLAN.md §9.2). Aucune
// confirmation n'est requise (PLAN.md §10.1).
func wrapTodoReadTool(tool llm.Tool, listID string) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params)+1)
		maps.Copy(args, params)
		args[todoListIDParam] = listID

		return tool.Execute(ctx, args)
	}

	return llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)
}

// wrapTodoWriteTool enveloppe un outil d'écriture (create_task, update_task,
// delete_task, ...) de sorte qu'il n'exécute JAMAIS l'appel MCP réel : il
// enregistre une action à confirmer dans collector, qui remonte à
// l'orchestrateur puis devient un plan persisté (internal/action.Engine).
//
// Toutes les actions proposées durant le tour rejoignent le même plan et sont
// donc confirmées d'un seul coup, puis exécutées séquentiellement avec un
// rapport distinguant chaque succès et chaque échec : c'est le moteur commun
// qui fournit désormais la confirmation groupée exigée par PLAN.md §6.4, là
// où la Phase 14 s'appuyait sur un outil confirm_todo_actions dédié.
//
// list_id est retiré des arguments proposés plutôt qu'y être figé : la
// ressource est résolue à nouveau au moment de la confirmation, à partir de
// la portée du plan (PLAN.md §10.5 point 6). Une valeur fournie par le modèle
// sous ce nom est donc systématiquement écartée (§9.2).
//
// dedupe mémorise les créations déjà proposées durant ce tour : deux appels
// de contenu identique ne produisent qu'une action (PLAN.md Phase 14,
// "déduplication").
func wrapTodoWriteTool(tool llm.Tool, collector *proposalCollector, dedupe *todoDedupeSet, agentName string, scope model.Scope, scopeID model.ScopeID) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params))
		for k, v := range params {
			// Jamais l'identifiant fourni par le modèle : voir ci-dessus.
			if k == todoListIDParam {
				continue
			}
			args[k] = v
		}

		// Seules les créations sont dédupliquées : deux mises à jour ou
		// suppressions successivement identiques peuvent être légitimes.
		if strings.HasPrefix(tool.Name(), "create_") {
			if !dedupe.add(tool.Name() + "\x00" + todoDedupeKey(args)) {
				return llm.NewToolResult(
					"Une action identique a déjà été enregistrée pour ce tour : aucun doublon n'a été ajouté.",
				), nil
			}
		}

		collector.add(delegation.ProposedAction{
			Summary:            summarizeTodoAction(tool.Name(), args),
			AgentID:            agentName,
			MCPServer:          todoMCPServerName,
			ToolName:           tool.Name(),
			Arguments:          args,
			RequiredPermission: fmt.Sprintf("todo.%s.write", scope),
			Scope:              scope,
			ScopeID:            scopeID,
		})

		return llm.NewToolResult(
			"Action enregistrée, en attente de la confirmation de l'utilisateur : elle n'a PAS encore été exécutée. " +
				"Tu peux en enregistrer d'autres dans le même tour, elles seront confirmées ensemble. " +
				"N'essaie pas de les confirmer toi-même.",
		), nil
	}

	return llm.NewFuncTool(
		tool.Name(),
		tool.Description()+" (l'action est soumise à la confirmation de l'utilisateur avant toute exécution réelle)",
		tool.Parameters(),
		execute,
	)
}

// todoDedupeSet mémorise les clés de déduplication des créations déjà
// proposées durant un tour. Protégé par un mutex pour la même raison que
// proposalCollector : ne pas dépendre d'une hypothèse implicite sur
// l'ordonnancement des exécutions d'outils.
type todoDedupeSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newTodoDedupeSet() *todoDedupeSet {
	return &todoDedupeSet{seen: make(map[string]bool)}
}

// add enregistre key et retourne false si elle était déjà présente.
func (s *todoDedupeSet) add(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.seen[key] {
		return false
	}
	s.seen[key] = true

	return true
}

// summarizeTodoAction produit la description humaine affichée dans la liste
// des actions à confirmer : c'est ce texte que l'utilisateur lit avant de
// confirmer, il ne doit jamais se réduire à un identifiant technique.
func summarizeTodoAction(toolName string, args map[string]any) string {
	title := stringParam(args, "title")
	if title == "" {
		title = stringParam(args, "name")
	}
	if title == "" {
		title = stringParam(args, "task")
	}

	if title != "" {
		return fmt.Sprintf("%s : %s", toolName, title)
	}

	return fmt.Sprintf("%s sur la liste de tâches de la portée courante", toolName)
}

// newTodoToolsRewriter construit un ToolsRewriterFunc (voir
// mcp_tool_agent.go) qui : résout listID pour la portée de la conversation
// courante (jamais une portée fournie par le modèle, PLAN.md §9.2) via
// resource.ResolveTodoListID, puis enveloppe chaque outil du serveur MCP todo
// selon sa classification lecture/écriture (isTodoWriteTool).
//
// Si aucune ressource "todo" n'est configurée pour la portée courante, la
// résolution échoue et le rewriter retourne une erreur avant tout appel au
// modèle — même principe que l'agenda (agenda.go, newCalendarToolsRewriter)
// et que l'échec de connexion à un serveur MCP (MCPToolAgent.Execute, Phase
// 12) : ne jamais deviner une ressource absente de la configuration.
func newTodoToolsRewriter(cfg *config.Config, agentName string) ToolsRewriterFunc {
	return func(ctx context.Context, req Request, tools []llm.Tool, collector *proposalCollector) ([]llm.Tool, error) {
		listID, err := resource.ResolveTodoListID(cfg, req.Conversation.Scope, req.Conversation.ScopeID)
		if err != nil {
			return nil, err
		}

		// Portée du tour : deux exécutions successives de l'agent peuvent
		// légitimement proposer la même création, seuls les doublons d'un
		// même tour sont écartés.
		dedupe := newTodoDedupeSet()

		wrapped := make([]llm.Tool, 0, len(tools))

		for _, tool := range tools {
			if isTodoWriteTool(tool.Name()) {
				wrapped = append(wrapped, wrapTodoWriteTool(tool, collector, dedupe, agentName, req.Conversation.Scope, req.Conversation.ScopeID))
			} else {
				wrapped = append(wrapped, wrapTodoReadTool(tool, listID))
			}
		}

		return wrapped, nil
	}
}

// NewTodoToolAgent construit le spécialiste todo : un MCPToolAgent
// (mcp_tool_agent.go) enrichi de la résolution de ressource et de la
// transformation des écritures en actions à confirmer décrites ci-dessus
// (PLAN.md §9.2, §10.1, §6.4, Phase 14), même schéma de composition que
// NewAgendaToolAgent (agenda.go).
func NewTodoToolAgent(
	client llm.ChatCompletionClient,
	systemPrompt, agentName, orgDisplayName string,
	mcpManager *mcp.Manager,
	mcpServerNames []string,
	mcpLimits mcp.Limits,
	maxSequentialToolCalls int,
	cfg *config.Config,
) *MCPToolAgent {
	base := NewMCPToolAgent(client, systemPrompt, agentName, orgDisplayName, mcpManager, mcpServerNames, mcpLimits, maxSequentialToolCalls)

	return base.WithToolsRewriter(newTodoToolsRewriter(cfg, agentName))
}
