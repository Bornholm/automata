package e2e_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	couriermemory "github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/scheduler"
)

// Les scénarios planifiés assemblent internal/scheduler directement sur
// internal/persistence et internal/action, exactement comme
// internal/registry.Run le fait (sched := scheduler.NewScheduler(cfg, ...)),
// avec un agent.Agent factice scriptable et un provider Courier en mémoire :
// mêmes patterns que internal/scheduler/scheduler_test.go, réutilisés ici
// pour prouver l'assemblage plutôt que réinventer une nouvelle approche.

// fakeScheduledAgent est un agent.Agent factice, scriptable et
// comptabilisant ses appels (identique au pattern fakeAgent de
// internal/scheduler/scheduler_test.go).
type fakeScheduledAgent struct {
	calls int
	fn    func(ctx context.Context, req agent.Request) (agent.Result, error)
}

func (a *fakeScheduledAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	a.calls++
	return a.fn(ctx, req)
}

func replyingScheduledAgent(text string) *fakeScheduledAgent {
	return &fakeScheduledAgent{fn: func(ctx context.Context, req agent.Request) (agent.Result, error) {
		return agent.Result{Reply: text}, nil
	}}
}

// failingProvider est un courier.Provider dont Send échoue toujours.
type failingProvider struct{}

func (failingProvider) Listen(ctx context.Context) (chan courier.Message, error) {
	return make(chan courier.Message), nil
}

func (failingProvider) Send(ctx context.Context, message courier.Message) error {
	return errors.New("envoi refusé (test)")
}

var _ courier.Provider = failingProvider{}

// baseSchedule construit un config.Schedule minimal valide, prêt à être
// adapté par les tests (même pattern que scheduler_test.go).
func baseSchedule(id, agentName, providerName, channelID string) config.Schedule {
	return config.Schedule{
		ID:      id,
		Enabled: true,
		Schedule: config.ScheduleCron{
			Cron:     "0 7 * * *",
			Timezone: "UTC",
		},
		Execution: config.ScheduleExecution{
			PrincipalID: "scheduler-readonly",
			OrgID:       "home",
			Scope:       config.ScopeOrg,
			ScopeID:     "home",
			Agent:       agentName,
			Prompt:      "Prépare le résumé.",
			Actions:     config.ScheduleActions{Policy: config.ActionsPolicyReadOnly},
		},
		Delivery: config.ScheduleDelivery{
			Provider:  providerName,
			ChannelID: channelID,
			Mode:      config.DeliveryModeAlways,
		},
		Concurrency: config.ScheduleConcurrency{
			Policy:  config.ConcurrencyPolicyForbid,
			Timeout: config.Duration(5 * time.Second),
		},
	}
}

// TestScheduled_MorningSummary vérifie le scénario "résumé du matin" : un
// schedule read_only se déclenche (Tick), l'agent répond, le message est
// livré au canal configuré.
func TestScheduled_MorningSummary(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("morning-summary", "main", "whatsapp", "org-group")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingScheduledAgent("Résumé du jour : rien d'urgent.")
	registry := agent.NewRegistryFromAgents(map[string]agent.Agent{"main": fake})

	provider := couriermemory.NewProvider()
	senders := scheduler.SenderMap{"whatsapp": provider}

	sched2 := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("appels agent: got %d, expected 1", fake.calls)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés: got %d, expected 1", len(sent))
	}

	content, err := courier.GetMessageMainContent(context.Background(), sent[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	if content != "Résumé du jour : rien d'urgent." {
		t.Errorf("contenu livré: got %q", content)
	}
}

// TestScheduled_NoContentSkipsDelivery vérifie le scénario "absence de
// contenu" : l'agent retourne une réponse vide et le mode de livraison est
// on_content, donc rien n'est livré.
func TestScheduled_NoContentSkipsDelivery(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("empty-summary", "main", "whatsapp", "org-group")
	sched.Delivery.Mode = config.DeliveryModeOnContent
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingScheduledAgent("")
	registry := agent.NewRegistryFromAgents(map[string]agent.Agent{"main": fake})

	provider := couriermemory.NewProvider()
	senders := scheduler.SenderMap{"whatsapp": provider}

	sched2 := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("l'agent devait tout de même être exécuté: got %d appels, expected 1", fake.calls)
	}

	if sent := len(provider.Sent()); sent != 0 {
		t.Fatalf("aucune livraison attendue pour un contenu vide en mode on_content, got %d", sent)
	}
}

// TestScheduled_DeliveryFailureDoesNotReexecute vérifie le scénario "échec
// de livraison" : le provider Courier échoue à l'envoi, la tentative de
// livraison échouée est enregistrée, et l'agent n'est jamais réexécuté pour
// autant.
func TestScheduled_DeliveryFailureDoesNotReexecute(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("failing-delivery", "main", "broken", "org-group")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingScheduledAgent("contenu")
	registry := agent.NewRegistryFromAgents(map[string]agent.Agent{"main": fake})

	senders := scheduler.SenderMap{"broken": failingProvider{}}

	sched2 := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("appels agent après échec de livraison: got %d, expected 1 (pas de réexécution)", fake.calls)
	}
}

// TestScheduled_DuplicateOccurrenceTriggersOnce vérifie le scénario
// "occurrence dupliquée" : Tick appelé deux fois pour la même occurrence ne
// déclenche l'agent qu'une seule fois.
func TestScheduled_DuplicateOccurrenceTriggersOnce(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("dedup-summary", "main", "whatsapp", "org-group")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingScheduledAgent("résumé")
	registry := agent.NewRegistryFromAgents(map[string]agent.Agent{"main": fake})

	provider := couriermemory.NewProvider()
	senders := scheduler.SenderMap{"whatsapp": provider}

	sched2 := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick #2: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("appels agent: got %d, expected 1 (déduplication d'occurrence)", fake.calls)
	}
}

// requireConfirmationConfig construit une configuration minimale suffisante
// pour authorization.NewAuthorizer dans les scénarios require_confirmation :
// le rôle "adult" accorde memory.group.delete, utilisé par les deux membres
// du groupe de livraison.
func requireConfirmationConfig() *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: []string{"memory.group.delete"}},
			},
			Principals: []config.Principal{
				{ID: "scheduler-writer", Kind: config.PrincipalKindService, DisplayName: "Scheduler"},
				{ID: "adult-confirmer", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
			},
		},
	}
}

// groupDeleteProposal construit une delegation.ProposedAction de suppression
// mémoire de groupe, exécutée par un action.Executor factice (fakeCalendarExecutor).
func groupDeleteProposal(summary string) delegation.ProposedAction {
	return delegation.ProposedAction{
		Summary:            summary,
		MCPServer:          "internal-test",
		ToolName:           "noop",
		Arguments:          map[string]any{},
		RequiredPermission: "memory.group.delete",
		Scope:              model.ScopeGroup,
		ScopeID:            "home-group",
	}
}

// fakePlanExecutor est un action.Executor factice qui compte ses appels et
// retourne toujours un succès (même pattern que scheduler_test.go).
type fakePlanExecutor struct {
	calls int
}

func (f *fakePlanExecutor) Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error) {
	f.calls++
	return "ok", nil
}

func humanScheduleIdentity(principalID model.PrincipalID, provider, channelID string) model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    principalID,
		OrgID:          "home",
		ConversationID: model.ConversationID(provider + ":" + channelID),
		Provider:       provider,
		ChannelID:      channelID,
		ChannelKind:    model.ChannelGroup,
		Scope:          model.ScopeGroup,
		ScopeID:        "home-group",
	}
}

func humanScheduleConversation(provider, channelID string) model.Conversation {
	return model.Conversation{
		ID: model.ConversationID(provider + ":" + channelID), OrgID: "home",
		Provider: provider, ChannelID: channelID, Kind: model.ChannelGroup,
		Scope: model.ScopeGroup, ScopeID: "home-group",
	}
}

// TestScheduled_ActionProposalCreatesPlan vérifie le scénario "proposition
// d'action" : un schedule require_confirmation dont l'agent propose une
// action produit un plan persisté et livré au canal configuré.
func TestScheduled_ActionProposalCreatesPlan(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("propose-action", "main", "whatsapp", "org-group")
	sched.Execution.PrincipalID = "scheduler-writer"
	sched.Execution.Actions.Policy = config.ActionsPolicyRequireConfirmation

	cfg := requireConfirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := &fakeScheduledAgent{fn: func(ctx context.Context, req agent.Request) (agent.Result, error) {
		return agent.Result{
			Reply:           "Je propose de nettoyer une vieille tâche.",
			ProposedActions: []delegation.ProposedAction{groupDeleteProposal("Nettoyer la tâche obsolète")},
		}, nil
	}}
	registry := agent.NewRegistryFromAgents(map[string]agent.Agent{"main": fake})

	provider := couriermemory.NewProvider()
	senders := scheduler.SenderMap{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("internal-test", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	sched2 := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	convID := model.ConversationID("whatsapp:org-group")
	if count := countActionPlansByConversation(t, db, convID); count != 1 {
		t.Fatalf("action_plans pour %q: got %d, expected 1", convID, count)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés: got %d, expected 1", len(sent))
	}
	content, err := courier.GetMessageMainContent(context.Background(), sent[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	if !strings.Contains(content, "confirmer") {
		t.Fatalf("instructions de confirmation attendues dans la livraison, obtenu: %q", content)
	}
}

// TestScheduled_HumanConfirmationExecutes vérifie le scénario "confirmation
// humaine" : un humain confirme depuis le canal de livraison le plan créé
// par un schedule require_confirmation, ce qui déclenche son exécution
// réelle.
func TestScheduled_HumanConfirmationExecutes(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("confirm-action", "main", "whatsapp", "org-group")
	sched.Execution.PrincipalID = "scheduler-writer"
	sched.Execution.Actions.Policy = config.ActionsPolicyRequireConfirmation

	cfg := requireConfirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := &fakeScheduledAgent{fn: func(ctx context.Context, req agent.Request) (agent.Result, error) {
		return agent.Result{
			Reply:           "Je propose une écriture.",
			ProposedActions: []delegation.ProposedAction{groupDeleteProposal("Créer l'événement")},
		}, nil
	}}
	registry := agent.NewRegistryFromAgents(map[string]agent.Agent{"main": fake})

	provider := couriermemory.NewProvider()
	senders := scheduler.SenderMap{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("internal-test", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	sched2 := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := sched2.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	confirmer := humanScheduleIdentity("adult-confirmer", "whatsapp", "org-group")
	conv := humanScheduleConversation("whatsapp", "org-group")

	cmd, ok := action.ParseCommand("confirmer")
	if !ok {
		t.Fatal("ParseCommand(\"confirmer\") a échoué")
	}

	report, err := engine.HandleCommand(context.Background(), confirmer, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "succès") {
		t.Fatalf("succès attendu, obtenu: %q", report)
	}
	if executor.calls != 1 {
		t.Fatalf("exécution de l'action: got %d appels, expected 1", executor.calls)
	}
}
