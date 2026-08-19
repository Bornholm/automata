package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

func testAgentConfigs() map[string]config.Agent {
	return map[string]config.Agent{
		"main": {
			Type:   config.AgentTypeOrchestrator,
			Client: "main",
			SystemPrompt: config.SystemPrompt{
				Content: "Tu es Automata, l'assistant généraliste du foyer, méthodique et concis.",
			},
			Delegates: []string{"agenda"},
			Capabilities: []string{
				"memory.personal.read",
				"memory.personal.write",
			},
		},
		"agenda": {
			Type:   config.AgentTypeSpecialist,
			Client: "main",
			SystemPrompt: config.SystemPrompt{
				Content: "Tu es l'agent Agenda, spécialisé dans la gestion du calendrier familial.",
			},
			Capabilities: []string{
				"calendar.personal.read",
				"calendar.personal.write",
			},
		},
	}
}

func TestBuildSystemPrompt_MainContainsInvariantRulesAndConfiguredContent(t *testing.T) {
	cfgs := testAgentConfigs()

	prompt := agent.BuildSystemPrompt("main", cfgs["main"])

	if !strings.Contains(prompt, agent.InvariantRules) {
		t.Error("le prompt de 'main' doit contenir les règles invariantes")
	}

	if !strings.Contains(prompt, "Automata, l'assistant généraliste du foyer") {
		t.Error("le prompt de 'main' doit contenir le contenu configuré de 'main'")
	}

	if !strings.Contains(prompt, "memory.personal.read") {
		t.Error("le prompt de 'main' doit lister ses capacités déclarées")
	}

	if !strings.Contains(prompt, "agenda") {
		t.Error("le prompt de 'main' doit mentionner ses délégués déclarés")
	}
}

func TestBuildSystemPrompt_AgendaHasOwnRulesAndPersonalityDifferentFromMain(t *testing.T) {
	cfgs := testAgentConfigs()

	agendaPrompt := agent.BuildSystemPrompt("agenda", cfgs["agenda"])
	mainPrompt := agent.BuildSystemPrompt("main", cfgs["main"])

	if !strings.Contains(agendaPrompt, agent.InvariantRules) {
		t.Error("le prompt de 'agenda' doit contenir les règles invariantes")
	}

	if !strings.Contains(agendaPrompt, "agent Agenda, spécialisé dans la gestion du calendrier") {
		t.Error("le prompt de 'agenda' doit contenir sa propre personnalité configurée")
	}

	if agendaPrompt == mainPrompt {
		t.Fatal("les prompts de 'agenda' et 'main' ne doivent pas être identiques")
	}
}

func TestBuildSystemPrompt_NoCrossContamination(t *testing.T) {
	cfgs := testAgentConfigs()

	mainPrompt := agent.BuildSystemPrompt("main", cfgs["main"])
	agendaPrompt := agent.BuildSystemPrompt("agenda", cfgs["agenda"])

	if strings.Contains(mainPrompt, "agent Agenda, spécialisé dans la gestion du calendrier") {
		t.Error("le prompt de 'main' ne doit contenir aucun fragment de la personnalité configurée de 'agenda'")
	}
	if strings.Contains(mainPrompt, "calendar.personal.read") {
		t.Error("le prompt de 'main' ne doit contenir aucune capacité déclarée de 'agenda'")
	}

	if strings.Contains(agendaPrompt, "Automata, l'assistant généraliste du foyer") {
		t.Error("le prompt de 'agenda' ne doit contenir aucun fragment de la personnalité configurée de 'main'")
	}
	if strings.Contains(agendaPrompt, "memory.personal.read") {
		t.Error("le prompt de 'agenda' ne doit contenir aucune capacité déclarée de 'main'")
	}
}

func TestBuildSystemPrompt_InvariantRulesPresentInAllAgents(t *testing.T) {
	cfgs := testAgentConfigs()

	for name, cfg := range cfgs {
		prompt := agent.BuildSystemPrompt(name, cfg)
		if !strings.Contains(prompt, agent.InvariantRules) {
			t.Errorf("le prompt de %q doit contenir les règles invariantes", name)
		}
	}
}

func TestBuildContextBlock_ContainsAllowedVariablesOnly(t *testing.T) {
	identity := model.ExecutionIdentity{
		Scope:                model.ScopeGroup,
		ChannelKind:          model.ChannelGroup,
		OrgDisplayName:       "Maison",
		PrincipalDisplayName: "Alice",
		PrincipalID:          "alice-internal-id",
		Trigger:              model.TriggerMessage,
	}

	block := agent.BuildContextBlock(identity, "Groupe principal", "agenda", time.Now())

	if !strings.Contains(block, "agenda") {
		t.Error("le bloc de contexte doit mentionner le nom de l'agent")
	}
	if !strings.Contains(block, "Maison") {
		t.Error("le bloc de contexte doit mentionner le nom de l'organisation")
	}
	if !strings.Contains(block, string(model.ScopeGroup)) {
		t.Error("le bloc de contexte doit mentionner la portée d'exécution")
	}
	if !strings.Contains(block, string(model.ChannelGroup)) {
		t.Error("le bloc de contexte doit mentionner le type de canal")
	}
	if !strings.Contains(block, "Talking to: Alice") {
		t.Error("le bloc de contexte doit nommer l'interlocuteur par son nom affiché")
	}
	if strings.Contains(block, "alice-internal-id") {
		t.Error("l'identifiant interne du principal ne doit JAMAIS atteindre le modèle")
	}
	if !strings.Contains(block, "Channel name: Groupe principal") {
		t.Error("le bloc de contexte doit mentionner le nom du canal")
	}
	if !strings.Contains(block, "Trigger: incoming user message") {
		t.Error("le bloc de contexte doit mentionner le déclencheur")
	}
}

// Une exécution planifiée ne répond à personne : le bloc de contexte doit le
// dire au modèle, sinon il rédige comme s'il répondait à un message.
func TestBuildContextBlock_ScheduledTriggerIsExplicit(t *testing.T) {
	identity := model.ExecutionIdentity{
		Trigger:     model.TriggerScheduledTask,
		Scope:       model.ScopePersonal,
		ChannelKind: model.ChannelPrivate,
	}

	block := agent.BuildContextBlock(identity, "", "main", time.Now())

	if !strings.Contains(block, "a task you previously scheduled") {
		t.Error("le bloc doit signaler l'exécution planifiée")
	}
	if strings.Contains(block, "Channel name:") {
		t.Error("un canal sans nom ne doit pas produire de ligne vide")
	}
}

func TestBuildSystemPrompt_ContainsHonestyRules(t *testing.T) {
	cfgs := testAgentConfigs()

	for name, cfg := range cfgs {
		prompt := agent.BuildSystemPrompt(name, cfg)
		if !strings.Contains(prompt, "## Limits and honesty") {
			t.Errorf("le prompt de %q doit contenir la section codée en dur sur les limites et l'honnêteté", name)
		}
		if !strings.Contains(prompt, "never announce a future action") {
			t.Errorf("le prompt de %q doit interdire d'annoncer une action future", name)
		}
		// Le durcissement anti-fausse confirmation est codé en dur, hors de
		// portée de la configuration : c'est le garde-fou qui rattrape un
		// prompt d'agent mal rédigé.
		if !strings.Contains(prompt, "unless a\ntool call in THIS turn returned success") {
			t.Errorf("le prompt de %q doit interdire d'annoncer une action non exécutée", name)
		}
	}
}

// Une surcharge de prompt par organisation ne remplace que la personnalité :
// les règles invariantes, les capacités et les règles d'honnêteté sont
// recomposées à l'identique. Aucune organisation ne doit pouvoir en être
// exemptée par configuration.
func TestBuildOrgSystemPrompts_KeepsHardcodedRules(t *testing.T) {
	agentCfg := testAgentConfigs()["main"]
	agentCfg.SystemPrompt.OrgOverrides = map[string]config.SystemPrompt{
		"work": {Content: "You are the assistant of the engineering team."},
	}

	prompts := agent.BuildOrgSystemPrompts("main", agentCfg)

	workPrompt, ok := prompts["work"]
	if !ok {
		t.Fatalf("prompts = %v, attendu une variante pour l'organisation work", prompts)
	}

	if !strings.Contains(workPrompt, "assistant of the engineering team") {
		t.Error("la variante doit porter la personnalité surchargée")
	}
	if strings.Contains(workPrompt, "assistant généraliste du foyer") {
		t.Error("la variante ne doit pas contenir la personnalité par défaut")
	}
	if !strings.Contains(workPrompt, agent.InvariantRules) {
		t.Error("la variante doit conserver les règles invariantes")
	}
	if !strings.Contains(workPrompt, "## Limits and honesty") {
		t.Error("la variante doit conserver les règles d'honnêteté codées en dur")
	}
}

// Sans surcharge déclarée, BuildOrgSystemPrompts retourne nil : les agents
// retombent alors systématiquement sur leur prompt par défaut.
func TestBuildOrgSystemPrompts_NilWithoutOverrides(t *testing.T) {
	if got := agent.BuildOrgSystemPrompts("main", testAgentConfigs()["main"]); got != nil {
		t.Errorf("BuildOrgSystemPrompts = %v, attendu nil", got)
	}
}
