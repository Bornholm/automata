package persistence

import (
	"context"
	"fmt"
)

// OrgDeletionBlocker nomme ce qui retient la suppression d'une
// organisation.
type OrgDeletionBlocker struct {
	// Kind : members | channels | conversations | usage.
	Kind  string
	Count int64
}

// Label rend le motif en clair, prêt à afficher.
func (b OrgDeletionBlocker) Label() string {
	switch b.Kind {
	case "members":
		return pluralize(b.Count, "%d membre rattaché", "%d membres rattachés")
	case "channels":
		return pluralize(b.Count, "%d canal lié", "%d canaux liés")
	case "conversations":
		return pluralize(b.Count, "%d conversation", "%d conversations")
	case "usage":
		return pluralize(b.Count, "%d appel facturé", "%d appels facturés")
	default:
		return b.Kind
	}
}

func pluralize(count int64, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf(singular, count)
	}

	return fmt.Sprintf(plural, count)
}

// DeletionBlockers énumère ce qui interdit de supprimer une organisation.
//
// Une organisation qui a vécu ne se supprime pas : ses conversations,
// ses souvenirs et sa consommation facturée forment un historique dont
// une partie vit hors de cette base (la mémoire amoxtli, les fichiers de
// pièces jointes). Effacer la ligne sans le reste laisserait des données
// orphelines, illisibles et pourtant présentes. Seule une organisation
// restée vide — typiquement créée en double — s'efface sans rien perdre.
func (r *OrganizationRepository) DeletionBlockers(ctx context.Context, q Querier, orgID string) ([]OrgDeletionBlocker, error) {
	counts := []struct {
		kind  string
		query string
	}{
		{"members", `SELECT COUNT(*) FROM members WHERE org_id = ?`},
		{"channels", `SELECT COUNT(*) FROM channel_bindings WHERE org_id = ?`},
		{"conversations", `SELECT COUNT(*) FROM conversations WHERE org_id = ?`},
		{"usage", `SELECT COUNT(*) FROM usage_records WHERE org_id = ?`},
	}

	var blockers []OrgDeletionBlocker
	for _, c := range counts {
		var count int64
		if err := q.QueryRowContext(ctx, c.query, orgID).Scan(&count); err != nil {
			return nil, fmt.Errorf("décompte %s de l'organisation %q: %w", c.kind, orgID, err)
		}
		if count > 0 {
			blockers = append(blockers, OrgDeletionBlocker{Kind: c.kind, Count: count})
		}
	}

	return blockers, nil
}

// Delete supprime une organisation restée vide, avec ce qui n'a de sens
// que par elle : son livre de comptes, ses jetons de liaison, ses
// réglages et ses activations de plugins. L'appelant doit avoir vérifié
// DeletionBlockers dans la même transaction.
func (r *OrganizationRepository) Delete(ctx context.Context, q Querier, orgID string) error {
	statements := []string{
		`DELETE FROM wallet_entries WHERE org_id = ?`,
		`DELETE FROM link_tokens WHERE org_id = ?`,
		`DELETE FROM org_settings WHERE org_id = ?`,
		`DELETE FROM plugin_activations WHERE org_id = ?`,
		`DELETE FROM plugin_configs WHERE org_id = ?`,
		`DELETE FROM plugin_secrets WHERE org_id = ?`,
		`DELETE FROM organizations WHERE id = ?`,
	}

	for _, statement := range statements {
		if _, err := q.ExecContext(ctx, statement, orgID); err != nil {
			return fmt.Errorf("suppression de l'organisation %q: %w", orgID, err)
		}
	}

	return nil
}
