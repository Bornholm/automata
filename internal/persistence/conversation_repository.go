package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
)

// ConversationRepository donne accès à la table conversations.
type ConversationRepository struct{}

// NewConversationRepository crée un ConversationRepository.
func NewConversationRepository() *ConversationRepository {
	return &ConversationRepository{}
}

// Insert insère une conversation.
func (r *ConversationRepository) Insert(ctx context.Context, q Querier, c Conversation) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO conversations (
			id, org_id, provider, external_channel_id, kind, scope, scope_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.ID, c.OrgID, c.Provider, c.ExternalChannelID, c.Kind, c.Scope, c.ScopeID, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insertion de la conversation %q: %w", c.ID, err)
	}
	return nil
}

// FindByID retourne la conversation identifiée par id, ou
// (Conversation{}, false, nil) si elle n'existe pas.
func (r *ConversationRepository) FindByID(ctx context.Context, q Querier, id model.ConversationID) (Conversation, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, provider, external_channel_id, kind, scope, scope_id, created_at, updated_at
		FROM conversations
		WHERE id = ?
	`, id)

	var c Conversation
	if err := row.Scan(&c.ID, &c.OrgID, &c.Provider, &c.ExternalChannelID, &c.Kind, &c.Scope, &c.ScopeID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Conversation{}, false, nil
		}
		return Conversation{}, false, fmt.Errorf("lecture de la conversation %q: %w", id, err)
	}

	return c, true, nil
}

// FindByProviderAndExternalChannelID retourne la conversation associée à
// (provider, externalChannelID), ou (Conversation{}, false, nil) si elle
// n'existe pas.
func (r *ConversationRepository) FindByProviderAndExternalChannelID(ctx context.Context, q Querier, provider, externalChannelID string) (Conversation, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, provider, external_channel_id, kind, scope, scope_id, created_at, updated_at
		FROM conversations
		WHERE provider = ? AND external_channel_id = ?
	`, provider, externalChannelID)

	var c Conversation
	if err := row.Scan(&c.ID, &c.OrgID, &c.Provider, &c.ExternalChannelID, &c.Kind, &c.Scope, &c.ScopeID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Conversation{}, false, nil
		}
		return Conversation{}, false, fmt.Errorf("lecture de la conversation (%q, %q): %w", provider, externalChannelID, err)
	}

	return c, true, nil
}
