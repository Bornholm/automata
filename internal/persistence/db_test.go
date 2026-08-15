package persistence_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// testConfig retourne une configuration de stockage applicatif pointant
// vers un fichier SQLite dans un répertoire temporaire, avec les pragmas
// recommandés par le plan (foreign keys, WAL, busy timeout).
func testConfig(t *testing.T) config.StorageApplication {
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

func openTestDB(t *testing.T, cfg config.StorageApplication) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

func TestOpenCreatesAllTables(t *testing.T) {
	cfg := testConfig(t)
	db := openTestDB(t, cfg)

	expected := []string{
		"schema_migrations",
		"principals",
		"conversations",
		"messages",
		"action_plans",
		"actions",
		"processed_messages",
		"scheduled_runs",
		"delivery_attempts",
		"audit_events",
	}

	for _, table := range expected {
		var name string
		err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
			row := tx.QueryRowContext(context.Background(),
				`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table)
			return row.Scan(&name)
		})
		if err != nil {
			t.Errorf("table %q absente: %v", table, err)
		}
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	cfg := testConfig(t)

	db1, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("premier Open: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Réouvrir la même base : les migrations doivent être un no-op.
	db2, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	var count int
	err = db2.WithTx(context.Background(), func(tx *sql.Tx) error {
		row := tx.QueryRowContext(context.Background(), `SELECT COUNT(1) FROM schema_migrations WHERE version = 1`)
		return row.Scan(&count)
	})
	if err != nil {
		t.Fatalf("lecture de schema_migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("migration 1 appliquée %d fois, attendu 1", count)
	}
}

func TestForeignKeyConstraintEnforced(t *testing.T) {
	cfg := testConfig(t)
	db := openTestDB(t, cfg)

	messages := persistence.NewMessageRepository()

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return messages.Insert(context.Background(), tx, persistence.Message{
			ID:                "msg-1",
			ConversationID:    model.ConversationID("missing-conversation"),
			ExternalMessageID: "ext-1",
			PrincipalID:       model.PrincipalID("alice"),
			Role:              "user",
			Content:           "hello",
			ContentKind:       "text",
			CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		})
	})
	if err == nil {
		t.Fatal("insertion d'un message avec conversation_id inexistant: attendu une erreur")
	}
}

func TestDuplicateMessageConstraint(t *testing.T) {
	cfg := testConfig(t)
	db := openTestDB(t, cfg)

	conversations := persistence.NewConversationRepository()
	messages := persistence.NewMessageRepository()

	now := time.Now().UTC().Format(time.RFC3339)
	conv := persistence.Conversation{
		ID:                model.ConversationID("conv-1"),
		OrgID:             model.OrgID("home"),
		Provider:          "whatsapp",
		ExternalChannelID: "chan-1",
		Kind:              model.ChannelPrivate,
		Scope:             model.ScopePersonal,
		ScopeID:           model.ScopeID("alice"),
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return conversations.Insert(context.Background(), tx, conv)
	}); err != nil {
		t.Fatalf("insertion de la conversation: %v", err)
	}

	msg := persistence.Message{
		ID:                "msg-1",
		ConversationID:    conv.ID,
		ExternalMessageID: "ext-1",
		PrincipalID:       model.PrincipalID("alice"),
		Role:              "user",
		Content:           "hello",
		ContentKind:       "text",
		CreatedAt:         now,
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return messages.Insert(context.Background(), tx, msg)
	}); err != nil {
		t.Fatalf("première insertion du message: %v", err)
	}

	msg.ID = "msg-2"
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return messages.Insert(context.Background(), tx, msg)
	})
	if err == nil {
		t.Fatal("insertion d'un message dupliqué (même conversation_id, external_message_id): attendu une erreur")
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	cfg := testConfig(t)
	db := openTestDB(t, cfg)

	processed := persistence.NewProcessedMessageRepository()
	sentinel := errors.New("échec délibéré")

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := processed.Insert(context.Background(), tx, persistence.ProcessedMessage{
			Provider:          "whatsapp",
			ExternalMessageID: "ext-rollback",
			ProcessedAt:       time.Now().UTC().Format(time.RFC3339),
			Status:            "ok",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("erreur attendue %v, obtenu %v", sentinel, err)
	}

	var found bool
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, ok, err := processed.FindByProviderAndExternalMessageID(context.Background(), tx, "whatsapp", "ext-rollback")
		found = ok
		return err
	})
	if err != nil {
		t.Fatalf("FindByProviderAndExternalMessageID: %v", err)
	}
	if found {
		t.Fatal("le message traité inséré dans la transaction annulée est visible, attendu absent")
	}
}

func TestConcurrentInsertsProcessedMessages(t *testing.T) {
	cfg := testConfig(t)
	db := openTestDB(t, cfg)

	processed := persistence.NewProcessedMessageRepository()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
				return processed.Insert(context.Background(), tx, persistence.ProcessedMessage{
					Provider:          "whatsapp",
					ExternalMessageID: fmt.Sprintf("ext-concurrent-%d", i),
					ProcessedAt:       time.Now().UTC().Format(time.RFC3339),
					Status:            "ok",
				})
			})
			errs <- err
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("insertion concurrente: %v", err)
		}
	}
}

func TestCloseThenUseReturnsError(t *testing.T) {
	cfg := testConfig(t)
	db := openTestDB(t, cfg)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return nil
	})
	if err == nil {
		t.Fatal("utilisation de la base après Close(): attendu une erreur")
	}
}

// TestOpenRestrictsFilePermissions vérifie que le fichier SQLite et son
// répertoire parent sont créés avec des permissions restreintes au seul
// propriétaire (PLAN.md Phase 19, point 5) : la base contient
// potentiellement des données personnelles (messages, plans d'actions).
func TestOpenRestrictsFilePermissions(t *testing.T) {
	cfg := testConfig(t)
	// Un sous-répertoire n'existant pas encore, pour que Open l'crée
	// lui-même via os.MkdirAll : t.TempDir() existe déjà et ses
	// permissions ne dépendent pas du code testé.
	cfg.Path = filepath.Join(filepath.Dir(cfg.Path), "data", "automata.sqlite")
	openTestDB(t, cfg)

	info, err := os.Stat(cfg.Path)
	if err != nil {
		t.Fatalf("stat du fichier de base: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions du fichier de base = %o, attendu 0600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(cfg.Path))
	if err != nil {
		t.Fatalf("stat du répertoire parent: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("permissions du répertoire parent = %o, attendu 0700", perm)
	}
}
