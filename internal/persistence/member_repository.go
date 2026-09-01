package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MemberRepository donne accès à la table members (migration 0010) : les
// membres pré-créés des organisations du socle SaaS.
type MemberRepository struct{}

// NewMemberRepository crée un MemberRepository.
func NewMemberRepository() *MemberRepository {
	return &MemberRepository{}
}

const memberColumns = `id, org_id, display_name, role, email, email_verified_at,
	provider, external_user_id, linked_at, onboarding_state, suggestions_muted, created_at, updated_at`

// Insert enregistre m. ignoreExisting (bootstrap) transforme un conflit
// d'identifiant en non-opération.
func (r *MemberRepository) Insert(ctx context.Context, q Querier, m Member, ignoreExisting bool) error {
	verb := "INSERT"
	if ignoreExisting {
		verb = "INSERT OR IGNORE"
	}

	_, err := q.ExecContext(ctx, verb+` INTO members
		(`+memberColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OrgID, m.DisplayName, m.Role, m.Email, formatTenantTime(m.EmailVerifiedAt),
		m.Provider, m.ExternalUserID, formatTenantTime(m.LinkedAt), m.OnboardingState, m.SuggestionsMuted,
		formatTenantTime(m.CreatedAt), formatTenantTime(m.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insertion du membre %q: %w", m.ID, err)
	}

	return nil
}

func scanMember(scan func(...any) error) (Member, error) {
	var (
		m                                          Member
		emailVerifiedAt, linkedAt, createdAt, upAt string
	)
	if err := scan(&m.ID, &m.OrgID, &m.DisplayName, &m.Role, &m.Email, &emailVerifiedAt,
		&m.Provider, &m.ExternalUserID, &linkedAt, &m.OnboardingState, &m.SuggestionsMuted, &createdAt, &upAt); err != nil {
		return Member{}, err
	}

	var err error
	if m.EmailVerifiedAt, err = parseTenantTime(emailVerifiedAt); err != nil {
		return Member{}, err
	}
	if m.LinkedAt, err = parseTenantTime(linkedAt); err != nil {
		return Member{}, err
	}
	if m.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return Member{}, err
	}
	if m.UpdatedAt, err = parseTenantTime(upAt); err != nil {
		return Member{}, err
	}

	return m, nil
}

// FindByID retourne le membre, ou (Member{}, false, nil) s'il n'existe pas.
func (r *MemberRepository) FindByID(ctx context.Context, q Querier, id string) (Member, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members WHERE id = ?`, id)

	m, err := scanMember(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Member{}, false, nil
		}
		return Member{}, false, fmt.Errorf("lecture du membre %q: %w", id, err)
	}

	return m, true, nil
}

// ListByOrg retourne les membres d'une organisation, triés par nom affiché.
func (r *MemberRepository) ListByOrg(ctx context.Context, q Querier, orgID string) ([]Member, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+memberColumns+` FROM members
		WHERE org_id = ? ORDER BY display_name COLLATE NOCASE`, orgID)
	if err != nil {
		return nil, fmt.Errorf("liste des membres de %q: %w", orgID, err)
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		m, err := scanMember(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("lecture d'un membre: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des membres: %w", err)
	}

	return members, nil
}

// CountByOrg retourne le nombre de membres par organisation, en une seule
// requête (liste ADM-02).
func (r *MemberRepository) CountByOrg(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT org_id, COUNT(*) FROM members GROUP BY org_id`)
	if err != nil {
		return nil, fmt.Errorf("comptage des membres: %w", err)
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var orgID string
		var n int64
		if err := rows.Scan(&orgID, &n); err != nil {
			return nil, fmt.Errorf("lecture d'un comptage de membres: %w", err)
		}
		counts[orgID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des comptages de membres: %w", err)
	}

	return counts, nil
}

// Update remplace la fiche du membre (nom, rôle, courriel, vérification,
// identité de messagerie). L'identifiant et l'organisation ne changent pas.
func (r *MemberRepository) Update(ctx context.Context, q Querier, m Member) error {
	res, err := q.ExecContext(ctx, `UPDATE members
		SET display_name = ?, role = ?, email = ?, email_verified_at = ?,
			provider = ?, external_user_id = ?, linked_at = ?, updated_at = ?
		WHERE id = ?`,
		m.DisplayName, m.Role, m.Email, formatTenantTime(m.EmailVerifiedAt),
		m.Provider, m.ExternalUserID, formatTenantTime(m.LinkedAt),
		formatTenantTime(m.UpdatedAt), m.ID)
	if err != nil {
		return fmt.Errorf("mise à jour du membre %q: %w", m.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("membre %q introuvable", m.ID)
	}

	return nil
}

// SetOnboardingState avance la visite d'accueil du membre.
//
// Update ne touche volontairement pas cette colonne : l'écran
// d'administration modifie un membre sans avoir à connaître l'état de sa
// visite, et ne doit surtout pas la réinitialiser par ricochet.
func (r *MemberRepository) SetOnboardingState(ctx context.Context, q Querier, memberID, state string) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE members SET onboarding_state = ? WHERE id = ?`, state, memberID); err != nil {
		return fmt.Errorf("visite d'accueil du membre %q: %w", memberID, err)
	}

	return nil
}

// SetSuggestionsMuted allume ou éteint l'introspection pour un membre.
// Comme SetOnboardingState, Update ne touche pas cette colonne : l'écran
// d'administration n'a pas à connaître ce choix pour éditer un membre.
func (r *MemberRepository) SetSuggestionsMuted(ctx context.Context, q Querier, memberID string, muted bool) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE members SET suggestions_muted = ? WHERE id = ?`, muted, memberID); err != nil {
		return fmt.Errorf("réglage des suggestions du membre %q: %w", memberID, err)
	}

	return nil
}

// FindByExternalUser retourne le membre lié à une identité de messagerie
// (point d'entrée de la résolution d'identité dynamique, lot B).
func (r *MemberRepository) FindByExternalUser(ctx context.Context, q Querier, provider, externalUserID string) (Member, bool, error) {
	return r.findByExternalUser(ctx, q, provider, externalUserID, "")
}

// FindByExternalUserInOrg retourne le membre lié à une identité de
// messagerie DANS une organisation donnée.
//
// Une même personne peut appartenir à plusieurs organisations avec le
// même compte de messagerie — son groupe familial et celui de son
// employeur, sur le même WhatsApp. C'est l'organisation du canal qui
// désigne lequel de ses profils parle : sans ce filtre, la première
// ligne venue gagnait, et la personne se voyait refuser la parole dans
// le groupe de son autre organisation.
func (r *MemberRepository) FindByExternalUserInOrg(ctx context.Context, q Querier, provider, externalUserID, orgID string) (Member, bool, error) {
	return r.findByExternalUser(ctx, q, provider, externalUserID, orgID)
}

func (r *MemberRepository) findByExternalUser(ctx context.Context, q Querier, provider, externalUserID, orgID string) (Member, bool, error) {
	query := `SELECT ` + memberColumns + ` FROM members
		WHERE provider = ? AND external_user_id = ? AND linked_at != ''`
	args := []any{provider, externalUserID}
	if orgID != "" {
		query += ` AND org_id = ?`
		args = append(args, orgID)
	}
	query += ` LIMIT 1`

	row := q.QueryRowContext(ctx, query, args...)

	m, err := scanMember(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Member{}, false, nil
		}
		return Member{}, false, fmt.Errorf("recherche d'un membre par identité de messagerie: %w", err)
	}

	return m, true, nil
}
