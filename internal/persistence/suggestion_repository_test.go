package persistence_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

func openSuggestionDB(t *testing.T) *persistence.DB {
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

// Le cycle complet d'une suggestion, et le cloisonnement de UpdateStatus :
// un identifiant recopié depuis un autre membre ne modifie rien — c'est ce
// qui rend la page de profil sûre.
func TestSuggestions_LifecycleAndScoping(t *testing.T) {
	db := openSuggestionDB(t)
	repo := persistence.NewSuggestionRepository()
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, persistence.Suggestion{
			ID: "sug-1", OrgID: "atelier", MemberID: "cam",
			Kind: "automation", Title: "Programmer la météo",
			Body: "Vous demandez la météo chaque vendredi.", Status: persistence.SuggestionStatusProposed,
			CreatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Un autre membre ne peut pas toucher la suggestion.
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		changed, err := repo.UpdateStatus(context.Background(), tx, "atelier", "lina", "sug-1", persistence.SuggestionStatusDismissed)
		if err != nil {
			return err
		}
		if changed {
			t.Error("un autre membre a pu modifier la suggestion : cloisonnement rompu")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateStatus étranger: %v", err)
	}

	// Le membre concerné, si.
	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		changed, err := repo.UpdateStatus(context.Background(), tx, "atelier", "cam", "sug-1", persistence.SuggestionStatusAccepted)
		if err != nil {
			return err
		}
		if !changed {
			t.Error("le membre concerné devrait pouvoir changer le statut")
		}

		list, err := repo.ListByMember(context.Background(), tx, "atelier", "cam", 10)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].Status != persistence.SuggestionStatusAccepted {
			t.Errorf("liste = %+v, attendu une suggestion acceptée", list)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
}

// L'agrégat mensuel ne porte que des comptes par statut : la synthèse de
// l'exploitant est anonyme par construction.
func TestSuggestions_CountByStatusSince(t *testing.T) {
	db := openSuggestionDB(t)
	repo := persistence.NewSuggestionRepository()
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		for i, status := range []string{
			persistence.SuggestionStatusProposed,
			persistence.SuggestionStatusAccepted,
			persistence.SuggestionStatusAccepted,
		} {
			if err := repo.Insert(context.Background(), tx, persistence.Suggestion{
				ID: "sug-" + string(rune('a'+i)), OrgID: "atelier", MemberID: "cam",
				Kind: "fix", Title: "t", Body: "b", Status: status, CreatedAt: now,
			}); err != nil {
				return err
			}
		}
		// Une vieille suggestion hors fenêtre.
		return repo.Insert(context.Background(), tx, persistence.Suggestion{
			ID: "sug-old", OrgID: "atelier", MemberID: "cam",
			Kind: "fix", Title: "t", Body: "b", Status: persistence.SuggestionStatusDismissed,
			CreatedAt: now.AddDate(0, -3, 0),
		})
	})
	if err != nil {
		t.Fatalf("semis: %v", err)
	}

	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		counts, err := repo.CountByStatusSince(context.Background(), tx, now.AddDate(0, -1, 0))
		if err != nil {
			return err
		}
		if counts[persistence.SuggestionStatusAccepted] != 2 || counts[persistence.SuggestionStatusProposed] != 1 {
			t.Errorf("agrégats = %v", counts)
		}
		if counts[persistence.SuggestionStatusDismissed] != 0 {
			t.Error("la vieille suggestion ne devrait pas compter")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("agrégat: %v", err)
	}
}
