package scheduler_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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
	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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
	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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

	s := scheduler.NewScheduler(cfg, clock, db, registry, senders, nil)

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
