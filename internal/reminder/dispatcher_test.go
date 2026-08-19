package reminder_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/reminder"
)

var dispatcherTestNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

func testDB(t *testing.T) *persistence.DB {
	t.Helper()

	cfg := config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}

	db, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func insertReminder(t *testing.T, db *persistence.DB, rem persistence.Reminder) {
	t.Helper()

	repo := persistence.NewReminderRepository()
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, rem)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func reminderStatus(t *testing.T, db *persistence.DB, id persistence.ReminderID) string {
	t.Helper()

	repo := persistence.NewReminderRepository()
	var status string
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		rem, found, err := repo.FindByID(context.Background(), tx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("rappel %q introuvable", id)
		}
		status = rem.Status
		return nil
	})
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	return status
}

func reminderRow(t *testing.T, db *persistence.DB, id persistence.ReminderID) persistence.Reminder {
	t.Helper()

	repo := persistence.NewReminderRepository()
	var rem persistence.Reminder
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		r, found, err := repo.FindByID(context.Background(), tx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("rappel %q introuvable", id)
		}
		rem = r
		return nil
	})
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	return rem
}

func baseReminder(id, provider, fireAt string) persistence.Reminder {
	return persistence.Reminder{
		ID:             persistence.ReminderID(id),
		OrgID:          "home",
		PrincipalID:    "alice",
		ConversationID: "conv-alice",
		Provider:       provider,
		ChannelID:      "chan-alice",
		Message:        "sortir les poubelles",
		FireAt:         fireAt,
		Status:         persistence.ReminderStatusPending,
		CreatedAt:      "2026-08-17T10:00:00Z",
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDispatcher_DeliversDueReminder(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	insertReminder(t, db, baseReminder("due", "memory", "2026-08-17T11:59:00Z"))
	insertReminder(t, db, baseReminder("future", "memory", "2026-08-17T13:00:00Z"))

	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés = %d, attendu 1 (seul le rappel échu part)", len(sent))
	}

	content, err := courier.GetMessageMainContent(context.Background(), sent[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	if !strings.Contains(content, "sortir les poubelles") || !strings.Contains(content, "Rappel") {
		t.Errorf("contenu = %q, attendu le préfixe de rappel et le message", content)
	}

	if got := reminderStatus(t, db, "due"); got != persistence.ReminderStatusSent {
		t.Errorf("statut de 'due' = %q, attendu sent", got)
	}
	if got := reminderStatus(t, db, "future"); got != persistence.ReminderStatusPending {
		t.Errorf("statut de 'future' = %q, attendu pending", got)
	}
}

func TestDispatcher_UnknownProviderMarksFailed(t *testing.T) {
	db := testDB(t)

	insertReminder(t, db, baseReminder("orphan", "disparu", "2026-08-17T11:00:00Z"))

	d := reminder.NewDispatcher(db, reminder.SenderMap{}, testLogger(), nil).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := reminderStatus(t, db, "orphan"); got != persistence.ReminderStatusFailed {
		t.Errorf("statut = %q, attendu failed", got)
	}
}

func TestDispatcher_TickIsIdempotent(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	insertReminder(t, db, baseReminder("once", "memory", "2026-08-17T11:00:00Z"))

	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithClock(func() time.Time { return dispatcherTestNow })

	for range 2 {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}

	if sent := provider.Sent(); len(sent) != 1 {
		t.Errorf("messages envoyés après deux ticks = %d, attendu 1 (un rappel sent ne repart jamais)", len(sent))
	}
}

func TestDispatcher_RecurringReminderIsRearmed(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	// Échu, récurrent chaque mardi 20h Paris. dispatcherTestNow est le lundi
	// 17 août 2026, 12:00 UTC : la prochaine occurrence est le mardi 18 à
	// 20:00+02:00, soit 18:00Z.
	rem := baseReminder("weekly", "memory", "2026-08-17T11:00:00Z")
	rem.Recurrence = "0 20 * * 2"
	rem.Timezone = "Europe/Paris"
	insertReminder(t, db, rem)

	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if sent := provider.Sent(); len(sent) != 1 {
		t.Fatalf("messages envoyés = %d, attendu 1", len(sent))
	}

	repo := persistence.NewReminderRepository()
	var after persistence.Reminder
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var found bool
		var err error
		after, found, err = repo.FindByID(context.Background(), tx, "weekly")
		if err == nil && !found {
			t.Fatal("rappel introuvable après livraison")
		}
		return err
	})
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	if after.Status != persistence.ReminderStatusPending {
		t.Errorf("statut = %q, attendu pending (le rappel récurrent reste actif)", after.Status)
	}
	if after.FireAt != "2026-08-18T18:00:00Z" {
		t.Errorf("fire_at = %q, attendu 2026-08-18T18:00:00Z (occurrence suivante)", after.FireAt)
	}

	// L'échéance étant future, un second tick n'envoie rien de plus.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick (2e): %v", err)
	}
	if sent := provider.Sent(); len(sent) != 1 {
		t.Errorf("messages envoyés après second tick = %d, attendu 1", len(sent))
	}
}

func TestReminderRepository_RescheduleNextRespectsCancellation(t *testing.T) {
	db := testDB(t)

	rem := baseReminder("cancelled", "memory", "2026-08-17T11:00:00Z")
	rem.Status = persistence.ReminderStatusCancelled
	insertReminder(t, db, rem)

	repo := persistence.NewReminderRepository()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		ok, err := repo.RescheduleNext(context.Background(), tx, "cancelled", "2026-08-18T18:00:00Z")
		if err != nil {
			return err
		}
		if ok {
			t.Error("RescheduleNext sur un rappel annulé: ok=true, attendu false (la série s'arrête)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RescheduleNext: %v", err)
	}
}

// stubRunner joue le rôle de l'exécuteur de tâches sans faire tourner de
// modèle : il enregistre ce qu'on lui demande et rend une réponse fixe.
type stubRunner struct {
	reply string
	err   error
	tasks []persistence.Reminder
}

func (r *stubRunner) RunTask(ctx context.Context, task persistence.Reminder) (string, error) {
	r.tasks = append(r.tasks, task)
	if r.err != nil {
		return "", r.err
	}
	return r.reply, nil
}

func baseTask(id, provider, fireAt string) persistence.Reminder {
	task := baseReminder(id, provider, fireAt)
	task.Kind = persistence.ReminderKindTask
	task.AgentID = "main"
	task.Message = "Prépare un bulletin météo court"
	return task
}

// Une tâche planifiée ne délivre pas sa consigne : elle délivre le travail
// de l'agent. C'est toute la différence avec un rappel — l'utilisateur qui
// demande un bulletin météo quotidien attend la météo, pas la phrase
// « prépare un bulletin météo ».
func TestDispatcher_ScheduledTaskDeliversAgentReply(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	insertReminder(t, db, baseTask("task", "memory", "2026-08-17T11:59:00Z"))

	runner := &stubRunner{reply: "Ciel dégagé, 24 °C cet après-midi."}
	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithTaskRunner(runner).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(runner.tasks) != 1 {
		t.Fatalf("tâches exécutées = %d, attendu 1", len(runner.tasks))
	}
	if runner.tasks[0].AgentID != "main" {
		t.Errorf("agent exécutant = %q, attendu celui figé à la création", runner.tasks[0].AgentID)
	}

	sent := provider.Sent()
	if len(sent) != 1 {
		t.Fatalf("messages envoyés = %d, attendu 1", len(sent))
	}

	content, err := courier.GetMessageMainContent(context.Background(), sent[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	if !strings.Contains(content, "Ciel dégagé") {
		t.Errorf("contenu = %q, attendu la réponse de l'agent", content)
	}
	if strings.Contains(content, "Rappel") || strings.Contains(content, "bulletin météo court") {
		t.Errorf("contenu = %q, la consigne a été délivrée au lieu du travail", content)
	}

	if got := reminderStatus(t, db, "task"); got != persistence.ReminderStatusSent {
		t.Errorf("statut = %q, attendu sent", got)
	}
}

// Une tâche dont l'exécution échoue n'envoie rien, mais n'est plus classée
// failed d'office : la panne de réseau du 2026-08-19 au matin a tué le
// bulletin quotidien alors que le réseau revenait une heure après. La
// tentative est reprogrammée tant que le déclenchement suivant n'est pas
// atteint.
func TestDispatcher_ScheduledTaskFailureIsRetriedLater(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	task := baseTask("task", "memory", "2026-08-17T11:59:00Z")
	task.Recurrence = "0 8 * * *"
	task.Timezone = "Europe/Paris"
	insertReminder(t, db, task)

	runner := &stubRunner{err: context.DeadlineExceeded}
	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithTaskRunner(runner).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(provider.Sent()) != 0 {
		t.Errorf("messages envoyés = %d, attendu 0 : rien ne doit partir quand le travail a échoué", len(provider.Sent()))
	}

	rem := reminderRow(t, db, "task")
	if rem.Status != persistence.ReminderStatusPending {
		t.Fatalf("statut = %q, attendu pending : un échec transitoire ne tue plus la série", rem.Status)
	}
	if rem.Attempts != 1 {
		t.Errorf("attempts = %d, attendu 1", rem.Attempts)
	}
	// Première tentative : 5 minutes après l'échec (12:00 + 5 min), pas au
	// tick suivant — retenter toutes les 30 s ferait travailler l'agent en
	// boucle sur une panne durable.
	if rem.FireAt != "2026-08-17T12:05:00Z" {
		t.Errorf("fire_at = %q, attendu 2026-08-17T12:05:00Z", rem.FireAt)
	}
}

// L'occurrence manquée d'une entrée récurrente n'est rattrapée que si le
// déclenchement suivant n'est pas encore passé. Au-delà, elle est sautée
// sans livraison et la série repart sur la prochaine occurrence future : un
// bulletin d'avant-hier délivré aujourd'hui n'a plus de valeur.
func TestDispatcher_StaleRecurringOccurrenceIsSkipped(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	// Échéance du 15 août, quotidienne : les occurrences des 16 et 17 sont
	// aussi passées à l'heure du test (17 août 12:00 UTC).
	task := baseTask("task", "memory", "2026-08-15T06:00:00Z")
	task.Recurrence = "0 8 * * *"
	task.Timezone = "Europe/Paris"
	task.Attempts = 3
	insertReminder(t, db, task)

	runner := &stubRunner{reply: "bulletin"}
	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithTaskRunner(runner).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(runner.tasks) != 0 {
		t.Errorf("tâches exécutées = %d, attendu 0 : une occurrence périmée ne fait pas travailler l'agent", len(runner.tasks))
	}
	if len(provider.Sent()) != 0 {
		t.Errorf("messages envoyés = %d, attendu 0", len(provider.Sent()))
	}

	rem := reminderRow(t, db, "task")
	if rem.Status != persistence.ReminderStatusPending {
		t.Fatalf("statut = %q, attendu pending : la série continue", rem.Status)
	}
	// Prochaine occurrence STRICTEMENT future : le 18 août 8h Paris (6h UTC).
	if rem.FireAt != "2026-08-18T06:00:00Z" {
		t.Errorf("fire_at = %q, attendu 2026-08-18T06:00:00Z", rem.FireAt)
	}
	if rem.Attempts != 0 {
		t.Errorf("attempts = %d, attendu 0 : nouveau cycle, compteur remis à zéro", rem.Attempts)
	}
}

// Quand la tentative suivante tomberait après le déclenchement suivant,
// l'occurrence est abandonnée mais la série est réarmée — elle ne meurt
// plus sur un échec, contrairement à l'ancien classement failed.
func TestDispatcher_RecurringFailureNearDeadlineRearmsTheSeries(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	// Prochaine occurrence à 12:05 UTC : la retentative (+5 min) tomberait
	// pile dessus, la fenêtre de rattrapage est donc close.
	task := baseTask("task", "memory", "2026-08-17T11:59:00Z")
	task.Recurrence = "5 12 * * *"
	task.Timezone = "UTC"
	insertReminder(t, db, task)

	runner := &stubRunner{err: context.DeadlineExceeded}
	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithTaskRunner(runner).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rem := reminderRow(t, db, "task")
	if rem.Status != persistence.ReminderStatusPending {
		t.Fatalf("statut = %q, attendu pending", rem.Status)
	}
	if rem.FireAt != "2026-08-17T12:05:00Z" {
		t.Errorf("fire_at = %q, attendu la prochaine occurrence 2026-08-17T12:05:00Z", rem.FireAt)
	}
	if rem.Attempts != 0 {
		t.Errorf("attempts = %d, attendu 0", rem.Attempts)
	}
}

// Une entrée à déclenchement unique n'a pas d'occurrence suivante pour
// fermer sa fenêtre : elle est retentée, mais bornée en tentatives, puis
// failed — un échec définitif doit rester visible.
func TestDispatcher_OneShotFailureExhaustsAttemptsThenFails(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	task := baseTask("task", "memory", "2026-08-17T11:59:00Z")
	task.Attempts = 6
	insertReminder(t, db, task)

	runner := &stubRunner{err: context.DeadlineExceeded}
	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithTaskRunner(runner).
		WithClock(func() time.Time { return dispatcherTestNow })

	// 7e tentative : encore reprogrammée.
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rem := reminderRow(t, db, "task")
	if rem.Status != persistence.ReminderStatusPending || rem.Attempts != 7 {
		t.Fatalf("après la 7e tentative : statut = %q, attempts = %d, attendu pending/7", rem.Status, rem.Attempts)
	}

	// 8e tentative : la dernière, l'entrée est classée failed.
	late := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithTaskRunner(runner).
		WithClock(func() time.Time { return dispatcherTestNow.Add(2 * time.Hour) })

	if err := late.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := reminderStatus(t, db, "task"); got != persistence.ReminderStatusFailed {
		t.Errorf("statut = %q, attendu failed après épuisement des tentatives", got)
	}
}

// Sans exécuteur câblé, une tâche échue échoue franchement plutôt que de
// délivrer un message vide présenté comme le travail demandé.
func TestDispatcher_ScheduledTaskWithoutRunnerFails(t *testing.T) {
	db := testDB(t)
	provider := memory.NewProvider(memory.WithChannels(courier.NewChannel("chan-alice", courier.ChannelKindDirect, "Alice")))
	t.Cleanup(func() { _ = provider.Close() })

	insertReminder(t, db, baseTask("task", "memory", "2026-08-17T11:59:00Z"))

	d := reminder.NewDispatcher(db, reminder.SenderMap{"memory": provider}, testLogger(), nil).
		WithClock(func() time.Time { return dispatcherTestNow })

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(provider.Sent()) != 0 {
		t.Errorf("messages envoyés = %d, attendu 0", len(provider.Sent()))
	}
	if got := reminderStatus(t, db, "task"); got != persistence.ReminderStatusFailed {
		t.Errorf("statut = %q, attendu failed", got)
	}
}
