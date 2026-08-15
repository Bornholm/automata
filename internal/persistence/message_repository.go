package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
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

// ListRecentByConversation retourne au plus limit messages de la
// conversation conversationID, dans l'ordre chronologique (les plus anciens
// en premier). L'ordre d'insertion (rowid) fait foi plutôt que created_at
// seul, afin de rester stable même en cas de messages insérés dans la même
// seconde.
func (r *MessageRepository) ListRecentByConversation(ctx context.Context, q Querier, conversationID model.ConversationID, limit int) ([]Message, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
		FROM (
			SELECT rowid, id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
			FROM messages
			WHERE conversation_id = ?
			ORDER BY rowid DESC
			LIMIT ?
		)
		ORDER BY rowid ASC
	`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("lecture de l'historique de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	messages := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.ExternalMessageID, &m.PrincipalID, &m.Role, &m.Content, &m.ContentKind, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("lecture de l'historique de la conversation %q: %w", conversationID, err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lecture de l'historique de la conversation %q: %w", conversationID, err)
	}

	return messages, nil
}
