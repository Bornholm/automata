package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
)

// ConversationSummaryRepository donne accès à la table
// conversation_summaries.
type ConversationSummaryRepository struct{}

// NewConversationSummaryRepository crée un ConversationSummaryRepository.
func NewConversationSummaryRepository() *ConversationSummaryRepository {
	return &ConversationSummaryRepository{}
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

	return s, true, nil
}

// Upsert insère ou remplace le résumé de la conversation.
func (r *ConversationSummaryRepository) Upsert(ctx context.Context, q Querier, s ConversationSummary) error {
	_, err := q.ExecContext(ctx, `
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
