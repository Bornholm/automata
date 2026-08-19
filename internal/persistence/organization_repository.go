package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// formatTenantTime sérialise un horodatage des tables du socle SaaS :
// RFC3339 UTC, chaîne vide pour la valeur zéro (« non renseigné », voir
// tenant_types.go).
func formatTenantTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTenantTime est l'inverse de formatTenantTime.
func parseTenantTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("horodatage invalide %q: %w", raw, err)
	}
	return t, nil
}

// OrganizationRepository donne accès à la table organizations (migration
// 0010) : les organisations clientes du socle SaaS, pilotées par
// l'interface d'administration web.
type OrganizationRepository struct{}

// NewOrganizationRepository crée un OrganizationRepository.
func NewOrganizationRepository() *OrganizationRepository {
	return &OrganizationRepository{}
}

// Insert enregistre org. ignoreExisting (bootstrap) transforme un conflit
// d'identifiant en non-opération plutôt qu'en erreur.
func (r *OrganizationRepository) Insert(ctx context.Context, q Querier, org Organization, ignoreExisting bool) error {
	verb := "INSERT"
	if ignoreExisting {
		verb = "INSERT OR IGNORE"
	}

	offered := 0
	if org.Offered {
		offered = 1
	}

	_, err := q.ExecContext(ctx, verb+` INTO organizations
		(id, display_name, offered, monthly_allowance, low_balance_notified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		org.ID, org.DisplayName, offered, org.MonthlyAllowance,
		formatTenantTime(org.LowBalanceNotifiedAt),
		formatTenantTime(org.CreatedAt), formatTenantTime(org.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insertion de l'organisation %q: %w", org.ID, err)
	}

	return nil
}

// Update remplace le nom affiché, le statut offert et l'allocation.
func (r *OrganizationRepository) Update(ctx context.Context, q Querier, org Organization) error {
	offered := 0
	if org.Offered {
		offered = 1
	}

	res, err := q.ExecContext(ctx, `UPDATE organizations
		SET display_name = ?, offered = ?, monthly_allowance = ?,
			low_balance_notified_at = ?, updated_at = ?
		WHERE id = ?`,
		org.DisplayName, offered, org.MonthlyAllowance,
		formatTenantTime(org.LowBalanceNotifiedAt), formatTenantTime(org.UpdatedAt), org.ID)
	if err != nil {
		return fmt.Errorf("mise à jour de l'organisation %q: %w", org.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("organisation %q introuvable", org.ID)
	}

	return nil
}

const organizationColumns = `id, display_name, offered, monthly_allowance, low_balance_notified_at, created_at, updated_at`

func scanOrganization(scan func(...any) error) (Organization, error) {
	var (
		org                              Organization
		offered                          int
		notifiedAt, createdAt, updatedAt string
	)
	if err := scan(&org.ID, &org.DisplayName, &offered, &org.MonthlyAllowance, &notifiedAt, &createdAt, &updatedAt); err != nil {
		return Organization{}, err
	}

	org.Offered = offered != 0

	var err error
	if org.LowBalanceNotifiedAt, err = parseTenantTime(notifiedAt); err != nil {
		return Organization{}, err
	}
	if org.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return Organization{}, err
	}
	if org.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return Organization{}, err
	}

	return org, nil
}

// FindByID retourne l'organisation, ou (Organization{}, false, nil) si
// elle n'existe pas.
func (r *OrganizationRepository) FindByID(ctx context.Context, q Querier, id string) (Organization, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+organizationColumns+` FROM organizations WHERE id = ?`, id)

	org, err := scanOrganization(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Organization{}, false, nil
		}
		return Organization{}, false, fmt.Errorf("lecture de l'organisation %q: %w", id, err)
	}

	return org, true, nil
}

// List retourne toutes les organisations, triées par nom affiché. search,
// s'il n'est pas vide, filtre par sous-chaîne insensible à la casse du nom
// affiché ou de l'identifiant.
func (r *OrganizationRepository) List(ctx context.Context, q Querier, search string) ([]Organization, error) {
	query := `SELECT ` + organizationColumns + ` FROM organizations`
	args := []any{}
	if search != "" {
		query += ` WHERE display_name LIKE ? COLLATE NOCASE OR id LIKE ? COLLATE NOCASE`
		pattern := "%" + search + "%"
		args = append(args, pattern, pattern)
	}
	query += ` ORDER BY display_name COLLATE NOCASE`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("liste des organisations: %w", err)
	}
	defer rows.Close()

	var orgs []Organization
	for rows.Next() {
		org, err := scanOrganization(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("lecture d'une organisation: %w", err)
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des organisations: %w", err)
	}

	return orgs, nil
}
