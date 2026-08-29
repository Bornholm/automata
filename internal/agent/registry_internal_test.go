package agent

import (
	"testing"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
)

// Un orchestrateur capture ses délégués tels qu'ils existent au moment où il
// est construit. Tant que spécialistes et orchestrateurs étaient construits
// dans la même boucle sur cfg.Agents, le résultat dépendait de l'ordre
// d'itération de la map, aléatoire en Go : un démarrage sur deux, la
// délégation atteignait l'agent nu au lieu du spécialiste équipé de ses
// outils MCP. La panne était invisible — pas d'erreur, juste un spécialiste
// qui répond de mémoire au lieu de chercher.
func TestNewRegistry_DelegatesReachMCPEquippedSpecialist(t *testing.T) {
	cfg := &config.Config{
		Version:      1,
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "streamable-http", URL: "https://mcp.example.test/mcp"},
		},
		Agents: map[string]config.Agent{
			"main": {
				Type:         config.AgentTypeOrchestrator,
				SystemPrompt: config.SystemPrompt{Content: "généraliste"},
				Delegates:    []string{"research"},
			},
			"research": {
				Type:         config.AgentTypeSpecialist,
				SystemPrompt: config.SystemPrompt{Content: "spécialiste"},
				MCPServers:   []string{"internet-search"},
			},
		},
	}

	manager := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = manager.Close() })

	// Répété : un seul passage pourrait tomber sur le bon ordre par chance.
	for i := 0; i < 20; i++ {
		registry, err := NewRegistryWithMemory(cfg, MemoryTools{}, ReminderTools{}, ProfileTools{}, nil, manager, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("NewRegistryWithMemory: %v", err)
		}

		main, err := registry.Get("main")
		if err != nil {
			t.Fatalf("Get(main): %v", err)
		}

		orchestrator, ok := main.(*OrchestratorAgent)
		if !ok {
			t.Fatalf("main est un %T, attendu *OrchestratorAgent", main)
		}

		specialist, ok := orchestrator.specialists["research"]
		if !ok {
			t.Fatal("le délégué research est absent de l'orchestrateur")
		}

		agentSpecialist, ok := specialist.(*AgentSpecialist)
		if !ok {
			t.Fatalf("le délégué est un %T, attendu *AgentSpecialist", specialist)
		}

		if _, ok := agentSpecialist.agent.(*MCPToolAgent); !ok {
			t.Fatalf("itération %d: la délégation cible un %T, attendu *MCPToolAgent (spécialiste sans ses outils MCP)", i, agentSpecialist.agent)
		}
	}
}
