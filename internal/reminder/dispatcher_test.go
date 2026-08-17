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

	d := reminder.NewDispatcher(db, map[string]courier.Provider{"memory": provider}, testLogger(), nil).
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

	d := reminder.NewDispatcher(db, map[string]courier.Provider{}, testLogger(), nil).
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

	d := reminder.NewDispatcher(db, map[string]courier.Provider{"memory": provider}, testLogger(), nil).
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
