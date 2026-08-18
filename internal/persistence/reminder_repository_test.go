package persistence_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/bornholm/automata/internal/persistence"
)

func insertTestReminder(t *testing.T, db *persistence.DB, rem persistence.Reminder) {
	t.Helper()

	repo := persistence.NewReminderRepository()
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, rem)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func testReminder(id, fireAt string) persistence.Reminder {
	return persistence.Reminder{
		ID:             persistence.ReminderID(id),
		OrgID:          "home",
		PrincipalID:    "alice",
		ConversationID: "conv-alice",
		Provider:       "whatsapp",
		ChannelID:      "chan-alice",
		Message:        "sortir les poubelles",
		FireAt:         fireAt,
		Status:         persistence.ReminderStatusPending,
		CreatedAt:      "2026-08-17T10:00:00Z",
	}
}

func TestReminderRepository_ListDueReturnsOnlyElapsedPending(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewReminderRepository()

	insertTestReminder(t, db, testReminder("due-1", "2026-08-17T11:00:00Z"))
	insertTestReminder(t, db, testReminder("due-2", "2026-08-17T09:00:00Z"))
	insertTestReminder(t, db, testReminder("future", "2026-08-18T09:00:00Z"))

	cancelled := testReminder("cancelled", "2026-08-17T08:00:00Z")
	cancelled.Status = persistence.ReminderStatusCancelled
	insertTestReminder(t, db, cancelled)

	var due []persistence.Reminder
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		due, err = repo.ListDue(context.Background(), tx, "2026-08-17T12:00:00Z", 50)
		return err
	})
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}

	if len(due) != 2 {
		t.Fatalf("ListDue: %d rappels, attendu 2", len(due))
	}

	// Les plus anciens d'abord.
	if due[0].ID != "due-2" || due[1].ID != "due-1" {
		t.Errorf("ordre = [%s, %s], attendu [due-2, due-1]", due[0].ID, due[1].ID)
	}
}

func TestReminderRepository_UpdateStatusIsOptimistic(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewReminderRepository()

	insertTestReminder(t, db, testReminder("rem-1", "2026-08-17T11:00:00Z"))

	ctx := context.Background()

	// pending → cancelled réussit.
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		ok, err := repo.UpdateStatus(ctx, tx, "rem-1", persistence.ReminderStatusPending, persistence.ReminderStatusCancelled, nil)
		if err != nil {
			return err
		}
		if !ok {
			t.Error("UpdateStatus pending→cancelled: ok=false, attendu true")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// pending → sent échoue ensuite : le rappel n'est plus pending.
	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		sentAt := "2026-08-17T11:00:05Z"
		ok, err := repo.UpdateStatus(ctx, tx, "rem-1", persistence.ReminderStatusPending, persistence.ReminderStatusSent, &sentAt)
		if err != nil {
			return err
		}
		if ok {
			t.Error("UpdateStatus sur un rappel annulé: ok=true, attendu false (verrou optimiste)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
}

func TestReminderRepository_ListPendingByConversationIsolates(t *testing.T) {
	db := openTestDB(t, testConfig(t))
	repo := persistence.NewReminderRepository()

	insertTestReminder(t, db, testReminder("mine", "2026-08-18T09:00:00Z"))

	other := testReminder("other", "2026-08-18T09:00:00Z")
	other.ConversationID = "conv-leo"
	insertTestReminder(t, db, other)

	var mine []persistence.Reminder
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		mine, err = repo.ListPendingByConversation(context.Background(), tx, "conv-alice", persistence.ReminderKindMessage)
		return err
	})
	if err != nil {
		t.Fatalf("ListPendingByConversation: %v", err)
	}

	if len(mine) != 1 || mine[0].ID != "mine" {
		t.Errorf("rappels de conv-alice = %v, attendu [mine] uniquement", mine)
	}
}
