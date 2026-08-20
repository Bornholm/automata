package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/secretbox"
)

// ConversationSummaryRepository donne accès à la table
// conversation_summaries.
type ConversationSummaryRepository struct {
	// cipher protège le résumé, qui condense tout ce qui s'est dit dans
	// la conversation : le laisser en clair viderait de son sens le
	// chiffrement des messages.
	cipher *secretbox.Box
}

// NewConversationSummaryRepository crée un ConversationSummaryRepository.
func NewConversationSummaryRepository(cipher *secretbox.Box) *ConversationSummaryRepository {
	return &ConversationSummaryRepository{cipher: cipher}
}

// Get retourne le résumé de la conversation, ou (ConversationSummary{},
// false, nil) s'il n'en existe pas encore.
func (r *ConversationSummaryRepository) Get(ctx context.Context, q Querier, conversationID model.ConversationID) (ConversationSummary, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT conversation_id, summary, last_message_rowid, messages_covered, updated_at
		FROM conversation_summaries
		WHERE conversation_id = ?
	`, conversationID)

	var s ConversationSummary
	if err := row.Scan(&s.ConversationID, &s.Summary, &s.LastMessageRowID, &s.MessagesCovered, &s.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConversationSummary{}, false, nil
		}
		return ConversationSummary{}, false, fmt.Errorf("lecture du résumé de la conversation %q: %w", conversationID, err)
	}

	summary, err := openContent(r.cipher, s.Summary)
	if err != nil {
		return ConversationSummary{}, false, fmt.Errorf("résumé de la conversation %q: %w", conversationID, err)
	}
	s.Summary = summary

	return s, true, nil
}

// Upsert insère ou remplace le résumé de la conversation.
func (r *ConversationSummaryRepository) Upsert(ctx context.Context, q Querier, s ConversationSummary) error {
	summary, err := sealContent(r.cipher, s.Summary)
	if err != nil {
		return fmt.Errorf("résumé de la conversation %q: %w", s.ConversationID, err)
	}
	s.Summary = summary

	_, err = q.ExecContext(ctx, `
		INSERT INTO conversation_summaries (conversation_id, summary, last_message_rowid, messages_covered, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (conversation_id) DO UPDATE SET
			summary = excluded.summary,
			last_message_rowid = excluded.last_message_rowid,
			messages_covered = excluded.messages_covered,
			updated_at = excluded.updated_at
	`, s.ConversationID, s.Summary, s.LastMessageRowID, s.MessagesCovered, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("écriture du résumé de la conversation %q: %w", s.ConversationID, err)
	}
	return nil
}
