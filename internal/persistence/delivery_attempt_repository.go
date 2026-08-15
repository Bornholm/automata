package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DeliveryAttemptRepository donne accès à la table delivery_attempts.
type DeliveryAttemptRepository struct{}

// NewDeliveryAttemptRepository crée un DeliveryAttemptRepository.
func NewDeliveryAttemptRepository() *DeliveryAttemptRepository {
	return &DeliveryAttemptRepository{}
}

// Insert insère une tentative de livraison.
func (r *DeliveryAttemptRepository) Insert(ctx context.Context, q Querier, d DeliveryAttempt) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO delivery_attempts (
			id, scheduled_run_id, provider, channel_id, attempt, status, error_code, created_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, d.ID, d.ScheduledRunID, d.Provider, d.ChannelID, d.Attempt, d.Status, d.ErrorCode, d.CreatedAt, d.CompletedAt)
	if err != nil {
		return fmt.Errorf("insertion de la tentative de livraison %q: %w", d.ID, err)
	}
	return nil
}

// FindByID retourne la tentative de livraison identifiée par id, ou
// (DeliveryAttempt{}, false, nil) si elle n'existe pas.
func (r *DeliveryAttemptRepository) FindByID(ctx context.Context, q Querier, id DeliveryAttemptID) (DeliveryAttempt, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, scheduled_run_id, provider, channel_id, attempt, status, error_code, created_at, completed_at
		FROM delivery_attempts
		WHERE id = ?
	`, id)

	var d DeliveryAttempt
	if err := row.Scan(&d.ID, &d.ScheduledRunID, &d.Provider, &d.ChannelID, &d.Attempt, &d.Status, &d.ErrorCode, &d.CreatedAt, &d.CompletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveryAttempt{}, false, nil
		}
		return DeliveryAttempt{}, false, fmt.Errorf("lecture de la tentative de livraison %q: %w", id, err)
	}

	return d, true, nil
}
