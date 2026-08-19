package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LinkTokenRepository donne accès à la table link_tokens (migration 0010) :
// les jetons de liaison à usage unique (ADM-04, ADM-05). Seul le hachage du
// jeton est stocké — voir tenant_types.go.
type LinkTokenRepository struct{}

// NewLinkTokenRepository crée un LinkTokenRepository.
func NewLinkTokenRepository() *LinkTokenRepository {
	return &LinkTokenRepository{}
}

const linkTokenColumns = `id, kind, member_id, org_id, token_hash, status, expires_at, used_at, created_at`

// Insert enregistre t.
func (r *LinkTokenRepository) Insert(ctx context.Context, q Querier, t LinkToken) error {
	_, err := q.ExecContext(ctx, `INSERT INTO link_tokens
		(`+linkTokenColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Kind, t.MemberID, t.OrgID, t.TokenHash, t.Status,
		formatTenantTime(t.ExpiresAt), formatTenantTime(t.UsedAt), formatTenantTime(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("insertion du jeton de liaison %q: %w", t.ID, err)
	}

	return nil
}

func scanLinkToken(scan func(...any) error) (LinkToken, error) {
	var (
		t                            LinkToken
		expiresAt, usedAt, createdAt string
	)
	if err := scan(&t.ID, &t.Kind, &t.MemberID, &t.OrgID, &t.TokenHash, &t.Status,
		&expiresAt, &usedAt, &createdAt); err != nil {
		return LinkToken{}, err
	}

	var err error
	if t.ExpiresAt, err = parseTenantTime(expiresAt); err != nil {
		return LinkToken{}, err
	}
	if t.UsedAt, err = parseTenantTime(usedAt); err != nil {
		return LinkToken{}, err
	}
	if t.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return LinkToken{}, err
	}

	return t, nil
}

// LatestByMember retourne le jeton personnel le plus récent du membre
// (l'écran ADM-04 n'affiche que le dernier cycle de vie), ou
// (LinkToken{}, false, nil) s'il n'y en a aucun.
func (r *LinkTokenRepository) LatestByMember(ctx context.Context, q Querier, memberID string) (LinkToken, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+linkTokenColumns+` FROM link_tokens
		WHERE member_id = ? AND kind = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		memberID, LinkTokenKindPersonal)

	t, err := scanLinkToken(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LinkToken{}, false, nil
		}
		return LinkToken{}, false, fmt.Errorf("lecture du jeton du membre %q: %w", memberID, err)
	}

	return t, true, nil
}

// ListPendingGroup retourne les jetons de groupe encore en attente (ADM-05,
// « en attente de liaison »), les plus récents d'abord.
func (r *LinkTokenRepository) ListPendingGroup(ctx context.Context, q Querier) ([]LinkToken, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+linkTokenColumns+` FROM link_tokens
		WHERE kind = ? AND status = ? ORDER BY created_at DESC`,
		LinkTokenKindGroup, LinkTokenStatusPending)
	if err != nil {
		return nil, fmt.Errorf("liste des jetons de groupe en attente: %w", err)
	}
	defer rows.Close()

	var tokens []LinkToken
	for rows.Next() {
		t, err := scanLinkToken(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("lecture d'un jeton de groupe: %w", err)
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des jetons de groupe: %w", err)
	}

	return tokens, nil
}

// Revoke passe le jeton à l'état révoqué s'il est encore en attente.
func (r *LinkTokenRepository) Revoke(ctx context.Context, q Querier, id string) error {
	res, err := q.ExecContext(ctx, `UPDATE link_tokens SET status = ?
		WHERE id = ? AND status = ?`, LinkTokenStatusRevoked, id, LinkTokenStatusPending)
	if err != nil {
		return fmt.Errorf("révocation du jeton %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("jeton %q introuvable ou déjà consommé", id)
	}

	return nil
}

// RevokePendingByMember révoque tous les jetons personnels en attente du
// membre — préalable à une régénération : jamais deux jetons valides à la
// fois pour la même personne.
func (r *LinkTokenRepository) RevokePendingByMember(ctx context.Context, q Querier, memberID string) error {
	_, err := q.ExecContext(ctx, `UPDATE link_tokens SET status = ?
		WHERE member_id = ? AND kind = ? AND status = ?`,
		LinkTokenStatusRevoked, memberID, LinkTokenKindPersonal, LinkTokenStatusPending)
	if err != nil {
		return fmt.Errorf("révocation des jetons du membre %q: %w", memberID, err)
	}

	return nil
}

// FindPendingByHash retourne le jeton en attente correspondant au hachage,
// s'il existe et n'est pas périmé à now. C'est le point d'entrée de la
// consommation par l'ingress (lot B), déjà en place pour les tests.
func (r *LinkTokenRepository) FindPendingByHash(ctx context.Context, q Querier, tokenHash string, now time.Time) (LinkToken, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+linkTokenColumns+` FROM link_tokens
		WHERE token_hash = ? AND status = ?`, tokenHash, LinkTokenStatusPending)

	t, err := scanLinkToken(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LinkToken{}, false, nil
		}
		return LinkToken{}, false, fmt.Errorf("recherche d'un jeton par hachage: %w", err)
	}

	if t.Expired(now) {
		return LinkToken{}, false, nil
	}

	return t, true, nil
}
