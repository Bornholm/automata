package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
)

// fixedCustomizer retourne toujours la même personnalisation.
type fixedCustomizer struct {
	custom agent.OrgCustomization
	err    error
}

func (c fixedCustomizer) CustomizationFor(context.Context, string) (agent.OrgCustomization, error) {
	return c.custom, c.err
}

// captureClient retient les options du premier appel.
type captureClient struct {
	opts *llm.ChatCompletionOptions
}

func (c *captureClient) ChatCompletion(_ context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	if c.opts == nil {
		c.opts = llm.NewChatCompletionOptions(funcs...)
	}
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, "ok"), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

func customizationRequest() agent.Request {
	return agent.Request{
		Identity: model.ExecutionIdentity{PrincipalID: "alice", OrgID: "atelier"},
		Input:    "bonjour",
	}
}

// customizationSpecialists fournit deux spécialistes délégables.
func customizationSpecialists() map[string]delegation.Specialist {
	return map[string]delegation.Specialist{
		"research": &fakeSpecialist{},
		"imagine":  &fakeSpecialist{},
	}
}

func TestOrgCustomization_RemovesDisabledSpecialists(t *testing.T) {
	client := &captureClient{}
	orchestrator := agent.NewOrchestratorAgent(client, "prompt", "main", customizationSpecialists(), 6).
		WithOrgCustomizer(fixedCustomizer{custom: agent.OrgCustomization{DisabledAgents: []string{"imagine"}}})

	if _, err := orchestrator.Execute(context.Background(), customizationRequest()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var names []string
	for _, tool := range client.opts.Tools {
		names = append(names, tool.Name())
	}

	joined := strings.Join(names, " ")
	if strings.Contains(joined, "delegate_to_imagine") {
		t.Errorf("le spécialiste retiré ne doit pas être exposé au modèle: %v", names)
	}
	if !strings.Contains(joined, "delegate_to_research") {
		t.Errorf("les spécialistes conservés doivent rester exposés: %v", names)
	}
}

func TestOrgCustomization_AppendsPromptWithoutReplacingIt(t *testing.T) {
	client := &captureClient{}
	orchestrator := agent.NewOrchestratorAgent(client, "REGLES DE L'INSTANCE", "main", nil, 6).
		WithOrgCustomizer(fixedCustomizer{custom: agent.OrgCustomization{PromptExtra: "Ton sobre et technique."}})

	if _, err := orchestrator.Execute(context.Background(), customizationRequest()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	system := client.opts.Messages[0].Content()
	if !strings.Contains(system, "REGLES DE L'INSTANCE") {
		t.Error("la consigne d'organisation ne doit jamais remplacer les règles de l'instance")
	}
	if !strings.Contains(system, "Ton sobre et technique.") {
		t.Error("la consigne d'organisation doit être ajoutée au prompt")
	}
	if strings.Index(system, "REGLES DE L'INSTANCE") > strings.Index(system, "Ton sobre et technique.") {
		t.Error("la consigne d'organisation doit venir après les règles de l'instance")
	}
}

// Une personnalisation illisible ne doit pas faire échouer le tour : le
// client parle, on lui répond avec les réglages par défaut.
func TestOrgCustomization_FailureFallsBackToDefaults(t *testing.T) {
	client := &captureClient{}
	orchestrator := agent.NewOrchestratorAgent(client, "prompt", "main", customizationSpecialists(), 6).
		WithOrgCustomizer(fixedCustomizer{err: errCustomization})

	if _, err := orchestrator.Execute(context.Background(), customizationRequest()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(client.opts.Tools) == 0 {
		t.Error("les outils par défaut doivent rester exposés malgré l'échec de lecture")
	}
}

type customizationError string

func (e customizationError) Error() string { return string(e) }

const errCustomization = customizationError("base indisponible")
