package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MessageRepository donne accès à la table messages.
type MessageRepository struct{}

// NewMessageRepository crée un MessageRepository.
func NewMessageRepository() *MessageRepository {
	return &MessageRepository{}
}

// Insert insère un message.
func (r *MessageRepository) Insert(ctx context.Context, q Querier, m Message) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO messages (
			id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.ConversationID, m.ExternalMessageID, m.PrincipalID, m.Role, m.Content, m.ContentKind, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("insertion du message %q: %w", m.ID, err)
	}
	return nil
}

// FindByID retourne le message identifié par id, ou (Message{}, false, nil)
// s'il n'existe pas.
func (r *MessageRepository) FindByID(ctx context.Context, q Querier, id string) (Message, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
		FROM messages
		WHERE id = ?
	`, id)

	var m Message
	if err := row.Scan(&m.ID, &m.ConversationID, &m.ExternalMessageID, &m.PrincipalID, &m.Role, &m.Content, &m.ContentKind, &m.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Message{}, false, nil
		}
		return Message{}, false, fmt.Errorf("lecture du message %q: %w", id, err)
	}

	return m, true, nil
}
