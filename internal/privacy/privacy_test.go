package privacy_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/privacy"
	"github.com/bornholm/automata/internal/usage"
)

func testDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
	})
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// seed prépare une organisation, un membre lié, ses messages privés et sa
// consommation.
func seed(t *testing.T, db *persistence.DB) {
	t.Helper()

	now := time.Now()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := persistence.NewOrganizationRepository().Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier Nord", CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}

		if err := persistence.NewMemberRepository().Insert(context.Background(), tx, persistence.Member{
			ID: "camille", OrgID: "atelier", DisplayName: "Camille Roux",
			Role: persistence.MemberRoleMember, Email: "camille@exemple.fr",
			Provider: "whatsapp", ExternalUserID: "camille-ext", LinkedAt: now,
			CreatedAt: now, UpdatedAt: now,
		}, true); err != nil {
			return err
		}

		messages := persistence.NewMessageRepository()
		for i, content := range []string{"bonjour", "peux-tu me rappeler demain ?"} {
			if err := messages.Insert(context.Background(), tx, persistence.Message{
				ID:                "msg-" + string(rune('a'+i)),
				ExternalMessageID: "ext-" + string(rune('a'+i)),
				ConversationID:    "whatsapp:camille-ext",
				PrincipalID:       "camille",
				Role:              "user",
				Content:           content,
				CreatedAt:         now.Format(time.RFC3339),
			}); err != nil {
				return err
			}
		}

		// Message d'un groupe : il ne doit jamais être exporté ni effacé.
		if err := messages.Insert(context.Background(), tx, persistence.Message{
			ID: "msg-groupe", ExternalMessageID: "ext-groupe", ConversationID: "whatsapp:groupe",
			PrincipalID: "camille", Role: "user", Content: "message de groupe",
			CreatedAt: now.Format(time.RFC3339),
		}); err != nil {
			return err
		}

		return persistence.NewUsageRecordRepository().Insert(context.Background(), tx, usage.Record{
			CreatedAt: now, OrgID: "atelier", PrincipalID: "camille",
			Component: "agent", Kind: "chat", CostAmount: 0.01, CostReported: true,
		})
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestExport_GathersPersonalDataOnly(t *testing.T) {
	db := testDB(t)
	seed(t, db)

	export, err := privacy.New(db, nil, nil).Export(context.Background(), "camille")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if export.Member.DisplayName != "Camille Roux" || export.Member.Organization != "Atelier Nord" {
		t.Errorf("compte exporté inattendu: %+v", export.Member)
	}
	if len(export.Messages) != 2 {
		t.Fatalf("%d message(s) exporté(s), attendu 2 (le message de groupe ne doit pas y être)", len(export.Messages))
	}
	for _, message := range export.Messages {
		if message.Content == "message de groupe" {
			t.Error("un message de groupe ne doit jamais figurer dans un export individuel")
		}
	}
	if len(export.Usage) != 1 {
		t.Errorf("%d mois de consommation, attendu 1", len(export.Usage))
	}
	if len(export.Explanations) == 0 {
		t.Error("l'export doit expliquer ce qu'il contient et ce qu'il omet")
	}
}

func TestDelete_ErasesPrivateDataAndDetachesAccount(t *testing.T) {
	db := testDB(t)
	seed(t, db)

	report, err := privacy.New(db, nil, nil).Delete(context.Background(), "camille")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if report.Messages != 2 {
		t.Errorf("%d message(s) supprimé(s), attendu 2", report.Messages)
	}

	err = db.WithTx(context.Background(), func(tx *sql.Tx) error {
		// Le compte survit, anonyme et détaché : le supprimer romprait les
		// conversations de groupe auxquelles il a participé.
		member, found, err := persistence.NewMemberRepository().FindByID(context.Background(), tx, "camille")
		if err != nil || !found {
			t.Fatalf("le compte doit subsister (found=%v, err=%v)", found, err)
		}
		if member.Email != "" || member.ExternalUserID != "" || member.Linked() {
			t.Errorf("le compte doit être détaché et sans courriel: %+v", member)
		}
		if member.DisplayName == "Camille Roux" {
			t.Error("le nom affiché doit être anonymisé")
		}

		// Le message de groupe reste : il appartient aussi aux autres.
		var groupMessages int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = 'whatsapp:groupe'`).Scan(&groupMessages); err != nil {
			return err
		}
		if groupMessages != 1 {
			t.Errorf("%d message(s) de groupe restant(s), attendu 1", groupMessages)
		}

		// Les traces d'usage subsistent, dissociées : ce sont des pièces
		// comptables.
		var records, attributed int
		if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(principal_id != ''), 0) FROM usage_records`).Scan(&records, &attributed); err != nil {
			return err
		}
		if records != 1 {
			t.Errorf("%d trace(s) d'usage, attendu 1 (conservation comptable)", records)
		}
		if attributed != 0 {
			t.Error("les traces d'usage ne doivent plus être rattachées à la personne")
		}

		return nil
	})
	if err != nil {
		t.Fatalf("vérification: %v", err)
	}
}
