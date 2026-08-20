package agent_test

import (
	"context"
	"testing"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// L'identité reconstruite pour une tâche est celle de son créateur, et son
// déclencheur la distingue d'un cron de configuration — c'est ce qui lui
// donne accès aux données de la personne qui l'a demandée.
func TestTaskRunner_IdentityIsTheCreatorScheduledTask(t *testing.T) {
	cfg := &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		LLMClients: map[string]config.LLMClient{
			"main": {Provider: "openai", Model: "gpt-test", APIKey: "sk-test", BaseURL: "https://api.example.test/v1"},
		},
		Agents: map[string]config.Agent{
			"main": {Type: config.AgentTypeOrchestrator, Client: "main", SystemPrompt: config.SystemPrompt{Content: "généraliste"}},
		},
		Channels: []config.Channel{
			{Provider: "whatsapp", ChannelID: "group-chan", Kind: config.ChannelKindGroup, OrgID: "home", Scope: "group", ScopeID: "famille"},
		},
	}

	registry, err := agent.NewRegistryWithMemory(cfg, agent.MemoryTools{}, agent.ReminderTools{}, agent.ProfileTools{}, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewRegistryWithMemory: %v", err)
	}

	runner := agent.NewTaskRunner(cfg, registry, nil)

	// L'agent n'est pas exécuté ici : seule la construction de l'identité est
	// vérifiée, via l'agent introuvable qui court-circuite avant tout appel.
	_, err = runner.RunTask(context.Background(), persistence.Reminder{
		AgentID: "inconnu", PrincipalID: "alice", OrgID: "home",
		Provider: "whatsapp", ChannelID: "group-chan",
	})
	if err == nil {
		t.Fatal("un agent inconnu doit faire échouer la tâche")
	}

	identity, conversation := agent.ExportedBuildTaskIdentity(runner, persistence.Reminder{
		PrincipalID: "alice", OrgID: "home",
		ConversationID: "whatsapp:group-chan",
		Provider:       "whatsapp", ChannelID: "group-chan",
	})

	if identity.Trigger != model.TriggerScheduledTask {
		t.Errorf("trigger = %q, attendu %q", identity.Trigger, model.TriggerScheduledTask)
	}
	if identity.PrincipalID != "alice" {
		t.Errorf("principal = %q, attendu celui du créateur", identity.PrincipalID)
	}
	// La portée vient de la configuration du canal, relue à l'exécution.
	if identity.Scope != model.ScopeGroup || identity.ScopeID != "famille" {
		t.Errorf("portée = (%q, %q), attendu (group, famille)", identity.Scope, identity.ScopeID)
	}
	if identity.ChannelKind != model.ChannelGroup {
		t.Errorf("type de canal = %q, attendu group", identity.ChannelKind)
	}
	if conversation.Scope != identity.Scope || conversation.ScopeID != identity.ScopeID {
		t.Error("la conversation doit porter la même portée que l'identité")
	}
}
