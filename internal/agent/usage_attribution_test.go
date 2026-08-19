package agent_test

import (
	"context"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/usage"
)

// ctxCapturingClient enregistre le contexte reçu par ChatCompletion, pour
// vérifier ce que le décorateur d'usage (internal/usage) y lirait au moment
// de l'appel réel.
type ctxCapturingClient struct {
	captured []context.Context
}

func (c *ctxCapturingClient) ChatCompletion(ctx context.Context, _ ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	c.captured = append(c.captured, ctx)
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, "ok"), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

func TestOrchestratorAgent_AttachesUsageAttributionToLLMCalls(t *testing.T) {
	client := &ctxCapturingClient{}
	orchestrator := agent.NewOrchestratorAgent(client, "prompt", "main", nil, 3)

	_, err := orchestrator.Execute(context.Background(), agent.Request{
		Identity: model.ExecutionIdentity{
			PrincipalID:    "alice",
			OrgID:          "home",
			ConversationID: "conv-42",
		},
		Input: "bonjour",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(client.captured) == 0 {
		t.Fatal("aucun appel LLM capturé")
	}

	attr, ok := usage.AttributionFromContext(client.captured[0])
	if !ok {
		t.Fatal("le contexte de l'appel LLM doit porter une attribution d'usage")
	}
	if attr.OrgID != "home" || attr.PrincipalID != "alice" || attr.ConversationID != "conv-42" {
		t.Errorf("attribution inattendue: %+v", attr)
	}
	if attr.Component != usage.ComponentAgent || attr.Agent != "main" {
		t.Errorf("composant/agent inattendus: %+v", attr)
	}
}
