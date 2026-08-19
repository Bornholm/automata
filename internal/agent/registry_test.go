package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/circuitbreaker"

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

	registry, err := agent.NewRegistry(cfg, nil)
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

	registry, err := agent.NewRegistry(cfg, nil)
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

	_, err := agent.NewRegistry(cfg, nil)
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

	registry, err := agent.NewRegistry(cfg, nil)
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

	// Depuis la Phase 8, "main" déclare des Delegates (voir
	// testRegistryConfig) : il est donc construit comme un
	// *agent.OrchestratorAgent, pas un *agent.GenAIAgent brut (voir
	// NewRegistry). "agenda" n'a pas de Delegates, il reste un GenAIAgent
	// simple.
	if _, ok := mainAgent.(*agent.OrchestratorAgent); !ok {
		t.Fatalf("mainAgent devrait être un *agent.OrchestratorAgent, obtenu %T", mainAgent)
	}
	if _, ok := agendaAgent.(*agent.GenAIAgent); !ok {
		t.Fatalf("agendaAgent devrait être un *agent.GenAIAgent, obtenu %T", agendaAgent)
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

func TestBuildLLMClient_WrapsResilienceMiddlewares(t *testing.T) {
	client, err := agent.BuildLLMClient(context.Background(), config.LLMClient{
		Provider: "openai",
		Model:    "gpt-test",
		APIKey:   "sk-test",
		BaseURL:  "https://api.example.test",
	})
	if err != nil {
		t.Fatalf("BuildLLMClient: %v", err)
	}

	// Le décorateur de comptabilité d'usage est le plus externe (une trace
	// par appel réussi, après retry, voir internal/usage) ; juste en
	// dessous, le circuit breaker enveloppe le retry (voir wrapResilience) :
	// sans lui, un 429 ou un 5xx transitoire du fournisseur ferait échouer
	// le tour entier.
	unwrapper, ok := client.(interface{ Unwrap() llm.Client })
	if !ok {
		t.Fatalf("client construit de type %T, attendu le décorateur d'usage (comptabilité non câblée)", client)
	}
	inner := unwrapper.Unwrap()
	if _, ok := inner.(*circuitbreaker.Client); !ok {
		t.Errorf("client enveloppé de type %T, attendu *circuitbreaker.Client (résilience non câblée)", inner)
	}
}

func TestNewRegistry_OrchestratorWithoutDelegatesKeepsOwnTools(t *testing.T) {
	cfg := testRegistryConfig()
	main := cfg.Agents["main"]
	main.Delegates = nil
	main.Reminders = true
	cfg.Agents["main"] = main

	registry, err := agent.NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	a, err := registry.Get("main")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Régression : un orchestrateur sans aucun délégué (spécialistes
	// commentés en configuration) était construit comme simple GenAIAgent,
	// perdant silencieusement mémoire et rappels malgré ses drapeaux.
	if _, ok := a.(*agent.OrchestratorAgent); !ok {
		t.Fatalf("agent main de type %T, attendu *agent.OrchestratorAgent (un orchestrateur sans délégué garde ses outils propres)", a)
	}
}
