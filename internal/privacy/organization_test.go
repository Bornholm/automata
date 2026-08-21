package privacy_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/privacy"
)

// seedTwoOrgs prépare deux organisations et une même personne membre des
// deux, avec le même compte de messagerie.
func seedTwoOrgs(t *testing.T, db *persistence.DB) {
	t.Helper()

	now := time.Now()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		orgs := persistence.NewOrganizationRepository()
		members := persistence.NewMemberRepository()

		for _, org := range []persistence.Organization{
			{ID: "atelier", DisplayName: "Atelier Nord", CreatedAt: now, UpdatedAt: now},
			{ID: "famille", DisplayName: "Famille", CreatedAt: now, UpdatedAt: now},
		} {
			if err := orgs.Insert(context.Background(), tx, org, true); err != nil {
				return err
			}
		}

		// Camille est membre des deux organisations avec le même WhatsApp ;
		// Simon n'est membre que de l'atelier.
		for _, member := range []persistence.Member{
			{ID: "camille-atelier", OrgID: "atelier", DisplayName: "Camille", Role: persistence.MemberRoleMember,
				Provider: "whatsapp", ExternalUserID: "camille-ext", LinkedAt: now, CreatedAt: now, UpdatedAt: now},
			{ID: "camille-famille", OrgID: "famille", DisplayName: "Camille", Role: persistence.MemberRoleMember,
				Provider: "whatsapp", ExternalUserID: "camille-ext", LinkedAt: now, CreatedAt: now, UpdatedAt: now},
			{ID: "simon", OrgID: "atelier", DisplayName: "Simon", Role: persistence.MemberRoleMember,
				Provider: "whatsapp", ExternalUserID: "simon-ext", LinkedAt: now, CreatedAt: now, UpdatedAt: now},
		} {
			if err := members.Insert(context.Background(), tx, member, true); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(context.Background(),
			`INSERT INTO conversations (id, org_id, provider, external_channel_id, kind, scope, scope_id, created_at, updated_at)
			 VALUES ('whatsapp:atelier-groupe', 'atelier', 'whatsapp', 'atelier-groupe', 'group', 'group', 'atelier', ?, ?),
			        ('whatsapp:famille-groupe', 'famille', 'whatsapp', 'famille-groupe', 'group', 'group', 'famille', ?, ?)`,
			now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
			return err
		}

		messages := persistence.NewMessageRepository(nil)
		for id, conversationID := range map[string]string{
			"msg-atelier": "whatsapp:atelier-groupe",
			"msg-famille": "whatsapp:famille-groupe",
		} {
			if err := messages.Insert(context.Background(), tx, persistence.Message{
				ID: id, ExternalMessageID: "ext-" + id, ConversationID: model.ConversationID(conversationID),
				PrincipalID: "camille-atelier", Role: "user", Content: "bonjour",
				CreatedAt: now.Format(time.RFC3339),
			}); err != nil {
				return err
			}
		}

		bindings := persistence.NewChannelBindingRepository()
		return bindings.Upsert(context.Background(), tx, persistence.ChannelBinding{
			Provider: "whatsapp", ChannelID: "atelier-groupe", OrgID: "atelier",
			Kind: "group", Scope: "group", DisplayName: "Atelier", CreatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// La suppression d'une organisation emporte tout ce qui n'existe que par
// elle — et rien de ce qui appartient à une autre.
func TestDeleteOrganization(t *testing.T) {
	db := testDB(t)
	seedTwoOrgs(t, db)

	report, err := privacy.New(db, nil, nil).DeleteOrganization(context.Background(), "atelier")
	if err != nil {
		t.Fatalf("DeleteOrganization: %v", err)
	}

	if report.Members != 2 {
		t.Errorf("membres supprimés = %d, attendu 2", report.Members)
	}
	// Camille garde son compte dans la famille ; seul Simon perd son
	// dernier rattachement.
	if report.OrphanMembers != 1 {
		t.Errorf("membres orphelins = %d, attendu 1 (Simon)", report.OrphanMembers)
	}
	if report.Conversations != 1 || report.Messages != 1 || report.Channels != 1 {
		t.Errorf("emporté : %d conversation(s), %d message(s), %d canal(aux)", report.Conversations, report.Messages, report.Channels)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		counts := map[string]string{
			"organisation":      `SELECT COUNT(*) FROM organizations WHERE id = 'atelier'`,
			"membres":           `SELECT COUNT(*) FROM members WHERE org_id = 'atelier'`,
			"conversations":     `SELECT COUNT(*) FROM conversations WHERE org_id = 'atelier'`,
			"messages":          `SELECT COUNT(*) FROM messages WHERE conversation_id = 'whatsapp:atelier-groupe'`,
			"liaisons de canal": `SELECT COUNT(*) FROM channel_bindings WHERE org_id = 'atelier'`,
		}
		for label, query := range counts {
			var count int
			if err := tx.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Errorf("%s : %d ligne(s) survivent", label, count)
			}
		}

		// L'autre organisation est intacte, compte de Camille compris.
		var kept int
		if err := tx.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM members WHERE id = 'camille-famille'`).Scan(&kept); err != nil {
			return err
		}
		if kept != 1 {
			t.Error("le compte de Camille dans l'autre organisation a disparu")
		}

		if err := tx.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM messages WHERE conversation_id = 'whatsapp:famille-groupe'`).Scan(&kept); err != nil {
			return err
		}
		if kept != 1 {
			t.Error("les messages de l'autre organisation ont été emportés")
		}

		return nil
	}); err != nil {
		t.Fatalf("vérification: %v", err)
	}
}

// Les relevés de consommation survivent, dissociés : ce sont des pièces
// comptables, pas des données de conversation.
func TestDeleteOrganizationKeepsUsageRecords(t *testing.T) {
	db := testDB(t)
	seed(t, db)

	if _, err := privacy.New(db, nil, nil).DeleteOrganization(context.Background(), "atelier"); err != nil {
		t.Fatalf("DeleteOrganization: %v", err)
	}

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var count, attached int
		if err := tx.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM usage_records WHERE org_id = 'atelier'`).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("relevés conservés = %d, attendu 1", count)
		}

		if err := tx.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM usage_records WHERE principal_id != ''`).Scan(&attached); err != nil {
			return err
		}
		if attached != 0 {
			t.Error("les relevés devraient être dissociés de la personne")
		}

		return nil
	}); err != nil {
		t.Fatalf("vérification: %v", err)
	}
}
