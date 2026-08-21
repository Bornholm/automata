package privacy

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/bornholm/automata/internal/persistence"
)

// OrgDeletionReport dit ce qu'une suppression d'organisation a emporté.
// Il sert à l'écrire au journal — jamais un contenu, seulement des
// compteurs.
type OrgDeletionReport struct {
	Members       int
	Channels      int
	Conversations int
	Messages      int
	Reminders     int
	Memories      int
	// OrphanMembers : membres dont c'était le dernier rattachement.
	// Leur identité de messagerie ne désigne plus personne.
	OrphanMembers int
}

// DeleteOrganization efface une organisation et tout ce qui n'existe que
// par elle.
//
// Ce qui part : ses membres, ses canaux rattachés, ses conversations avec
// leurs messages, résumés et pièces jointes, ses rappels, ses plans
// d'actions, ses tâches planifiées, ses jetons, ses réglages, ses
// activations de plugins et leurs secrets, et ses souvenirs — personnels
// comme collectifs.
//
// Ce qui reste : les traces de consommation et les mouvements de
// portefeuille. Ce sont des pièces comptables, et le principe du paquet
// est de les dissocier plutôt que de les détruire — une recette ou un
// coût constaté ne s'efface pas parce qu'un client s'en va.
//
// Un membre n'appartient qu'à une organisation : sa ligne est propre à
// celle-ci, et la supprimer ne touche jamais le profil que la même
// personne détiendrait ailleurs. Quand ce rattachement était le dernier
// de son identité de messagerie, elle ne désigne plus personne — c'est ce
// que compte OrphanMembers.
func (s *Service) DeleteOrganization(ctx context.Context, orgID string) (OrgDeletionReport, error) {
	var report OrgDeletionReport

	// Les souvenirs d'abord : ils vivent hors de la transaction SQL, dans
	// la base mémoire. Les effacer en dernier laisserait, si la
	// transaction échouait, une organisation vivante amputée de sa
	// mémoire — l'inverse est réparable, ces souvenirs ne désignent plus
	// rien.
	if s.memory != nil {
		memories, err := s.memory.List(ctx)
		if err != nil {
			return report, fmt.Errorf("privacy: lecture des souvenirs: %w", err)
		}
		for _, m := range memories {
			if m.Metadata["org_id"] != orgID {
				continue
			}
			if err := s.memory.Forget(ctx, m.ID); err != nil {
				return report, fmt.Errorf("privacy: oubli du souvenir %q: %w", m.ID, err)
			}
			report.Memories++
		}
	}

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		members, err := s.members.ListByOrg(ctx, tx, orgID)
		if err != nil {
			return err
		}
		report.Members = len(members)

		for _, member := range members {
			// Dernier rattachement de cette identité de messagerie ?
			// Le compter avant de supprimer la ligne, sans quoi la
			// question n'a plus de réponse.
			if member.Linked() {
				elsewhere, err := s.membersElsewhere(ctx, tx, member.Provider, member.ExternalUserID, orgID)
				if err != nil {
					return err
				}
				if elsewhere == 0 {
					report.OrphanMembers++
				}
			}

			if _, err := tx.ExecContext(ctx, `DELETE FROM profile_links WHERE member_id = ?`, member.ID); err != nil {
				return fmt.Errorf("suppression des liens de profil: %w", err)
			}
		}

		conversations, err := conversationIDsOfOrg(ctx, tx, orgID)
		if err != nil {
			return err
		}
		report.Conversations = len(conversations)

		for _, conversationID := range conversations {
			// Les pièces jointes tombent par ON DELETE CASCADE depuis
			// messages ; les plans, eux, référencent la conversation et
			// doivent partir avant elle.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM actions WHERE plan_id IN (SELECT id FROM action_plans WHERE conversation_id = ?)`,
				conversationID); err != nil {
				return fmt.Errorf("suppression des actions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM action_plans WHERE conversation_id = ?`, conversationID); err != nil {
				return fmt.Errorf("suppression des plans d'actions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_summaries WHERE conversation_id = ?`, conversationID); err != nil {
				return fmt.Errorf("suppression des résumés: %w", err)
			}

			result, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, conversationID)
			if err != nil {
				return fmt.Errorf("suppression des messages: %w", err)
			}
			if n, err := result.RowsAffected(); err == nil {
				report.Messages += int(n)
			}
		}

		result, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE org_id = ?`, orgID)
		if err != nil {
			return fmt.Errorf("suppression des rappels: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			report.Reminders = int(n)
		}

		result, err = tx.ExecContext(ctx, `DELETE FROM channel_bindings WHERE org_id = ?`, orgID)
		if err != nil {
			return fmt.Errorf("suppression des liaisons de canal: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			report.Channels = int(n)
		}

		// Traces de consommation : dissociées de la personne et de la
		// conversation, conservées comme pièces comptables (voir l'en-tête
		// du paquet).
		if _, err := tx.ExecContext(ctx,
			`UPDATE usage_records SET principal_id = '', conversation_id = '' WHERE org_id = ?`, orgID); err != nil {
			return fmt.Errorf("dissociation des traces d'usage: %w", err)
		}

		statements := []string{
			`DELETE FROM conversations WHERE org_id = ?`,
			`DELETE FROM scheduled_runs WHERE org_id = ?`,
			`DELETE FROM audit_events WHERE org_id = ?`,
			`DELETE FROM members WHERE org_id = ?`,
			`DELETE FROM link_tokens WHERE org_id = ?`,
			`DELETE FROM org_settings WHERE org_id = ?`,
			`DELETE FROM plugin_activations WHERE org_id = ?`,
			`DELETE FROM plugin_configs WHERE org_id = ?`,
			`DELETE FROM plugin_secrets WHERE org_id = ?`,
			`DELETE FROM organizations WHERE id = ?`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement, orgID); err != nil {
				return fmt.Errorf("suppression de l'organisation %q: %w", orgID, err)
			}
		}

		return nil
	})
	if err != nil {
		return report, fmt.Errorf("privacy: suppression de l'organisation %q: %w", orgID, err)
	}

	return report, nil
}

// membersElsewhere compte les autres rattachements d'une identité de
// messagerie, hors de l'organisation en cours de suppression.
func (s *Service) membersElsewhere(ctx context.Context, tx *sql.Tx, provider, externalUserID, excludeOrgID string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM members
		WHERE provider = ? AND external_user_id = ? AND linked_at != '' AND org_id != ?`,
		provider, externalUserID, excludeOrgID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("décompte des rattachements de l'identité: %w", err)
	}

	return count, nil
}

// conversationIDsOfOrg liste les conversations d'une organisation.
func conversationIDsOfOrg(ctx context.Context, q persistence.Querier, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM conversations WHERE org_id = ?`, orgID)
	if err != nil {
		return nil, fmt.Errorf("liste des conversations de l'organisation %q: %w", orgID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lecture d'une conversation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des conversations: %w", err)
	}

	return ids, nil
}
