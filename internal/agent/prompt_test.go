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
		Scope:          model.ScopeGroup,
		ChannelKind:    model.ChannelGroup,
		OrgDisplayName: "Maison",
	}

	block := agent.BuildContextBlock(identity, "agenda", time.Now())

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
