package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
)

// ErrMaxDelegationsReached est retournée par OrchestratorAgent.Execute
// lorsque le plafond d'itérations de la boucle d'appel d'outils
// (agentCfg.Limits.MaxSequentialToolCalls) est atteint sans que le modèle
// n'ait produit de réponse finale sans tool-call.
//
// Décision de conception (PLAN.md Phase 8, "détecter les boucles et
// plafonner les délégations") : on n'invente jamais une réponse à partir
// d'un tour de boucle interrompu — le même principe que ErrEmptyReply
// (agent.go) s'applique ici. Une erreur explicite laisse l'appelant
// (internal/conversation.Handler) traiter l'échec comme n'importe quelle
// autre erreur d'agent, plutôt que de risquer d'envoyer à l'utilisateur une
// réponse tronquée qui semblerait délibérée.
var ErrMaxDelegationsReached = errors.New("agent: plafond de délégations atteint sans réponse finale du modèle")

// OrchestratorAgent est un agent généraliste capable de déléguer à des
// sous-agents spécialistes, exposés comme des outils LLM (PLAN.md §6.1,
// §6.4, Phase 8). Contrairement à GenAIAgent (streaming, sans outil), il
// utilise une complétion non-streaming : l'API GenAI expose les tool-calls
// demandés par le modèle directement sur ChatCompletionResponse.ToolCalls()
// (llm/chat_completion.go), ce qui rend la boucle "appeler -> exécuter les
// outils -> renvoyer les résultats -> rappeler" nettement plus simple qu'en
// reconstruisant des deltas de tool-calls depuis un flux de chunks
// (llm.ToolCallDelta existe côté streaming, mais reconstruire des appels
// d'outils complets à partir de deltas incrémentaux n'apporterait ici aucun
// bénéfice utilisateur, puisque les outils de délégation ne sont pas
// affichés en direct). Voir docs/integration-inventory.md §2 "Tool
// calling".
type OrchestratorAgent struct {
	client                 llm.ChatCompletionClient
	systemPrompt           string
	agentName              string
	orgDisplayName         string
	specialists            map[string]delegation.Specialist
	maxSequentialToolCalls int
	memoryTools            MemoryTools
	metrics                *observability.Metrics
}

// NewOrchestratorAgent construit un OrchestratorAgent. specialists associe
// chaque nom de délégué (tel que déclaré dans agentCfg.Delegates) au
// delegation.Specialist correspondant ; un outil "delegate_to_<nom>" est
// exposé au modèle pour chaque entrée. maxSequentialToolCalls plafonne le
// nombre d'itérations de la boucle d'appel d'outils (§6.4) ; une valeur <= 0
// retombe sur 1 (au moins un appel de complétion, jamais de boucle
// illimitée).
func NewOrchestratorAgent(client llm.ChatCompletionClient, systemPrompt, agentName, orgDisplayName string, specialists map[string]delegation.Specialist, maxSequentialToolCalls int) *OrchestratorAgent {
	return &OrchestratorAgent{
		client:                 client,
		systemPrompt:           systemPrompt,
		agentName:              agentName,
		orgDisplayName:         orgDisplayName,
		specialists:            specialists,
		maxSequentialToolCalls: maxSequentialToolCalls,
	}
}

// Execute implémente Agent. La boucle d'appel d'outils est strictement
// séquentielle (PLAN.md §6.4, "l'exécution initiale sera séquentielle") :
// lorsqu'un tour du modèle demande plusieurs tool-calls, ils sont exécutés
// un par un, dans l'ordre reçu, avant le tour suivant. La mécanique de
// boucle elle-même est factorisée dans runToolLoop (toolloop.go), partagée
// avec MCPToolAgent (Phase 12) : voir son commentaire de package.
func (a *OrchestratorAgent) Execute(ctx context.Context, req Request) (Result, error) {
	collector := newProposalCollector()

	tools := a.buildDelegationTools(req.Identity, collector)
	tools = append(tools, a.memoryTools.buildMemoryTools(req.Identity, collector)...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	messages := buildChatMessages(a.systemPrompt, a.agentName, a.orgDisplayName, req)

	maxIterations := a.maxSequentialToolCalls
	if maxIterations <= 0 {
		maxIterations = 1
	}

	loopResult, err := runToolLoop(ctx, a.client, messages, tools, maxIterations, ErrMaxDelegationsReached)
	if err != nil {
		return Result{}, err
	}

	return Result{Reply: loopResult.Text, ProposedActions: collector.take()}, nil
}

// buildDelegationTools construit un llm.Tool "delegate_to_<agentID>" par
// spécialiste connu. Reconstruit à chaque exécution (plutôt que mémorisé à
// la construction de l'OrchestratorAgent) car chaque outil capture
// l'identité d'exécution propre à la requête courante : l'identité n'est
// jamais décidée par le modèle (InvariantRules, règle 1), seulement
// transmise par l'application.
func (a *OrchestratorAgent) buildDelegationTools(identity model.ExecutionIdentity, collector *proposalCollector) []llm.Tool {
	tools := make([]llm.Tool, 0, len(a.specialists))

	for agentID, specialist := range a.specialists {
		tools = append(tools, newDelegationTool(agentID, specialist, identity, collector, a.metrics))
	}

	// Ordre déterministe : la map d'origine n'a pas d'ordre garanti, et un
	// ordre stable des outils envoyés au modèle facilite le débogage et les
	// tests.
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	return tools
}

// newDelegationTool construit l'outil LLM "delegate_to_<agentID>" qui
// adapte les arguments produits par le modèle en delegation.Request, puis
// exécute specialist.Execute. Un échec du spécialiste n'est jamais remonté
// comme erreur Go (ce qui ferait échouer tout le tour) : il est transmis au
// modèle comme contenu de résultat d'outil, en clair, pour qu'il puisse
// s'adapter (PLAN.md Phase 8, test "spécialiste en erreur").
func newDelegationTool(agentID string, specialist delegation.Specialist, identity model.ExecutionIdentity, collector *proposalCollector, metrics *observability.Metrics) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("goal", "Objectif précis à atteindre par le spécialiste.", "string").
		Property("relevant_input", "Éléments de contexte explicitement nécessaires à la tâche, formulés en clair. Ne jamais transmettre l'historique complet de la conversation.", "string").
		Property("constraints", "Contraintes additionnelles à respecter par le spécialiste.", "array", map[string]any{"type": "string"}, "items")

	return llm.NewFuncTool(
		"delegate_to_"+agentID,
		fmt.Sprintf("Délègue une tâche au spécialiste %q et retourne un résumé de son résultat.", agentID),
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			goal, _ := params["goal"].(string)
			if strings.TrimSpace(goal) == "" {
				return llm.NewToolResult(fmt.Sprintf("erreur: le paramètre 'goal' est requis et ne peut pas être vide pour déléguer à %q.", agentID)), nil
			}

			relevantInput, _ := params["relevant_input"].(string)

			var constraints []string
			if raw, ok := params["constraints"].([]any); ok {
				for _, c := range raw {
					if s, ok := c.(string); ok {
						constraints = append(constraints, s)
					}
				}
			}

			metrics.IncDelegation(agentID)

			result, err := specialist.Execute(ctx, delegation.Request{
				AgentID:       agentID,
				Goal:          goal,
				RelevantInput: relevantInput,
				Constraints:   constraints,
				Identity:      identity,
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("le spécialiste %q a échoué: %v", agentID, err)), nil
			}

			for _, pa := range result.ProposedActions {
				collector.add(pa)
			}

			return llm.NewToolResult(result.Summary), nil
		},
	)
}

// WithMemoryTools attache tools à a : les outils search_memory/remember/
// forget_memory correspondants (selon tools.Search/Remember/Forget) sont
// exposés au modèle en plus des délégations, dès le prochain Execute
// (PLAN.md §6.1, §8, Phase 10). Retourne a pour permettre le chaînage à la
// construction (voir internal/agent.NewRegistryWithMemory). Un appel avec la
// valeur zéro de MemoryTools désactive tous les outils mémoire, ce qui est
// le comportement par défaut d'un OrchestratorAgent tout juste construit.
func (a *OrchestratorAgent) WithMemoryTools(tools MemoryTools) *OrchestratorAgent {
	a.memoryTools = tools
	return a
}

// WithMetrics attache metrics à a : les délégations vers chaque spécialiste
// (outil "delegate_to_<agentID>") sont comptabilisées dès le prochain
// Execute (PLAN.md §14.3, Phase 20). metrics nil désactive l'observation
// (comportement par défaut d'un OrchestratorAgent tout juste construit).
// Retourne a pour permettre le chaînage à la construction, comme
// WithMemoryTools.
func (a *OrchestratorAgent) WithMetrics(metrics *observability.Metrics) *OrchestratorAgent {
	a.metrics = metrics
	return a
}

var _ Agent = &OrchestratorAgent{}
