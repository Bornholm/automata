package agent

import (
	"context"
	"fmt"

	"github.com/bornholm/genai/llm"
)

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
func runToolLoop(ctx context.Context, client llm.ChatCompletionClient, messages []llm.Message, tools []llm.Tool, maxIterations int, maxReachedErr error) (toolLoopResult, error) {
	if maxIterations <= 0 {
		maxIterations = 1
	}

	var toolResults []string

	for i := 0; i < maxIterations; i++ {
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
			messages = append(messages, toolMessage)
			toolResults = append(toolResults, toolMessage.Content())
		}

		if err := ctx.Err(); err != nil {
			return toolLoopResult{}, err
		}
	}

	return toolLoopResult{}, maxReachedErr
}
