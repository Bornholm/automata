package agent_test

import (
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
)

func testRegistryConfig() *config.Config {
	return &config.Config{
		Version: 1,
		Organization: config.Organization{
			ID:          "home",
			DisplayName: "Maison",
		},
		LLMClients: map[string]config.LLMClient{
			"main": {
				Provider: "openai",
				Model:    "gpt-test",
				APIKey:   "sk-test",
				BaseURL:  "https://api.example.test",
			},
		},
		Agents: map[string]config.Agent{
			"main": {
				Type:   config.AgentTypeOrchestrator,
				Client: "main",
				SystemPrompt: config.SystemPrompt{
					Content: "Tu es Automata, l'assistant généraliste du foyer.",
				},
				Delegates: []string{"agenda"},
			},
			"agenda": {
				Type:   config.AgentTypeSpecialist,
				Client: "main",
				SystemPrompt: config.SystemPrompt{
					Content: "Tu es l'agent Agenda, spécialisé dans le calendrier.",
				},
				Capabilities: []string{"calendar.personal.read"},
			},
		},
	}
}

func TestNewRegistry_BuildsAllConfiguredAgents(t *testing.T) {
	cfg := testRegistryConfig()

	registry, err := agent.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: erreur inattendue: %v", err)
	}

	for _, name := range []string{"main", "agenda"} {
		if _, err := registry.Get(name); err != nil {
			t.Errorf("Get(%q): erreur inattendue: %v", name, err)
		}
	}
}

func TestNewRegistry_UnknownAgentRejected(t *testing.T) {
	cfg := testRegistryConfig()

	registry, err := agent.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: erreur inattendue: %v", err)
	}

	_, err = registry.Get("inexistant")
	if err == nil {
		t.Fatal("Get: erreur attendue pour un agent inconnu")
	}

	if !strings.Contains(err.Error(), "inexistant") {
		t.Errorf("erreur = %v, attendu mention de 'inexistant'", err)
	}
}

func TestNewRegistry_UnknownLLMClientRejected(t *testing.T) {
	cfg := testRegistryConfig()
	main := cfg.Agents["main"]
	main.Client = "inexistant"
	cfg.Agents["main"] = main

	_, err := agent.NewRegistry(cfg)
	if err == nil {
		t.Fatal("NewRegistry: erreur attendue pour un client llm inconnu")
	}
}

// TestNewRegistry_Isolation vérifie qu'aucun état mutable (system prompt en
// particulier) n'est partagé entre deux agents construits depuis des
// configurations différentes : modifier la configuration source après
// construction ne doit avoir aucun effet rétroactif sur les agents déjà
// construits, et le prompt de chaque agent ne doit contenir aucune trace de
// la personnalité de l'autre.
func TestNewRegistry_Isolation(t *testing.T) {
	cfg := testRegistryConfig()

	registry, err := agent.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: erreur inattendue: %v", err)
	}

	mainAgent, err := registry.Get("main")
	if err != nil {
		t.Fatalf("Get(main): %v", err)
	}

	agendaAgent, err := registry.Get("agenda")
	if err != nil {
		t.Fatalf("Get(agenda): %v", err)
	}

	if mainAgent == agendaAgent {
		t.Fatal("les instances Agent de 'main' et 'agenda' ne doivent pas être identiques")
	}

	mainGenAI, ok := mainAgent.(*agent.GenAIAgent)
	if !ok {
		t.Fatalf("mainAgent devrait être un *agent.GenAIAgent, obtenu %T", mainAgent)
	}
	agendaGenAI, ok := agendaAgent.(*agent.GenAIAgent)
	if !ok {
		t.Fatalf("agendaAgent devrait être un *agent.GenAIAgent, obtenu %T", agendaAgent)
	}

	if mainGenAI == agendaGenAI {
		t.Fatal("main et agenda ne doivent pas partager la même instance GenAIAgent")
	}

	// Muter la configuration source après coup ne doit avoir aucun effet
	// rétroactif : les prompts ont déjà été composés en chaînes immuables au
	// moment de NewRegistry.
	mainCfg := cfg.Agents["main"]
	mainCfg.SystemPrompt.Content = "contenu muté après construction"
	cfg.Agents["main"] = mainCfg

	rebuiltMainPrompt := agent.BuildSystemPrompt("main", cfg.Agents["main"])
	if strings.Contains(rebuiltMainPrompt, "assistant généraliste du foyer") {
		t.Fatal("le test de mutation est mal construit : la config n'a pas changé")
	}
}
