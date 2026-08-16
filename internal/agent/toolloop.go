package agent

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
)

// proposalCollector accumule les delegation.ProposedAction produites par
// les outils exécutés durant un tour de runToolLoop (PLAN.md §10, Phase
// 15). C'est le canal choisi pour faire remonter une proposition depuis
// l'exécution d'un outil LLM jusqu'à Agent.Result, sans changer la
// signature de llm.Tool.Execute (context.Context, map[string]any) ni celle
// de runToolLoop : un outil qui produit une proposition (voir
// MemoryTools.newForgetMemoryTool) capture le collector correspondant à
// l'exécution courante dans sa closure et y ajoute l'action, en plus de
// retourner un texte lisible au modèle. Protégé par un mutex bien que
// runToolLoop exécute les outils strictement séquentiellement (jamais deux
// exécutions concurrentes pour un même tour) : un canal explicitement
// thread-safe est plus robuste qu'une hypothèse implicite sur l'ordre
// d'exécution qui pourrait changer sans que ce fichier soit relu.
type proposalCollector struct {
	mu        sync.Mutex
	proposals []delegation.ProposedAction
}

func newProposalCollector() *proposalCollector {
	return &proposalCollector{}
}

// add ajoute p aux propositions accumulées. Sûr à appeler avec un récepteur
// nil (aucun collector actif) : ne fait rien.
func (c *proposalCollector) add(p delegation.ProposedAction) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proposals = append(c.proposals, p)
}

// take retourne les propositions accumulées. Sûr à appeler avec un
// récepteur nil (retourne nil).
func (c *proposalCollector) take() []delegation.ProposedAction {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proposals
}

// toolLoopResult est le résultat d'un appel à runToolLoop : le texte de la
// réponse finale du modèle (sans tool-call), et le contenu textuel de
// chaque résultat d'outil exécuté durant le tour, dans l'ordre d'exécution
// (utilisé par MCPToolAgent pour en extraire des références, voir
// mcp_tool_agent.go).
type toolLoopResult struct {
	Text        string
	ToolResults []string
}

// runToolLoop factorise la mécanique "appeler le LLM avec des tools ->
// exécuter les tool-calls -> renvoyer les résultats -> rappeler ->
// plafonner", partagée par OrchestratorAgent (délégation, Phase 8) et
// MCPToolAgent (outils MCP réels, Phase 12) : les deux ont besoin
// exactement de la même boucle de tool-calling non-streaming, seuls les
// outils exposés au modèle diffèrent.
//
// messages doit déjà contenir le system prompt (avec bloc de contexte) et
// l'historique de la requête courante (voir buildChatMessages) : runToolLoop
// n'en construit aucun lui-même. maxIterations <= 0 retombe sur 1 (au moins
// un appel de complétion, jamais de boucle illimitée). maxReachedErr est
// retournée telle quelle si maxIterations tours se sont écoulés sans réponse
// finale du modèle : chaque appelant fournit son propre sentinel (voir
// ErrMaxDelegationsReached, ErrMaxToolCallsReached) pour que
// errors.Is continue de distinguer les deux origines côté appelant.
//
// maxContextBytes borne le cumul des résultats d'outils réinjectés dans la
// conversation au fil du tour (PLAN.md §9.4, "budget total des résultats",
// agents.<nom>.limits.max_tool_context_bytes) ; <= 0 le laisse illimité. Ce
// budget est distinct du plafond par résultat appliqué côté MCP
// (mcp.Limits.MaxToolResultBytes) : sans lui, maxIterations appels tenant
// chacun sous le plafond unitaire dépassent tout de même largement le
// contexte annoncé.
func runToolLoop(ctx context.Context, client llm.ChatCompletionClient, messages []llm.Message, tools []llm.Tool, maxIterations int, maxContextBytes int64, maxReachedErr error) (toolLoopResult, error) {
	if maxIterations <= 0 {
		maxIterations = 1
	}

	var (
		toolResults []string
		usedBytes   int64
	)

	for range maxIterations {
		resp, err := client.ChatCompletion(ctx, llm.WithMessages(messages...), llm.WithTools(tools...))
		if err != nil {
			return toolLoopResult{}, fmt.Errorf("agent: appel du client llm: %w", err)
		}

		toolCalls := resp.ToolCalls()
		if len(toolCalls) == 0 {
			text := ""
			if msg := resp.Message(); msg != nil {
				text = msg.Content()
			}

			if text == "" {
				return toolLoopResult{}, ErrEmptyReply
			}

			return toolLoopResult{Text: text, ToolResults: toolResults}, nil
		}

		messages = append(messages, llm.NewToolCallsMessage(toolCalls...))

		for _, tc := range toolCalls {
			toolMessage, err := llm.ExecuteToolCall(ctx, tc, tools...)
			if err != nil {
				// Un échec dur (paramètres illisibles, panique interne au
				// tool) ne doit pas faire échouer tout le tour : on le
				// transmet au modèle comme résultat d'outil, exactement
				// comme un échec applicatif du spécialiste (voir
				// newDelegationTool), pour qu'il puisse s'adapter.
				toolMessage = llm.NewToolMessage(tc.ID(), llm.NewToolResult(fmt.Sprintf("erreur d'exécution de l'outil %q: %v", tc.Name(), err)))
			}

			// Le budget ne s'applique qu'au texte réinjecté dans la
			// conversation : l'outil a déjà été exécuté, ses effets (actions
			// proposées, appels MCP en lecture) sont donc préservés. Seul ce
			// que le modèle relit est borné, et toute réduction lui est
			// signalée explicitement (PLAN.md §9.4).
			content, used := applyContextBudget(toolMessage.Content(), maxContextBytes, usedBytes)
			if content != toolMessage.Content() {
				toolMessage = llm.NewToolMessage(tc.ID(), llm.NewToolResult(content))
			}
			usedBytes = used

			messages = append(messages, toolMessage)
			toolResults = append(toolResults, toolMessage.Content())
		}

		if err := ctx.Err(); err != nil {
			return toolLoopResult{}, err
		}
	}

	return toolLoopResult{}, maxReachedErr
}

// applyContextBudget réduit content pour tenir dans ce qu'il reste de
// maxBytes après usedBytes déjà consommés, et retourne le texte à réinjecter
// ainsi que le nouveau cumul. maxBytes <= 0 désactive le budget.
//
// Une fois le budget épuisé, les résultats suivants ne sont pas simplement
// omis en silence : le modèle reçoit une note explicite, sans quoi il
// interpréterait un résultat vide comme "l'outil n'a rien trouvé" et
// conclurait à tort.
func applyContextBudget(content string, maxBytes, usedBytes int64) (string, int64) {
	if maxBytes <= 0 {
		return content, usedBytes
	}

	remaining := maxBytes - usedBytes
	if remaining <= 0 {
		return "[budget de contexte d'outils épuisé : résultat non transmis]", usedBytes
	}

	if int64(len(content)) <= remaining {
		return content, usedBytes + int64(len(content))
	}

	truncated := truncateUTF8(content, remaining)

	return truncated + fmt.Sprintf("\n[résultat tronqué : budget de contexte d'outils de %d octets atteint]", maxBytes), maxBytes
}

// truncateUTF8 coupe s à max octets au plus, sans jamais laisser une
// séquence UTF-8 incomplète en fin de chaîne.
func truncateUTF8(s string, max int64) string {
	if max <= 0 {
		return ""
	}
	if int64(len(s)) <= max {
		return s
	}

	b := s[:max]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}

	return b
}
