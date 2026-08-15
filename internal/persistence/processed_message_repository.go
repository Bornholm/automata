package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProcessedMessageRepository donne accès à la table processed_messages.
type ProcessedMessageRepository struct{}

// NewProcessedMessageRepository crée un ProcessedMessageRepository.
func NewProcessedMessageRepository() *ProcessedMessageRepository {
	return &ProcessedMessageRepository{}
}

// Insert insère une trace de message traité.
func (r *ProcessedMessageRepository) Insert(ctx context.Context, q Querier, m ProcessedMessage) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO processed_messages (provider, external_message_id, processed_at, status)
		VALUES (?, ?, ?, ?)
	`, m.Provider, m.ExternalMessageID, m.ProcessedAt, m.Status)
	if err != nil {
		return fmt.Errorf("insertion du message traité (%q, %q): %w", m.Provider, m.ExternalMessageID, err)
	}
	return nil
}

// FindByProviderAndExternalMessageID retourne la trace de traitement pour
// (provider, externalMessageID), ou (ProcessedMessage{}, false, nil) si elle
// n'existe pas.
func (r *ProcessedMessageRepository) FindByProviderAndExternalMessageID(ctx context.Context, q Querier, provider, externalMessageID string) (ProcessedMessage, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT provider, external_message_id, processed_at, status
		FROM processed_messages
		WHERE provider = ? AND external_message_id = ?
	`, provider, externalMessageID)

	var m ProcessedMessage
	if err := row.Scan(&m.Provider, &m.ExternalMessageID, &m.ProcessedAt, &m.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProcessedMessage{}, false, nil
		}
		return ProcessedMessage{}, false, fmt.Errorf("lecture du message traité (%q, %q): %w", provider, externalMessageID, err)
	}

	return m, true, nil
}
