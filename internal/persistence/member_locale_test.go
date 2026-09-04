package persistence_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/persistence"
)

func openMemberDB(t *testing.T) *persistence.DB {
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

// La langue survit à l'écriture et à la relecture, et une colonne vide —
// tous les membres existants au moment de la migration — rend le défaut
// plutôt qu'une langue vide qui ne désignerait aucun catalogue.
func TestMemberLocale_RoundTripAndDefault(t *testing.T) {
	db := openMemberDB(t)
	members := persistence.NewMemberRepository()
	now := time.Now().UTC()

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		orgs := persistence.NewOrganizationRepository()
		if err := orgs.Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier", CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}

		if err := members.Insert(context.Background(), tx, persistence.Member{
			ID: "cam", OrgID: "atelier", DisplayName: "Cam", Role: "member",
			Locale: i18n.ES, CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}
		// Un membre d'avant la migration : colonne vide.
		return members.Insert(context.Background(), tx, persistence.Member{
			ID: "dom", OrgID: "atelier", DisplayName: "Dom", Role: "member",
			CreatedAt: now, UpdatedAt: now,
		}, true)
	})
	if err != nil {
		t.Fatalf("semis: %v", err)
	}

	cases := map[string]i18n.Locale{"cam": i18n.ES, "dom": i18n.Default}
	for id, want := range cases {
		err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
			member, found, err := members.FindByID(context.Background(), tx, id)
			if err != nil || !found {
				t.Fatalf("relecture de %q: found=%v err=%v", id, found, err)
			}
			if member.Locale != want {
				t.Errorf("%s: locale = %q, attendu %q", id, member.Locale, want)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("lecture: %v", err)
		}
	}

	// Une valeur écrite à la main, ou une langue retirée du catalogue, ne
	// doit pas empêcher de lire le membre.
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE members SET locale = 'pt' WHERE id = 'cam'`)
		return err
	}); err != nil {
		t.Fatalf("altération: %v", err)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		member, _, err := members.FindByID(context.Background(), tx, "cam")
		if err != nil {
			return err
		}
		if member.Locale != i18n.Default {
			t.Errorf("locale invalide = %q, repli sur le défaut attendu", member.Locale)
		}
		return nil
	}); err != nil {
		t.Fatalf("relecture: %v", err)
	}
}

// L'écran d'administration écrit la langue par Update : sans la colonne
// dans le UPDATE, le choix serait perdu au premier enregistrement, en
// silence.
func TestMemberLocale_SurvivesUpdate(t *testing.T) {
	db := openMemberDB(t)
	members := persistence.NewMemberRepository()
	now := time.Now().UTC()

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		orgs := persistence.NewOrganizationRepository()
		if err := orgs.Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier", CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}
		return members.Insert(context.Background(), tx, persistence.Member{
			ID: "cam", OrgID: "atelier", DisplayName: "Cam", Role: "member",
			CreatedAt: now, UpdatedAt: now,
		}, true)
	}); err != nil {
		t.Fatalf("semis: %v", err)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		member, _, err := members.FindByID(context.Background(), tx, "cam")
		if err != nil {
			return err
		}
		member.Locale = i18n.EN
		member.UpdatedAt = now
		return members.Update(context.Background(), tx, member)
	}); err != nil {
		t.Fatalf("mise à jour: %v", err)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		member, _, err := members.FindByID(context.Background(), tx, "cam")
		if err != nil {
			return err
		}
		if member.Locale != i18n.EN {
			t.Errorf("locale après mise à jour = %q, attendu %q", member.Locale, i18n.EN)
		}
		return nil
	}); err != nil {
		t.Fatalf("relecture: %v", err)
	}
}
