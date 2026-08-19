package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
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
	orgPrompts             map[string]string
	textOnly               bool
	agentName              string
	specialists            map[string]delegation.Specialist
	specialistDescriptions map[string]string
	maxSequentialToolCalls int
	maxActionsPerTurn      int
	maxToolContextBytes    int64
	memoryTools            MemoryTools
	reminderTools          ReminderTools
	metrics                *observability.Metrics
	logger                 *slog.Logger
}

// NewOrchestratorAgent construit un OrchestratorAgent. specialists associe
// chaque nom de délégué (tel que déclaré dans agentCfg.Delegates) au
// delegation.Specialist correspondant ; un outil "delegate_to_<nom>" est
// exposé au modèle pour chaque entrée. maxSequentialToolCalls plafonne le
// nombre d'itérations de la boucle d'appel d'outils (§6.4) ; une valeur <= 0
// retombe sur 1 (au moins un appel de complétion, jamais de boucle
// illimitée).
func NewOrchestratorAgent(client llm.ChatCompletionClient, systemPrompt, agentName string, specialists map[string]delegation.Specialist, maxSequentialToolCalls int) *OrchestratorAgent {
	return &OrchestratorAgent{
		client:                 client,
		systemPrompt:           systemPrompt,
		agentName:              agentName,
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
	mediaCollector := newMediaCollector()

	tools := a.buildDelegationTools(req.Identity, req.Attachments, collector, mediaCollector)
	tools = append(tools, a.memoryTools.buildMemoryTools(req.Identity, collector)...)
	tools = append(tools, a.reminderTools.buildReminderTools(req.Identity)...)
	// L'introspection capture les outils du tour APRÈS leur assemblage :
	// son instantané reflète exactement ce que ce tour offre, permissions
	// comprises.
	tools = append(tools, newDescribeCapabilitiesTool(tools, a.specialists, a.specialistDescriptions, req.Identity))
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	messages := buildChatMessages(resolveSystemPrompt(a.systemPrompt, a.orgPrompts, req.Identity.OrgID), a.agentName, a.textOnly, req)

	maxIterations := a.maxSequentialToolCalls
	if maxIterations <= 0 {
		maxIterations = 1
	}

	loopResult, err := runToolLoop(ctx, a.client, messages, tools, maxIterations, a.maxToolContextBytes, ErrMaxDelegationsReached, a.logger, a.agentName)
	if err != nil {
		return Result{}, err
	}

	proposals := collector.take()

	// Plafond d'actions par tour (PLAN.md §9.4, agents.<nom>.limits.
	// max_actions_per_turn). Le dépassement rejette le lot ENTIER plutôt que
	// d'en conserver les N premières : ces actions sont sensibles et
	// destinées à une confirmation groupée, or n'en garder qu'un préfixe
	// arbitraire ferait confirmer à l'utilisateur autre chose que ce que
	// l'agent a annoncé. Le texte de réponse, lui, est conservé : il porte le
	// raisonnement qui aide à reformuler.
	if a.maxActionsPerTurn > 0 && len(proposals) > a.maxActionsPerTurn {
		notice := fmt.Sprintf(
			"\n\n⚠️ Ce tour a produit %d actions à confirmer, au-delà de la limite de %d configurée pour l'agent %q. Aucune action n'a été enregistrée : reformulez votre demande en la découpant en plusieurs étapes.",
			len(proposals), a.maxActionsPerTurn, a.agentName,
		)

		return Result{Reply: loopResult.Text + notice, Attachments: a.collectedMedia(loopResult, mediaCollector)}, nil
	}

	return Result{
		Reply:           loopResult.Text,
		ProposedActions: proposals,
		Attachments:     a.collectedMedia(loopResult, mediaCollector),
	}, nil
}

// collectedMedia agrège les médias produits durant le tour : ceux des outils
// appelés directement par l'orchestrateur (mémoire) et ceux remontés par les
// spécialistes délégués.
func (a *OrchestratorAgent) collectedMedia(loopResult toolLoopResult, mediaCollector *mediaCollector) []media.Media {
	return append(append([]media.Media(nil), loopResult.Attachments...), mediaCollector.take()...)
}

// WithVision déclare si le modèle du client accepte les images en entrée.
// À false, aucune pièce jointe ne part vers le modèle — les délégations,
// elles, continuent de les transporter. Retourne a pour permettre le
// chaînage.
func (a *OrchestratorAgent) WithVision(enabled bool) *OrchestratorAgent {
	a.textOnly = !enabled
	return a
}

// WithOrgSystemPrompts remplace le prompt système par organisation : la clé
// est un organizations[].id, la valeur un prompt complet déjà composé (voir
// BuildOrgSystemPrompts). Le prompt du constructeur reste le défaut pour
// toute organisation absente de la map. Retourne a pour permettre le
// chaînage.
func (a *OrchestratorAgent) WithOrgSystemPrompts(prompts map[string]string) *OrchestratorAgent {
	a.orgPrompts = prompts
	return a
}

// WithMaxActionsPerTurn plafonne le nombre d'actions que ce tour peut
// proposer à la confirmation (PLAN.md §9.4). Une valeur <= 0 (défaut) laisse
// le nombre d'actions non borné. Retourne a pour permettre le chaînage.
func (a *OrchestratorAgent) WithMaxActionsPerTurn(max int) *OrchestratorAgent {
	a.maxActionsPerTurn = max
	return a
}

// WithMaxToolContextBytes borne le cumul des résultats d'outils réinjectés
// dans la conversation durant un tour (PLAN.md §9.4). Une valeur <= 0
// (défaut) laisse ce budget illimité. Retourne a pour permettre le chaînage.
func (a *OrchestratorAgent) WithMaxToolContextBytes(max int64) *OrchestratorAgent {
	a.maxToolContextBytes = max
	return a
}

// buildDelegationTools construit un llm.Tool "delegate_to_<agentID>" par
// spécialiste connu. Reconstruit à chaque exécution (plutôt que mémorisé à
// la construction de l'OrchestratorAgent) car chaque outil capture
// l'identité d'exécution propre à la requête courante : l'identité n'est
// jamais décidée par le modèle (InvariantRules, règle 1), seulement
// transmise par l'application.
func (a *OrchestratorAgent) buildDelegationTools(identity model.ExecutionIdentity, attachments []media.Media, collector *proposalCollector, mediaCollector *mediaCollector) []llm.Tool {
	tools := make([]llm.Tool, 0, len(a.specialists))

	for agentID, specialist := range a.specialists {
		tools = append(tools, newDelegationTool(agentID, a.specialistDescriptions[agentID], specialist, identity, attachments, collector, mediaCollector, a.metrics, a.logger))
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
func newDelegationTool(agentID, description string, specialist delegation.Specialist, identity model.ExecutionIdentity, attachments []media.Media, collector *proposalCollector, mediaCollector *mediaCollector, metrics *observability.Metrics, logger *slog.Logger) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("goal", "Precise objective for the specialist to reach.", "string").
		Property("relevant_input", "Context strictly needed for the task, spelled out. Never pass the whole conversation history.", "string").
		Property("constraints", "Additional constraints the specialist must respect.", "array", map[string]any{"type": "string"}, "items")

	// La description du spécialiste (agents.<nom>.description) est ce sur
	// quoi le modèle décide de déléguer ou non : sans elle il ne connaît que
	// le nom du délégué, et un petit modèle préfère répondre qu'il ne sait
	// pas faire plutôt que d'appeler un outil dont il ignore la portée.
	toolDescription := fmt.Sprintf("Delegate a task to the %q specialist and get a summary of its result.", agentID)
	if strings.TrimSpace(description) != "" {
		toolDescription = fmt.Sprintf("Delegate a task to the %q specialist, which %s. Returns a summary of its result.", agentID, strings.TrimSpace(description))
	}

	return llm.NewFuncTool(
		"delegate_to_"+agentID,
		toolDescription,
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
				// Les pièces jointes du tour accompagnent toujours la
				// délégation : le modèle ne peut pas les recopier dans
				// relevant_input (voir delegation.Request.Attachments).
				Attachments: attachments,
			})
			if err != nil {
				// L'échec part au modèle sous forme de texte : sans cette
				// trace, il ne subsisterait AUCUNE trace technique de la
				// panne, le tour se terminant par un « tour terminé » normal
				// après que le modèle a reformulé l'erreur à sa façon.
				if logger != nil {
					logger.WarnContext(ctx, "agent: spécialiste en échec", "agent", agentID, "error", err)
				}

				return llm.NewToolResult(fmt.Sprintf("le spécialiste %q a échoué: %v", agentID, err)), nil
			}

			for _, pa := range result.ProposedActions {
				collector.add(pa)
			}

			// Les médias produits par le spécialiste remontent jusqu'à la
			// réponse envoyée à l'utilisateur, sans passer par le texte du
			// résumé (que le modèle réécrit librement).
			mediaCollector.add(result.Attachments...)

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

// WithReminderTools attache tools à a : les outils create_reminder/
// list_reminders/cancel_reminder sont exposés au modèle en plus des
// délégations, dès le prochain Execute. Un appel avec la valeur zéro de
// ReminderTools désactive tous les outils de rappels (comportement par
// défaut d'un OrchestratorAgent tout juste construit). Retourne a pour
// permettre le chaînage à la construction, comme WithMemoryTools.
func (a *OrchestratorAgent) WithReminderTools(tools ReminderTools) *OrchestratorAgent {
	a.reminderTools = tools
	return a
}

// WithSpecialistDescriptions renseigne, par nom de délégué, la phrase
// décrivant ce qu'il sait faire (agents.<nom>.description). Elle est reprise
// dans la description de l'outil delegate_to_<nom>. Un nom absent de la map
// garde la description générique.
func (a *OrchestratorAgent) WithSpecialistDescriptions(descriptions map[string]string) *OrchestratorAgent {
	a.specialistDescriptions = descriptions
	return a
}

// WithLogger attache logger à a : chaque tour journalise alors son
// introspection (outils exposés, appels d'outils avec durée, fin de tour) —
// jamais les arguments ni les résultats, qui portent du contenu privé. nil
// (défaut) désactive cette journalisation.
func (a *OrchestratorAgent) WithLogger(logger *slog.Logger) *OrchestratorAgent {
	a.logger = logger
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
