package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
)

// fakeSkillsProvider sert un catalogue fixe, avec le ciblage par agent.
type fakeSkillsProvider struct {
	// byAgent associe un nom d'agent aux compétences qui lui sont
	// visibles ; la clé "*" vaut pour tous.
	byAgent map[string][]agent.SkillSummary
	content map[string]string
}

func (f *fakeSkillsProvider) SkillsFor(_ context.Context, agentName string) []agent.SkillSummary {
	return append(append([]agent.SkillSummary(nil), f.byAgent["*"]...), f.byAgent[agentName]...)
}

func (f *fakeSkillsProvider) LoadSkill(ctx context.Context, agentName, skillName string) (string, bool) {
	for _, s := range f.SkillsFor(ctx, agentName) {
		if s.Name == skillName {
			content, ok := f.content[skillName]
			return content, ok
		}
	}
	return "", false
}

func testSkillsProvider() *fakeSkillsProvider {
	return &fakeSkillsProvider{
		byAgent: map[string][]agent.SkillSummary{
			"*":         {{Name: "write-a-summary", Description: "Summarize a long document"}},
			"workspace": {{Name: "mask-logo-in-image", Description: "Mask a logo on a photo"}},
		},
		content: map[string]string{
			"write-a-summary":    "Read it, then write three bullets.",
			"mask-logo-in-image": "Locate the logo, blur the region, attach the file.",
		},
	}
}

// systemPromptOf retourne le message système du premier tour.
func systemPromptOf(t *testing.T, client *fakeCompletionClient) string {
	t.Helper()

	if len(client.optsHistory) == 0 {
		t.Fatal("aucun appel au modèle")
	}
	for _, m := range client.optsHistory[0].Messages {
		if m.Role() == llm.RoleSystem {
			return m.Content()
		}
	}
	t.Fatal("aucun message système")
	return ""
}

// toolNamesOf retourne les noms d'outils exposés au premier tour.
func toolNamesOf(t *testing.T, client *fakeCompletionClient) []string {
	t.Helper()

	if len(client.optsHistory) == 0 {
		t.Fatal("aucun appel au modèle")
	}
	var names []string
	for _, tool := range client.optsHistory[0].Tools {
		names = append(names, tool.Name())
	}
	return names
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// Le catalogue rejoint le prompt système du tour et load_skill les
// outils, dès que la bibliothèque a quelque chose pour cet agent.
func TestOrchestrator_SkillCatalogReachesPrompt(t *testing.T) {
	client := &fakeCompletionClient{
		responseFunc: func(_ int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("ok"), nil
		},
	}

	orchestrator := agent.NewOrchestratorAgent(client, "system", "main", nil, 3).
		WithSkills(testSkillsProvider())

	if _, err := orchestrator.Execute(context.Background(), agent.Request{Input: "bonjour"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	prompt := systemPromptOf(t, client)
	if !strings.Contains(prompt, "## Skills") {
		t.Errorf("le catalogue devrait figurer dans le prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "write-a-summary — Summarize a long document") {
		t.Errorf("l'entrée de catalogue est absente: %q", prompt)
	}
	if strings.Contains(prompt, "mask-logo-in-image") {
		t.Error("une compétence ciblée workspace ne devrait pas atteindre l'orchestrateur")
	}
	if !contains(toolNamesOf(t, client), "load_skill") {
		t.Errorf("load_skill devrait être monté: %v", toolNamesOf(t, client))
	}
}

// Sans provider, ni catalogue ni outil : aucun budget de tokens dépensé
// pour une bibliothèque inexistante.
func TestOrchestrator_NoSkillsNoCatalog(t *testing.T) {
	client := &fakeCompletionClient{
		responseFunc: func(_ int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("ok"), nil
		},
	}

	orchestrator := agent.NewOrchestratorAgent(client, "system", "main", nil, 3)
	if _, err := orchestrator.Execute(context.Background(), agent.Request{Input: "bonjour"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(systemPromptOf(t, client), "## Skills") {
		t.Error("aucun catalogue ne devrait être injecté sans provider")
	}
	if contains(toolNamesOf(t, client), "load_skill") {
		t.Error("load_skill ne devrait pas être monté sans provider")
	}
}

// Un catalogue vide pour cet agent équivaut à l'absence de provider.
func TestOrchestrator_EmptyCatalogMountsNothing(t *testing.T) {
	client := &fakeCompletionClient{
		responseFunc: func(_ int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("ok"), nil
		},
	}

	orchestrator := agent.NewOrchestratorAgent(client, "system", "main", nil, 3).
		WithSkills(&fakeSkillsProvider{})

	if _, err := orchestrator.Execute(context.Background(), agent.Request{Input: "bonjour"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(systemPromptOf(t, client), "## Skills") {
		t.Error("un catalogue vide ne devrait rien injecter")
	}
	if contains(toolNamesOf(t, client), "load_skill") {
		t.Error("load_skill ne devrait pas être monté sur un catalogue vide")
	}
}

// load_skill rend le contenu markdown tel quel — aucune inférence, aucun
// réseau.
func TestLoadSkillReturnsContent(t *testing.T) {
	var results []string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "load_skill", `{"name":"write-a-summary"}`)), nil
			}
			return scriptedFinalResponse("Voilà le résumé."), nil
		},
	}

	orchestrator := agent.NewOrchestratorAgent(client, "system", "main", nil, 3).
		WithSkills(testSkillsProvider())

	if _, err := orchestrator.Execute(context.Background(), agent.Request{Input: "résume ce texte"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, m := range client.optsHistory[len(client.optsHistory)-1].Messages {
		if m.Role() == llm.RoleTool {
			results = append(results, m.Content())
		}
	}

	joined := strings.Join(results, "\n")
	if !strings.Contains(joined, "Read it, then write three bullets.") {
		t.Errorf("le contenu de la compétence n'est pas revenu au modèle: %q", joined)
	}
}

// Une compétence introuvable est un échec MÉTIER : un résultat d'outil
// « error: … », jamais une erreur Go qui avorterait le tour.
func TestLoadSkillUnknownIsToolResult(t *testing.T) {
	var toolResults []string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", "load_skill", `{"name":"invented-skill"}`)), nil
			}
			return scriptedFinalResponse("Je fais sans."), nil
		},
	}

	orchestrator := agent.NewOrchestratorAgent(client, "system", "main", nil, 3).
		WithSkills(testSkillsProvider())

	result, err := orchestrator.Execute(context.Background(), agent.Request{Input: "fais quelque chose"})
	if err != nil {
		t.Fatalf("un nom inconnu ne doit pas avorter le tour: %v", err)
	}
	if result.Reply == "" {
		t.Error("le tour devrait tout de même produire une réponse")
	}

	for _, m := range client.optsHistory[len(client.optsHistory)-1].Messages {
		if m.Role() == llm.RoleTool {
			toolResults = append(toolResults, m.Content())
		}
	}

	joined := strings.Join(toolResults, "\n")
	if !strings.Contains(joined, "error: no skill named") {
		t.Errorf("le refus devrait revenir en résultat d'outil: %q", joined)
	}
}

// Le ciblage se lit du point de vue du sous-agent de plugin : le nom
// d'agent est celui du plugin.
func TestPluginSubAgent_SeesTargetedSkill(t *testing.T) {
	client := &fakeCompletionClient{
		responseFunc: func(_ int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("fait"), nil
		},
	}

	spec := testPluginSpec()
	spec.PluginName = "workspace"

	subAgent := agent.NewPluginSubAgent(spec, client, &fakePluginCaller{}, 0, nil).
		WithSkills(testSkillsProvider())

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "workspace",
		Goal:     "masque le logo",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	prompt := systemPromptOf(t, client)
	if !strings.Contains(prompt, "mask-logo-in-image") {
		t.Errorf("la compétence ciblée workspace devrait être visible: %q", prompt)
	}
	if !contains(toolNamesOf(t, client), "load_skill") {
		t.Errorf("load_skill devrait être monté sur le sous-agent: %v", toolNamesOf(t, client))
	}
}

// Le même provider ne montre pas cette compétence à un autre agent.
func TestPluginSubAgent_DoesNotSeeOtherAgentsSkill(t *testing.T) {
	client := &fakeCompletionClient{
		responseFunc: func(_ int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("fait"), nil
		},
	}

	spec := testPluginSpec()
	spec.PluginName = "research"

	subAgent := agent.NewPluginSubAgent(spec, client, &fakePluginCaller{}, 0, nil).
		WithSkills(testSkillsProvider())

	if _, err := subAgent.Execute(context.Background(), delegation.Request{
		AgentID:  "research",
		Goal:     "cherche",
		Identity: pluginTestIdentity(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(systemPromptOf(t, client), "mask-logo-in-image") {
		t.Error("une compétence ciblée workspace ne devrait pas atteindre research")
	}
}
