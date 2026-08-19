package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProfileLinkRepository donne accès à la table profile_links (migration
// 0010) : les liens de profil temporaires à usage unique (PRO-*). Même
// politique que link_tokens : seul le hachage du secret est stocké.
type ProfileLinkRepository struct{}

// NewProfileLinkRepository crée un ProfileLinkRepository.
func NewProfileLinkRepository() *ProfileLinkRepository {
	return &ProfileLinkRepository{}
}

const profileLinkColumns = `id, member_id, token_hash, status, expires_at, opened_at, created_at`

// Insert enregistre l.
func (r *ProfileLinkRepository) Insert(ctx context.Context, q Querier, l ProfileLink) error {
	_, err := q.ExecContext(ctx, `INSERT INTO profile_links
		(`+profileLinkColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.MemberID, l.TokenHash, l.Status,
		formatTenantTime(l.ExpiresAt), formatTenantTime(l.OpenedAt), formatTenantTime(l.CreatedAt))
	if err != nil {
		return fmt.Errorf("insertion du lien de profil %q: %w", l.ID, err)
	}

	return nil
}

// FindByID retourne le lien, ou (ProfileLink{}, false, nil) s'il n'existe
// pas. L'appelant décide de l'état à présenter (en attente, ouvert,
// expiré) : les trois se rendent différemment (PRO-90).
func (r *ProfileLinkRepository) FindByID(ctx context.Context, q Querier, id string) (ProfileLink, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+profileLinkColumns+` FROM profile_links WHERE id = ?`, id)

	var (
		l                              ProfileLink
		expiresAt, openedAt, createdAt string
	)
	err := row.Scan(&l.ID, &l.MemberID, &l.TokenHash, &l.Status, &expiresAt, &openedAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProfileLink{}, false, nil
		}
		return ProfileLink{}, false, fmt.Errorf("lecture du lien de profil %q: %w", id, err)
	}

	if l.ExpiresAt, err = parseTenantTime(expiresAt); err != nil {
		return ProfileLink{}, false, err
	}
	if l.OpenedAt, err = parseTenantTime(openedAt); err != nil {
		return ProfileLink{}, false, err
	}
	if l.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return ProfileLink{}, false, err
	}

	return l, true, nil
}

// MarkOpened consomme le lien : pending → opened, atomique. Retourne false
// si le lien avait déjà été consommé entre-temps (PRO-90 « déjà utilisé »).
func (r *ProfileLinkRepository) MarkOpened(ctx context.Context, q Querier, id string, at time.Time) (bool, error) {
	res, err := q.ExecContext(ctx, `UPDATE profile_links SET status = ?, opened_at = ?
		WHERE id = ? AND status = ?`,
		ProfileLinkStatusOpened, formatTenantTime(at), id, ProfileLinkStatusPending)
	if err != nil {
		return false, fmt.Errorf("consommation du lien de profil %q: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("consommation du lien de profil %q: %w", id, err)
	}

	return n > 0, nil
}
