package persistence_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
)

func openMissionDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func testMission(id string) persistence.Mission {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	return persistence.Mission{
		ID: id, OrgID: "atelier", PrincipalID: "cam",
		ConversationID: "whatsapp:cam-ext", Provider: "whatsapp", ChannelID: "cam-ext",
		AgentID: "main", Title: "Réclamation parking",
		Objective:   "Suivre la réclamation du parking, relancer chaque semaine sans réponse.",
		Status:      persistence.MissionStatusActive,
		NextCheckAt: now.AddDate(0, 0, 7), CreatedAt: now, UpdatedAt: now,
	}
}

// Le cycle d'une mission : créée, échue, journal mis à jour, close. Et le
// cloisonnement d'UpdateStatus : le membre d'à côté ne clôt rien.
func TestMissions_LifecycleAndScoping(t *testing.T) {
	db := openMissionDB(t)
	repo := persistence.NewMissionRepository(nil)
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, testMission("mis-1"))
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		// Pas échue avant l'échéance.
		due, err := repo.ListDue(context.Background(), tx, now, 10)
		if err != nil {
			return err
		}
		if len(due) != 0 {
			t.Errorf("%d missions échues avant l'échéance", len(due))
		}

		// Échue après.
		due, err = repo.ListDue(context.Background(), tx, now.AddDate(0, 0, 8), 10)
		if err != nil {
			return err
		}
		if len(due) != 1 {
			t.Fatalf("%d missions échues, une attendue", len(due))
		}

		// Un réveil note et replanifie.
		if err := repo.UpdateJournal(context.Background(), tx, "mis-1",
			"2026-09-10: no reply yet, follow-up proposed.",
			persistence.MissionStatusActive, now.AddDate(0, 0, 14), 0, now.AddDate(0, 0, 7)); err != nil {
			return err
		}

		mission, found, err := repo.FindByID(context.Background(), tx, "mis-1")
		if err != nil || !found {
			t.Fatalf("FindByID: found=%v err=%v", found, err)
		}
		if !strings.Contains(mission.Journal, "no reply yet") {
			t.Errorf("journal = %q", mission.Journal)
		}

		// Un autre membre ne peut pas la clore.
		changed, err := repo.UpdateStatus(context.Background(), tx, "atelier", "lina", "mis-1",
			persistence.MissionStatusAbandoned, now)
		if err != nil {
			return err
		}
		if changed {
			t.Error("un autre membre a pu clore la mission : cloisonnement rompu")
		}

		// Le sien, si — et la mission close n'est plus jamais échue.
		if _, err := repo.UpdateStatus(context.Background(), tx, "atelier", "cam", "mis-1",
			persistence.MissionStatusAbandoned, now); err != nil {
			return err
		}
		due, err = repo.ListDue(context.Background(), tx, now.AddDate(1, 0, 0), 10)
		if err != nil {
			return err
		}
		if len(due) != 0 {
			t.Error("une mission close est encore réveillée")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
}

// Avec une clé, objectif et journal sont illisibles en SQL direct — c'est
// la différence assumée avec les tâches planifiées, restées en clair.
func TestMissions_ContentSealedAtRest(t *testing.T) {
	db := openMissionDB(t)
	box, err := secretbox.NewContentBox(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("NewContentBox: %v", err)
	}
	repo := persistence.NewMissionRepository(box)

	mission := testMission("mis-2")
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, mission)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var rawObjective string
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT objective FROM missions WHERE id = 'mis-2'`).Scan(&rawObjective)
	}); err != nil {
		t.Fatalf("lecture brute: %v", err)
	}
	if strings.Contains(rawObjective, "parking") {
		t.Fatal("l'objectif se lit en clair dans la base")
	}

	// Et se relit déchiffré par le repo.
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		got, found, err := repo.FindByID(context.Background(), tx, "mis-2")
		if err != nil || !found {
			t.Fatalf("FindByID: %v", err)
		}
		if got.Objective != mission.Objective {
			t.Errorf("objectif relu = %q", got.Objective)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
