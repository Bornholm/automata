package scheduler_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/scheduler"
)

// --- fixtures ---------------------------------------------------------

func testStorageConfig(t *testing.T) config.StorageApplication {
	t.Helper()
	return config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}
}

func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(context.Background(), testStorageConfig(t))
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeClock est une horloge contrôlable manuellement par les tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// fakeAgent est un agent.Agent factice, scriptable et comptabilisant ses
// appels.
type fakeAgent struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, req agent.Request) (agent.Result, error)

	lastIdentity model.ExecutionIdentity
}

func newFakeAgent(fn func(ctx context.Context, req agent.Request) (agent.Result, error)) *fakeAgent {
	return &fakeAgent{fn: fn}
}

func (a *fakeAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	a.mu.Lock()
	a.calls++
	a.lastIdentity = req.Identity
	a.mu.Unlock()

	return a.fn(ctx, req)
}

func (a *fakeAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func replyingAgent(text string) *fakeAgent {
	return newFakeAgent(func(ctx context.Context, req agent.Request) (agent.Result, error) {
		return agent.Result{Reply: text}, nil
	})
}

func blockingAgent() *fakeAgent {
	return newFakeAgent(func(ctx context.Context, req agent.Request) (agent.Result, error) {
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	})
}

// failingProvider est un courier.Provider dont Send échoue toujours,
// utilisé pour tester la séparation exécution/livraison.
type failingProvider struct{}

func (failingProvider) Listen(ctx context.Context) (chan courier.Message, error) {
	return make(chan courier.Message), nil
}

func (failingProvider) Send(ctx context.Context, message courier.Message) error {
	return errors.New("envoi refusé (test)")
}

var _ courier.Provider = failingProvider{}

// baseSchedule construit un config.Schedule minimal valide, en lecture
// seule, prêt à être adapté par les tests.
func baseSchedule(id, cronExpr, timezone, agentName, providerName string) config.Schedule {
	return config.Schedule{
		ID:      id,
		Enabled: true,
		Schedule: config.ScheduleCron{
			Cron:     cronExpr,
			Timezone: timezone,
		},
		Execution: config.ScheduleExecution{
			PrincipalID: "scheduler-readonly",
			OrgID:       "home",
			Scope:       config.ScopeOrg,
			ScopeID:     "home",
			Agent:       agentName,
			Prompt:      "Prépare le résumé.",
			Actions: config.ScheduleActions{
				Policy: config.ActionsPolicyReadOnly,
			},
		},
		Delivery: config.ScheduleDelivery{
			Provider:  providerName,
			ChannelID: "org-group",
			Mode:      config.DeliveryModeAlways,
		},
		Concurrency: config.ScheduleConcurrency{
			Policy:  config.ConcurrencyPolicyForbid,
			Timeout: config.Duration(5 * time.Second),
		},
	}
}

func newRegistry(agents map[string]agent.Agent) *agent.Registry {
	return agent.NewRegistryFromAgents(agents)
}

// proposingAgent est un agent factice qui répond reply et propose proposed
// (PLAN.md §17).
func proposingAgent(reply string, proposed []delegation.ProposedAction) *fakeAgent {
	return newFakeAgent(func(ctx context.Context, req agent.Request) (agent.Result, error) {
		return agent.Result{Reply: reply, ProposedActions: proposed}, nil
	})
}

// confirmationConfig construit une configuration minimale (organisation,
// rôles, principaux) suffisante pour authorization.NewAuthorizer dans les
// tests require_confirmation : le rôle "adult" accorde calendar.org.write,
// le rôle "child" ne l'accorde pas.
func confirmationConfig() *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: []string{"calendar.org.write"}},
				"child": {Permissions: []string{}},
			},
			Principals: []config.Principal{
				{ID: "scheduler-writer", Kind: config.PrincipalKindService, DisplayName: "Scheduler", Roles: nil},
				{ID: "adult-confirmer", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
				{ID: "child-confirmer", Kind: config.PrincipalKindHuman, DisplayName: "Enfant", Roles: []string{"child"}},
			},
		},
	}
}

// requireConfirmationSchedule construit un schedule config.Schedule dont la
// politique d'actions est require_confirmation, livré sur un canal de
// groupe (provider/channelID donnés), pour que le confirmateur humain n'ait
// pas besoin d'être l'auteur du plan (PLAN.md §10.5 authorizeConfirmer :
// seule la portée personal restreint au créateur).
func requireConfirmationSchedule(id, cronExpr, timezone, agentName, providerName, channelID string) config.Schedule {
	sched := baseSchedule(id, cronExpr, timezone, agentName, providerName)
	sched.Execution.PrincipalID = "scheduler-writer"
	sched.Execution.Actions.Policy = config.ActionsPolicyRequireConfirmation
	sched.Delivery.ChannelID = channelID
	return sched
}

// calendarWriteProposal construit une delegation.ProposedAction d'écriture
// calendrier org, exécutée par l'exécuteur mcpServer/toolName (fake dans les
// tests).
func calendarWriteProposal(summary, mcpServer, toolName string) delegation.ProposedAction {
	return delegation.ProposedAction{
		Summary:            summary,
		MCPServer:          mcpServer,
		ToolName:           toolName,
		Arguments:          map[string]any{},
		RequiredPermission: "calendar.org.write",
		Scope:              model.ScopeOrg,
		ScopeID:            "home",
	}
}

// humanIdentity construit l'identité d'exécution d'un humain confirmant
// dans le canal de groupe provider/channelID, avec le même ConversationID
// que celui que buildIdentity du scheduler calcule pour ce même canal
// (provider + ":" + channelID) — reproduisant ce que
// internal/identity.Resolver.ResolveMessage calculerait pour un message
// entrant réel sur ce canal.
func humanIdentity(principalID model.PrincipalID, provider, channelID string) model.ExecutionIdentity {
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

func humanConversation(provider, channelID string) model.Conversation {
	return model.Conversation{
		ID:        model.ConversationID(provider + ":" + channelID),
		OrgID:     "home",
		Provider:  provider,
		ChannelID: channelID,
		Kind:      model.ChannelGroup,
		Scope:     model.ScopeGroup,
		ScopeID:   "home-group",
	}
}

// fakePlanExecutor est un action.Executor factice qui compte ses appels et
// retourne toujours un succès.
type fakePlanExecutor struct {
	mu    sync.Mutex
	calls int
}

func (f *fakePlanExecutor) Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return "ok", nil
}

func (f *fakePlanExecutor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// --- tests --------------------------------------------------------------

func TestScheduler_Tick_Triggers(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("morning-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("Résumé du jour")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := fake.callCount(); got != 1 {
		t.Fatalf("agent calls: got %d, expected 1", got)
	}

	run, found := findLatestRun(t, db, sched.ID)
	if !found {
		t.Fatalf("scheduled_run: aucun enregistrement trouvé")
	}
	if run.Status != scheduler.StatusSucceeded {
		t.Errorf("scheduled_run.status: got %q, expected %q", run.Status, scheduler.StatusSucceeded)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés: got %d, expected 1", len(sent))
	}
	content, err := courier.GetMessageMainContent(context.Background(), sent[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	if content != "Résumé du jour" {
		t.Errorf("contenu livré: got %q, expected %q", content, "Résumé du jour")
	}
}

func TestScheduler_Tick_Disabled(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("disabled-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	sched.Enabled = false
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("ne devrait jamais être livré")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	for i := 0; i < 3; i++ {
		if err := s.Tick(context.Background(), at.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}

	if got := fake.callCount(); got != 0 {
		t.Fatalf("agent calls: got %d, expected 0 (schedule désactivé)", got)
	}
}

func TestScheduler_Tick_Timezones(t *testing.T) {
	db := openTestDB(t)

	parisLoc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("LoadLocation Europe/Paris: %v", err)
	}
	tokyoLoc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation Asia/Tokyo: %v", err)
	}

	parisAt := time.Date(2024, 6, 3, 9, 0, 0, 0, parisLoc)
	tokyoAt := time.Date(2024, 6, 3, 9, 0, 0, 0, tokyoLoc)

	if parisAt.UTC().Equal(tokyoAt.UTC()) {
		t.Fatalf("fixture invalide : les deux occurrences UTC ne devraient pas coïncider")
	}

	schedParis := baseSchedule("tz-paris", "0 9 * * *", "Europe/Paris", "paris-agent", "whatsapp")
	schedTokyo := baseSchedule("tz-tokyo", "0 9 * * *", "Asia/Tokyo", "tokyo-agent", "whatsapp")
	cfg := &config.Config{Schedules: []config.Schedule{schedParis, schedTokyo}}

	parisAgent := replyingAgent("paris")
	tokyoAgent := replyingAgent("tokyo")
	registry := newRegistry(map[string]agent.Agent{"paris-agent": parisAgent, "tokyo-agent": tokyoAgent})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	clock := newFakeClock(parisAt.UTC())
	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), parisAt.UTC()); err != nil {
		t.Fatalf("Tick (paris instant): %v", err)
	}

	if got := parisAgent.callCount(); got != 1 {
		t.Fatalf("paris agent calls après l'instant paris: got %d, expected 1", got)
	}
	if got := tokyoAgent.callCount(); got != 0 {
		t.Fatalf("tokyo agent calls après l'instant paris: got %d, expected 0", got)
	}

	clock.Set(tokyoAt.UTC())
	if err := s.Tick(context.Background(), tokyoAt.UTC()); err != nil {
		t.Fatalf("Tick (tokyo instant): %v", err)
	}

	if got := tokyoAgent.callCount(); got != 1 {
		t.Fatalf("tokyo agent calls après l'instant tokyo: got %d, expected 1", got)
	}
}

func TestScheduler_Tick_DaylightSavingTransition(t *testing.T) {
	db := openTestDB(t)

	parisLoc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("LoadLocation Europe/Paris: %v", err)
	}

	// 2024-03-31 : passage à l'heure d'été en Europe (CET -> CEST).
	beforeDST := time.Date(2024, 3, 30, 10, 0, 0, 0, parisLoc)
	afterDST := time.Date(2024, 3, 31, 10, 0, 0, 0, parisLoc)

	if beforeDST.UTC().Hour() == afterDST.UTC().Hour() {
		t.Fatalf("fixture invalide : les heures UTC devraient différer de part et d'autre du changement d'heure")
	}

	sched := baseSchedule("dst-summary", "0 10 * * *", "Europe/Paris", "main", "whatsapp")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("dst")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	clock := newFakeClock(beforeDST.UTC())
	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), beforeDST.UTC()); err != nil {
		t.Fatalf("Tick avant DST: %v", err)
	}
	if got := fake.callCount(); got != 1 {
		t.Fatalf("appels après l'occurrence avant DST: got %d, expected 1", got)
	}

	clock.Set(afterDST.UTC())
	if err := s.Tick(context.Background(), afterDST.UTC()); err != nil {
		t.Fatalf("Tick après DST: %v", err)
	}
	if got := fake.callCount(); got != 2 {
		t.Fatalf("appels après l'occurrence après DST: got %d, expected 2 (ni doublée ni sautée)", got)
	}
}

func TestScheduler_Tick_DuplicateOccurrence(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("dedup-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("résumé")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick #2: %v", err)
	}

	if got := fake.callCount(); got != 1 {
		t.Fatalf("agent calls: got %d, expected 1 (déduplication)", got)
	}

	count := countRuns(t, db, sched.ID)
	if count != 1 {
		t.Fatalf("scheduled_runs pour %q: got %d, expected 1", sched.ID, count)
	}
}

func TestScheduler_Tick_ConcurrencyForbid(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("forbid-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("résumé")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	// Simule une exécution déjà en cours pour une occurrence antérieure.
	runningFor := at.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	insertRunningScheduledRun(t, db, sched.ID, runningFor)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := fake.callCount(); got != 0 {
		t.Fatalf("agent calls avec exécution en cours: got %d, expected 0", got)
	}

	// La précédente exécution se termine : l'occurrence due doit maintenant
	// être déclenchée.
	markRunSucceeded(t, db, sched.ID, runningFor)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick après libération: %v", err)
	}

	if got := fake.callCount(); got != 1 {
		t.Fatalf("agent calls après libération: got %d, expected 1", got)
	}
}

func TestScheduler_Tick_Timeout(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("timeout-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	sched.Concurrency.Timeout = config.Duration(50 * time.Millisecond)
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := blockingAgent()
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	done := make(chan error, 1)
	go func() {
		done <- s.Tick(context.Background(), at)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Tick n'est pas revenu après le timeout de l'agent : blocage du tick global")
	}

	run, found := findLatestRun(t, db, sched.ID)
	if !found {
		t.Fatalf("scheduled_run: aucun enregistrement trouvé")
	}
	if run.Status != scheduler.StatusFailed {
		t.Errorf("scheduled_run.status: got %q, expected %q", run.Status, scheduler.StatusFailed)
	}
}

func TestScheduler_Tick_MinimalPermissionsIdentity(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("readonly-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	sched.Execution.PrincipalID = "scheduled-reader"
	sched.Execution.Scope = config.ScopeOrg
	sched.Execution.ScopeID = "home"
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("résumé")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// L'identité construite provient exclusivement de la configuration de
	// confiance (schedule.Execution) : aucune vérification supplémentaire
	// via internal/authorization n'est nécessaire ici, puisque le
	// principal, la portée et son identifiant sont fixés par l'opérateur
	// dans schedules[].execution, jamais dérivés d'un contenu utilisateur
	// ou LLM (voir PLAN.md §11.2, §3.2).
	if fake.lastIdentity.Trigger != model.TriggerCron {
		t.Errorf("identity.Trigger: got %q, expected %q", fake.lastIdentity.Trigger, model.TriggerCron)
	}
	if fake.lastIdentity.PrincipalID != "scheduled-reader" {
		t.Errorf("identity.PrincipalID: got %q, expected %q", fake.lastIdentity.PrincipalID, "scheduled-reader")
	}
	if fake.lastIdentity.Scope != model.ScopeOrg {
		t.Errorf("identity.Scope: got %q, expected %q", fake.lastIdentity.Scope, model.ScopeOrg)
	}
	if fake.lastIdentity.ScopeID != "home" {
		t.Errorf("identity.ScopeID: got %q, expected %q", fake.lastIdentity.ScopeID, "home")
	}
}

func TestScheduler_Delivery(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("delivery-summary", "0 7 * * *", "UTC", "main", "whatsapp")
	sched.Delivery.Mode = config.DeliveryModeOnContent
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("contenu livré")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés: got %d, expected 1", len(sent))
	}
	if string(sent[0].Channel().ChannelID()) != sched.Delivery.ChannelID {
		t.Errorf("channel: got %q, expected %q", sent[0].Channel().ChannelID(), sched.Delivery.ChannelID)
	}

	run, found := findLatestRun(t, db, sched.ID)
	if !found {
		t.Fatalf("scheduled_run introuvable")
	}
	if run.DeliveryStatus == nil || *run.DeliveryStatus != scheduler.DeliveryStatusSucceeded {
		t.Errorf("delivery_status: got %v, expected %q", run.DeliveryStatus, scheduler.DeliveryStatusSucceeded)
	}

	attempts := countDeliveryAttempts(t, db, run.ID)
	if attempts != 1 {
		t.Fatalf("delivery_attempts: got %d, expected 1", attempts)
	}
}

func TestScheduler_Delivery_FailureDoesNotReexecute(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := baseSchedule("delivery-failure-summary", "0 7 * * *", "UTC", "main", "broken")
	cfg := &config.Config{Schedules: []config.Schedule{sched}}

	fake := replyingAgent("contenu")
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	senders := map[string]courier.Provider{"broken": failingProvider{}}

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := fake.callCount(); got != 1 {
		t.Fatalf("agent calls après échec de livraison: got %d, expected 1", got)
	}

	run, found := findLatestRun(t, db, sched.ID)
	if !found {
		t.Fatalf("scheduled_run introuvable")
	}
	if run.DeliveryStatus == nil || *run.DeliveryStatus != scheduler.DeliveryStatusFailed {
		t.Errorf("delivery_status: got %v, expected %q", run.DeliveryStatus, scheduler.DeliveryStatusFailed)
	}
	if run.Status != scheduler.StatusSucceeded {
		t.Errorf("scheduled_run.status: got %q, expected %q (l'échec de livraison ne doit pas affecter le statut d'exécution)", run.Status, scheduler.StatusSucceeded)
	}

	if err := s.RetryDelivery(context.Background(), sched, run.ID, "contenu"); err != nil {
		t.Fatalf("RetryDelivery: %v", err)
	}

	if got := fake.callCount(); got != 1 {
		t.Fatalf("agent calls après nouvelle tentative de livraison: got %d, expected 1 (pas de réexécution)", got)
	}

	attempts := countDeliveryAttempts(t, db, run.ID)
	if attempts != 2 {
		t.Fatalf("delivery_attempts après nouvelle tentative: got %d, expected 2", attempts)
	}
}

// --- helpers de vérification directe en base ---------------------------

func findLatestRun(t *testing.T, db *persistence.DB, scheduleID string) (persistence.ScheduledRun, bool) {
	t.Helper()

	var (
		run   persistence.ScheduledRun
		found bool
	)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		run, found, err = persistence.NewScheduledRunRepository().FindLatestByScheduleID(context.Background(), tx, scheduleID)
		return err
	})
	if err != nil {
		t.Fatalf("FindLatestByScheduleID: %v", err)
	}

	return run, found
}

func countRuns(t *testing.T, db *persistence.DB, scheduleID string) int {
	t.Helper()

	var count int
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		row := tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM scheduled_runs WHERE schedule_id = ?`, scheduleID)
		return row.Scan(&count)
	})
	if err != nil {
		t.Fatalf("count scheduled_runs: %v", err)
	}

	return count
}

func countDeliveryAttempts(t *testing.T, db *persistence.DB, runID persistence.ScheduledRunID) int {
	t.Helper()

	var count int
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		n, err := persistence.NewDeliveryAttemptRepository().CountByScheduledRunID(context.Background(), tx, runID)
		count = n
		return err
	})
	if err != nil {
		t.Fatalf("count delivery_attempts: %v", err)
	}

	return count
}

func insertRunningScheduledRun(t *testing.T, db *persistence.DB, scheduleID, scheduledFor string) {
	t.Helper()

	now := time.Now().UTC().Format(time.RFC3339)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewScheduledRunRepository().Insert(context.Background(), tx, persistence.ScheduledRun{
			ID:           persistence.ScheduledRunID("running-" + scheduledFor),
			ScheduleID:   scheduleID,
			ScheduledFor: scheduledFor,
			StartedAt:    &now,
			Status:       scheduler.StatusRunning,
			PrincipalID:  "scheduler-readonly",
			OrgID:        "home",
			Scope:        model.ScopeOrg,
			ScopeID:      "home",
			AgentID:      "main",
			CreatedAt:    now,
		})
	})
	if err != nil {
		t.Fatalf("insertion d'une exécution en cours: %v", err)
	}
}

func markRunSucceeded(t *testing.T, db *persistence.DB, scheduleID, scheduledFor string) {
	t.Helper()

	completedAt := time.Now().UTC().Format(time.RFC3339)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		run, found, err := persistence.NewScheduledRunRepository().FindByScheduleAndScheduledFor(context.Background(), tx, scheduleID, scheduledFor)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("exécution planifiée introuvable pour %q à %q", scheduleID, scheduledFor)
		}
		return persistence.NewScheduledRunRepository().UpdateStatus(context.Background(), tx, run.ID, scheduler.StatusSucceeded, &completedAt, nil)
	})
	if err != nil {
		t.Fatalf("clôture de l'exécution en cours: %v", err)
	}
}

func countActionPlansByConversation(t *testing.T, db *persistence.DB, conversationID model.ConversationID) int {
	t.Helper()

	var count int
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		row := tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM action_plans WHERE conversation_id = ?`, conversationID)
		return row.Scan(&count)
	})
	if err != nil {
		t.Fatalf("count action_plans: %v", err)
	}

	return count
}

func findAuditEventsByType(t *testing.T, db *persistence.DB, eventType string) []persistence.AuditEvent {
	t.Helper()

	var events []persistence.AuditEvent
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `
			SELECT id, org_id, principal_id, trigger, conversation_id, event_type, resource_kind,
				resource_scope, resource_scope_id, outcome, metadata_json, created_at
			FROM audit_events WHERE event_type = ?
		`, eventType)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e persistence.AuditEvent
			if err := rows.Scan(&e.ID, &e.OrgID, &e.PrincipalID, &e.Trigger, &e.ConversationID, &e.EventType,
				&e.ResourceKind, &e.ResourceScope, &e.ResourceScopeID, &e.Outcome, &e.MetadataJSON, &e.CreatedAt); err != nil {
				return err
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("recherche des événements d'audit %q: %v", eventType, err)
	}

	return events
}

// --- tests Phase 17 : tâches planifiées avec confirmation --------------

func TestScheduler_RequireConfirmation_ProposesPlan(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("propose-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose de créer un événement.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement anniversaire", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	run, found := findLatestRun(t, db, sched.ID)
	if !found {
		t.Fatalf("scheduled_run: aucun enregistrement trouvé")
	}
	if run.Status != scheduler.StatusSucceeded {
		t.Fatalf("scheduled_run.status: got %q, expected %q", run.Status, scheduler.StatusSucceeded)
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
	if !strings.Contains(content, "Créer l'événement anniversaire") {
		t.Fatalf("texte de proposition attendu dans la livraison, obtenu: %q", content)
	}
	if !strings.Contains(content, "confirmer") {
		t.Fatalf("instructions de confirmation attendues dans la livraison, obtenu: %q", content)
	}
}

func TestScheduler_RequireConfirmation_ConfirmByAuthorizedHuman(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("confirm-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose une écriture.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	confirmer := humanIdentity("adult-confirmer", "whatsapp", "org-group")
	conv := humanConversation("whatsapp", "org-group")

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
	if got := executor.callCount(); got != 1 {
		t.Fatalf("exécution de l'action: got %d appels, expected 1", got)
	}
}

func TestScheduler_RequireConfirmation_InsufficientPermission(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("child-confirm-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose une écriture.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	confirmer := humanIdentity("child-confirmer", "whatsapp", "org-group")
	conv := humanConversation("whatsapp", "org-group")

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), confirmer, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "échec") {
		t.Fatalf("échec attendu (permission insuffisante), obtenu: %q", report)
	}
	if got := executor.callCount(); got != 0 {
		t.Fatalf("aucune exécution attendue (permission insuffisante), got %d appels", got)
	}
}

func TestScheduler_RequireConfirmation_Expiration(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("expire-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose une écriture.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
		action.WithPlanTTL(time.Minute),
		action.WithClock(clock.Now),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	clock.Set(at.Add(2 * time.Minute))

	confirmer := humanIdentity("adult-confirmer", "whatsapp", "org-group")
	conv := humanConversation("whatsapp", "org-group")

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), confirmer, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "expiré") {
		t.Fatalf("expiration attendue, obtenu: %q", report)
	}
	if got := executor.callCount(); got != 0 {
		t.Fatalf("aucune exécution attendue (plan expiré), got %d appels", got)
	}
}

func TestScheduler_RequireConfirmation_TickTwiceDoesNotDuplicatePlan(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("dedup-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose une écriture.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick #1: %v", err)
	}
	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick #2: %v", err)
	}

	if got := fake.callCount(); got != 1 {
		t.Fatalf("agent calls: got %d, expected 1 (déduplication d'occurrence)", got)
	}

	convID := model.ConversationID("whatsapp:org-group")
	if count := countActionPlansByConversation(t, db, convID); count != 1 {
		t.Fatalf("action_plans pour %q: got %d, expected 1 (pas de doublon)", convID, count)
	}
}

func TestScheduler_RequireConfirmation_RestartThenConfirm(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db, err := persistence.Open(context.Background(), storageCfg)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("restart-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose une écriture.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Simule un redémarrage du processus : fermeture de la connexion, puis
	// réouverture d'une nouvelle instance *persistence.DB sur le même
	// fichier, et construction d'un nouvel action.Engine dessus (même
	// pattern que TestEngine_RedemarrageAvantConfirmation, Phase 15).
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	db2, err := persistence.Open(context.Background(), storageCfg)
	if err != nil {
		t.Fatalf("persistence.Open (redémarrage): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	engine2 := action.NewEngine(db2, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	confirmer := humanIdentity("adult-confirmer", "whatsapp", "org-group")
	conv := humanConversation("whatsapp", "org-group")

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine2.HandleCommand(context.Background(), confirmer, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand après redémarrage: %v", err)
	}
	if !strings.Contains(report, "succès") {
		t.Fatalf("succès attendu après redémarrage, obtenu: %q", report)
	}
	if got := executor.callCount(); got != 1 {
		t.Fatalf("exécution attendue après redémarrage: got %d appels, expected 1", got)
	}
}

func TestScheduler_RequireConfirmation_Audit(t *testing.T) {
	db := openTestDB(t)

	at := time.Date(2024, 1, 2, 7, 0, 0, 0, time.UTC)
	clock := newFakeClock(at)

	sched := requireConfirmationSchedule("audit-write", "0 7 * * *", "UTC", "main", "whatsapp", "org-group")
	cfg := confirmationConfig()
	cfg.Schedules = []config.Schedule{sched}

	fake := proposingAgent("Je propose une écriture.", []delegation.ProposedAction{
		calendarWriteProposal("Créer l'événement", "calendar-server", "create_event"),
	})
	registry := newRegistry(map[string]agent.Agent{"main": fake})

	provider := memory.NewProvider()
	senders := map[string]courier.Provider{"whatsapp": provider}

	authorizer := authorization.NewAuthorizer(cfg)
	executor := &fakePlanExecutor{}
	engine := action.NewEngine(db, authorizer, nil, cfg,
		action.WithExecutor("calendar-server", executor),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	)

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, engine, nil)

	if err := s.Tick(context.Background(), at); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	proposedEvents := findAuditEventsByType(t, db, "action_plan.proposed")
	if len(proposedEvents) != 1 {
		t.Fatalf("audit_events action_plan.proposed: got %d, expected 1", len(proposedEvents))
	}
	if proposedEvents[0].PrincipalID != "scheduler-writer" {
		t.Errorf("audit action_plan.proposed principal_id: got %q, expected %q (auteur technique)", proposedEvents[0].PrincipalID, "scheduler-writer")
	}
	if proposedEvents[0].Trigger != model.TriggerCron {
		t.Errorf("audit action_plan.proposed trigger: got %q, expected %q", proposedEvents[0].Trigger, model.TriggerCron)
	}

	confirmer := humanIdentity("adult-confirmer", "whatsapp", "org-group")
	conv := humanConversation("whatsapp", "org-group")

	cmd, _ := action.ParseCommand("confirmer")
	if _, err := engine.HandleCommand(context.Background(), confirmer, conv, cmd); err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}

	confirmedEvents := findAuditEventsByType(t, db, "action_plan.confirmed")
	if len(confirmedEvents) != 1 {
		t.Fatalf("audit_events action_plan.confirmed: got %d, expected 1", len(confirmedEvents))
	}
	if confirmedEvents[0].PrincipalID != "adult-confirmer" {
		t.Errorf("audit action_plan.confirmed principal_id: got %q, expected %q (confirmateur humain)", confirmedEvents[0].PrincipalID, "adult-confirmer")
	}
	if confirmedEvents[0].Trigger != model.TriggerMessage {
		t.Errorf("audit action_plan.confirmed trigger: got %q, expected %q", confirmedEvents[0].Trigger, model.TriggerMessage)
	}
	if confirmedEvents[0].Outcome != action.StatusSucceeded {
		t.Errorf("audit action_plan.confirmed outcome: got %q, expected %q", confirmedEvents[0].Outcome, action.StatusSucceeded)
	}
}
