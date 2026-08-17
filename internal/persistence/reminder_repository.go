package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Statuts d'un rappel (table reminders). Un rappel naît pending, puis passe
// une seule fois à sent, cancelled ou failed — jamais de retour en arrière.
const (
	ReminderStatusPending   = "pending"
	ReminderStatusSent      = "sent"
	ReminderStatusCancelled = "cancelled"
	ReminderStatusFailed    = "failed"
)

// ReminderRepository donne accès à la table reminders.
type ReminderRepository struct{}

// NewReminderRepository crée un ReminderRepository.
func NewReminderRepository() *ReminderRepository {
	return &ReminderRepository{}
}

// Insert insère un rappel.
func (r *ReminderRepository) Insert(ctx context.Context, q Querier, rem Reminder) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO reminders (id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rem.ID, rem.OrgID, rem.PrincipalID, rem.ConversationID, rem.Provider, rem.ChannelID, rem.Message, rem.FireAt, rem.Status, rem.CreatedAt, rem.SentAt, rem.Recurrence, rem.Timezone)
	if err != nil {
		return fmt.Errorf("insertion du rappel %q: %w", rem.ID, err)
	}
	return nil
}

// FindByID retourne le rappel id, ou (Reminder{}, false, nil) s'il n'existe
// pas.
func (r *ReminderRepository) FindByID(ctx context.Context, q Querier, id ReminderID) (Reminder, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone
		FROM reminders
		WHERE id = ?
	`, id)

	rem, err := scanReminder(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Reminder{}, false, nil
		}
		return Reminder{}, false, fmt.Errorf("lecture du rappel %q: %w", id, err)
	}

	return rem, true, nil
}

// ListDue retourne les rappels pending dont l'échéance est atteinte à
// l'instant now (RFC3339 UTC — la comparaison lexicographique équivaut à la
// comparaison temporelle pour ce format à fuseau fixe), les plus anciens
// d'abord, bornés à limit.
func (r *ReminderRepository) ListDue(ctx context.Context, q Querier, now string, limit int) ([]Reminder, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone
		FROM reminders
		WHERE status = ? AND fire_at <= ?
		ORDER BY fire_at ASC
		LIMIT ?
	`, ReminderStatusPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("liste des rappels dus: %w", err)
	}
	defer rows.Close()

	return collectReminders(rows)
}

// ListPendingByConversation retourne les rappels pending d'une conversation,
// par échéance croissante.
func (r *ReminderRepository) ListPendingByConversation(ctx context.Context, q Querier, conversationID string) ([]Reminder, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone
		FROM reminders
		WHERE conversation_id = ? AND status = ?
		ORDER BY fire_at ASC
	`, conversationID, ReminderStatusPending)
	if err != nil {
		return nil, fmt.Errorf("liste des rappels de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	return collectReminders(rows)
}

// UpdateStatus fait passer le rappel id du statut fromStatus à toStatus et
// renseigne sent_at (peut être nil). ok vaut false si le rappel n'était pas
// (ou plus) au statut attendu : c'est le verrou optimiste qui empêche
// d'envoyer un rappel annulé entre-temps, ou d'annuler un rappel déjà parti.
func (r *ReminderRepository) UpdateStatus(ctx context.Context, q Querier, id ReminderID, fromStatus, toStatus string, sentAt *string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE reminders
		SET status = ?, sent_at = ?
		WHERE id = ? AND status = ?
	`, toStatus, sentAt, id, fromStatus)
	if err != nil {
		return false, fmt.Errorf("mise à jour du statut du rappel %q: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mise à jour du statut du rappel %q: %w", id, err)
	}

	return affected == 1, nil
}

// RescheduleNext avance l'échéance d'un rappel récurrent sur nextFireAt en
// le laissant au statut pending. ok vaut false si le rappel n'est plus
// pending (annulé pendant la livraison) : la série s'arrête alors là, comme
// pour UpdateStatus.
func (r *ReminderRepository) RescheduleNext(ctx context.Context, q Querier, id ReminderID, nextFireAt string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE reminders
		SET fire_at = ?, sent_at = NULL
		WHERE id = ? AND status = ?
	`, nextFireAt, id, ReminderStatusPending)
	if err != nil {
		return false, fmt.Errorf("réarmement du rappel %q: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("réarmement du rappel %q: %w", id, err)
	}

	return affected == 1, nil
}

func scanReminder(row *sql.Row) (Reminder, error) {
	var rem Reminder
	err := row.Scan(&rem.ID, &rem.OrgID, &rem.PrincipalID, &rem.ConversationID, &rem.Provider, &rem.ChannelID, &rem.Message, &rem.FireAt, &rem.Status, &rem.CreatedAt, &rem.SentAt, &rem.Recurrence, &rem.Timezone)
	return rem, err
}

func collectReminders(rows *sql.Rows) ([]Reminder, error) {
	var reminders []Reminder
	for rows.Next() {
		var rem Reminder
		if err := rows.Scan(&rem.ID, &rem.OrgID, &rem.PrincipalID, &rem.ConversationID, &rem.Provider, &rem.ChannelID, &rem.Message, &rem.FireAt, &rem.Status, &rem.CreatedAt, &rem.SentAt, &rem.Recurrence, &rem.Timezone); err != nil {
			return nil, fmt.Errorf("lecture d'un rappel: %w", err)
		}
		reminders = append(reminders, rem)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des rappels: %w", err)
	}
	return reminders, nil
}
