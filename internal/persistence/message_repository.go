package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/secretbox"
)

// MessageRepository donne accès à la table messages.
type MessageRepository struct {
	// cipher chiffre le contenu des messages avant écriture et le
	// déchiffre à la lecture. Nil : les contenus restent en clair, comme
	// avant l'existence du réglage.
	cipher *secretbox.Box
}

// NewMessageRepository crée un MessageRepository.
func NewMessageRepository(cipher *secretbox.Box) *MessageRepository {
	return &MessageRepository{cipher: cipher}
}

// Insert insère un message.
func (r *MessageRepository) Insert(ctx context.Context, q Querier, m Message) error {
	content, err := sealContent(r.cipher, m.Content)
	if err != nil {
		return err
	}
	m.Content = content

	_, err = q.ExecContext(ctx, `
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

	content, err := openContent(r.cipher, m.Content)
	if err != nil {
		return Message{}, false, fmt.Errorf("message %q: %w", id, err)
	}
	m.Content = content

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

		content, err := openContent(r.cipher, m.Content)
		if err != nil {
			return nil, fmt.Errorf("message %q: %w", m.ID, err)
		}
		m.Content = content

		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lecture de l'historique de la conversation %q: %w", conversationID, err)
	}

	return messages, nil
}

// ListRecentByConversationAfterRowID se comporte comme
// ListRecentByConversation mais ignore les messages de rowid inférieur ou
// égal à afterRowID — typiquement la frontière d'un résumé de compaction
// (conversation_summaries.last_message_rowid), pour ne jamais rejouer
// verbatim un message déjà condensé dans le résumé. afterRowID à 0 équivaut
// à ListRecentByConversation.
func (r *MessageRepository) ListRecentByConversationAfterRowID(ctx context.Context, q Querier, conversationID model.ConversationID, afterRowID int64, limit int) ([]Message, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
		FROM (
			SELECT rowid, id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
			FROM messages
			WHERE conversation_id = ? AND rowid > ?
			ORDER BY rowid DESC
			LIMIT ?
		)
		ORDER BY rowid ASC
	`, conversationID, afterRowID, limit)
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

		content, err := openContent(r.cipher, m.Content)
		if err != nil {
			return nil, fmt.Errorf("message %q: %w", m.ID, err)
		}
		m.Content = content

		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lecture de l'historique de la conversation %q: %w", conversationID, err)
	}

	return messages, nil
}

// CountByConversationAfterRowID compte les messages de la conversation dont
// le rowid est strictement supérieur à afterRowID.
func (r *MessageRepository) CountByConversationAfterRowID(ctx context.Context, q Querier, conversationID model.ConversationID, afterRowID int64) (int64, error) {
	row := q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM messages
		WHERE conversation_id = ? AND rowid > ?
	`, conversationID, afterRowID)

	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("comptage des messages de la conversation %q: %w", conversationID, err)
	}

	return count, nil
}

// ListOldestByConversationAfterRowID retourne les limit plus anciens
// messages de la conversation dont le rowid est strictement supérieur à
// afterRowID, dans l'ordre chronologique, ainsi que le rowid du dernier
// d'entre eux (0 si aucun) — la future frontière du résumé de compaction.
func (r *MessageRepository) ListOldestByConversationAfterRowID(ctx context.Context, q Querier, conversationID model.ConversationID, afterRowID int64, limit int) ([]Message, int64, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT rowid, id, conversation_id, external_message_id, principal_id, role, content, content_kind, created_at
		FROM messages
		WHERE conversation_id = ? AND rowid > ?
		ORDER BY rowid ASC
		LIMIT ?
	`, conversationID, afterRowID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("lecture des messages à compacter de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	var (
		messages  = make([]Message, 0, limit)
		lastRowID int64
	)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&lastRowID, &m.ID, &m.ConversationID, &m.ExternalMessageID, &m.PrincipalID, &m.Role, &m.Content, &m.ContentKind, &m.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("lecture des messages à compacter de la conversation %q: %w", conversationID, err)
		}

		content, err := openContent(r.cipher, m.Content)
		if err != nil {
			return nil, 0, fmt.Errorf("message %q: %w", m.ID, err)
		}
		m.Content = content

		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("lecture des messages à compacter de la conversation %q: %w", conversationID, err)
	}

	return messages, lastRowID, nil
}
