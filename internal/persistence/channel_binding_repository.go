package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ChannelBinding est le DTO de la table channel_bindings (migration
// 0011) : un canal rattaché à une organisation par consommation d'un
// jeton, hors configuration YAML.
type ChannelBinding struct {
	Provider    string
	ChannelID   string
	OrgID       string
	Kind        string
	Scope       string
	ScopeID     string
	MemberID    string
	DisplayName string
	CreatedAt   time.Time
}

// ChannelBindingRepository donne accès à la table channel_bindings.
type ChannelBindingRepository struct{}

// NewChannelBindingRepository crée un ChannelBindingRepository.
func NewChannelBindingRepository() *ChannelBindingRepository {
	return &ChannelBindingRepository{}
}

const channelBindingColumns = `provider, channel_id, org_id, kind, scope, scope_id, member_id, display_name, created_at`

// Upsert enregistre ou remplace la liaison (un canal relié à nouveau
// écrase la liaison précédente : la dernière consommation de jeton fait
// foi).
func (r *ChannelBindingRepository) Upsert(ctx context.Context, q Querier, b ChannelBinding) error {
	_, err := q.ExecContext(ctx, `INSERT INTO channel_bindings
		(`+channelBindingColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, channel_id) DO UPDATE SET
			org_id = excluded.org_id, kind = excluded.kind, scope = excluded.scope,
			scope_id = excluded.scope_id, member_id = excluded.member_id,
			display_name = excluded.display_name`,
		b.Provider, b.ChannelID, b.OrgID, b.Kind, b.Scope, b.ScopeID,
		b.MemberID, b.DisplayName, formatTenantTime(b.CreatedAt))
	if err != nil {
		return fmt.Errorf("enregistrement de la liaison de canal %s/%s: %w", b.Provider, b.ChannelID, err)
	}

	return nil
}

// Delete détache un canal D'UNE ORGANISATION PRÉCISE. Le filtre par
// org_id est le cloisonnement : un identifiant de canal recopié depuis la
// fiche d'une autre organisation ne détache rien. Retourne false si aucune
// liaison ne correspondait.
//
// La conversation et son historique restent en place. Détacher veut dire
// qu'Automata cesse d'y répondre, pas qu'on efface ce qui s'y est dit ;
// l'effacement a ses propres boutons, sur le membre ou l'organisation.
func (r *ChannelBindingRepository) Delete(ctx context.Context, q Querier, orgID, provider, channelID string) (bool, error) {
	result, err := q.ExecContext(ctx, `DELETE FROM channel_bindings
		WHERE org_id = ? AND provider = ? AND channel_id = ?`, orgID, provider, channelID)
	if err != nil {
		return false, fmt.Errorf("détachement du canal %s/%s: %w", provider, channelID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("détachement du canal %s/%s: %w", provider, channelID, err)
	}

	return affected > 0, nil
}

// Find retourne la liaison d'un canal, ou (ChannelBinding{}, false, nil).
func (r *ChannelBindingRepository) Find(ctx context.Context, q Querier, provider, channelID string) (ChannelBinding, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+channelBindingColumns+` FROM channel_bindings
		WHERE provider = ? AND channel_id = ?`, provider, channelID)

	var (
		b         ChannelBinding
		createdAt string
	)
	err := row.Scan(&b.Provider, &b.ChannelID, &b.OrgID, &b.Kind, &b.Scope, &b.ScopeID,
		&b.MemberID, &b.DisplayName, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelBinding{}, false, nil
		}
		return ChannelBinding{}, false, fmt.Errorf("lecture de la liaison de canal %s/%s: %w", provider, channelID, err)
	}

	if b.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return ChannelBinding{}, false, err
	}

	return b, true, nil
}

// ListByOrg retourne les liaisons d'une organisation.
func (r *ChannelBindingRepository) ListByOrg(ctx context.Context, q Querier, orgID string) ([]ChannelBinding, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+channelBindingColumns+` FROM channel_bindings
		WHERE org_id = ? ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("liaisons de canaux de %q: %w", orgID, err)
	}
	defer rows.Close()

	var bindings []ChannelBinding
	for rows.Next() {
		var (
			b         ChannelBinding
			createdAt string
		)
		if err := rows.Scan(&b.Provider, &b.ChannelID, &b.OrgID, &b.Kind, &b.Scope, &b.ScopeID,
			&b.MemberID, &b.DisplayName, &createdAt); err != nil {
			return nil, fmt.Errorf("lecture d'une liaison de canal: %w", err)
		}
		if b.CreatedAt, err = parseTenantTime(createdAt); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des liaisons de canaux: %w", err)
	}

	return bindings, nil
}

// ListAll retourne toutes les liaisons (écran des canaux et plateformes).
func (r *ChannelBindingRepository) ListAll(ctx context.Context, q Querier) ([]ChannelBinding, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+channelBindingColumns+` FROM channel_bindings ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("liaisons de canaux: %w", err)
	}
	defer rows.Close()

	var bindings []ChannelBinding
	for rows.Next() {
		var (
			b         ChannelBinding
			createdAt string
		)
		if err := rows.Scan(&b.Provider, &b.ChannelID, &b.OrgID, &b.Kind, &b.Scope, &b.ScopeID,
			&b.MemberID, &b.DisplayName, &createdAt); err != nil {
			return nil, fmt.Errorf("lecture d'une liaison de canal: %w", err)
		}
		if b.CreatedAt, err = parseTenantTime(createdAt); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des liaisons de canaux: %w", err)
	}

	return bindings, nil
}
