package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bornholm/automata/internal/secretbox"
)

// Statuts d'un rappel (table reminders). Un rappel naît pending, puis passe
// une seule fois à sent, cancelled ou failed — jamais de retour en arrière.
const (
	ReminderStatusPending   = "pending"
	ReminderStatusSent      = "sent"
	ReminderStatusCancelled = "cancelled"
	ReminderStatusFailed    = "failed"
)

// Natures d'une entrée de la table reminders. Un rappel délivre son message
// tel quel ; une tâche le donne comme consigne à un agent et délivre la
// réponse (voir migration 0007).
const (
	ReminderKindMessage = "message"
	ReminderKindTask    = "task"
)

// ReminderRepository donne accès à la table reminders.
type ReminderRepository struct {
	// cipher protège le texte du rappel, qui est du contenu écrit par la
	// personne ou par l'agent pour elle.
	cipher *secretbox.Box
}

// NewReminderRepository crée un ReminderRepository.
func NewReminderRepository(cipher *secretbox.Box) *ReminderRepository {
	return &ReminderRepository{cipher: cipher}
}

// Insert insère un rappel ou une tâche planifiée.
//
// Une nature vide vaut ReminderKindMessage : c'est le comportement
// historique, et une entrée dont la nature ne correspond à rien serait
// invisible de tous les outils de liste comme du dispatcher.
func (r *ReminderRepository) Insert(ctx context.Context, q Querier, rem Reminder) error {
	if rem.Kind == "" {
		rem.Kind = ReminderKindMessage
	}

	message, err := sealContent(r.cipher, rem.Message)
	if err != nil {
		return fmt.Errorf("rappel %q: %w", rem.ID, err)
	}
	rem.Message = message

	_, err = q.ExecContext(ctx, `
		INSERT INTO reminders (id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone, kind, agent_id, attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rem.ID, rem.OrgID, rem.PrincipalID, rem.ConversationID, rem.Provider, rem.ChannelID, rem.Message, rem.FireAt, rem.Status, rem.CreatedAt, rem.SentAt, rem.Recurrence, rem.Timezone, rem.Kind, rem.AgentID, rem.Attempts)
	if err != nil {
		return fmt.Errorf("insertion du rappel %q: %w", rem.ID, err)
	}
	return nil
}

// FindByID retourne le rappel id, ou (Reminder{}, false, nil) s'il n'existe
// pas.
func (r *ReminderRepository) FindByID(ctx context.Context, q Querier, id ReminderID) (Reminder, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone, kind, agent_id, attempts
		FROM reminders
		WHERE id = ?
	`, id)

	rem, err := r.scanReminder(row)
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
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone, kind, agent_id, attempts
		FROM reminders
		WHERE status = ? AND fire_at <= ?
		ORDER BY fire_at ASC
		LIMIT ?
	`, ReminderStatusPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("liste des rappels dus: %w", err)
	}
	defer rows.Close()

	return r.collectReminders(rows)
}

// ListPendingByConversation retourne les entrées pending d'une conversation
// pour une nature donnée (ReminderKindMessage ou ReminderKindTask), par
// échéance croissante.
//
// Le filtre par nature n'est pas cosmétique : list_reminders et
// list_scheduled_tasks sont deux outils distincts, et un rappel qui
// apparaîtrait dans la liste des tâches serait annulable par
// cancel_scheduled_task — une confusion que rien ne rattraperait ensuite.
func (r *ReminderRepository) ListPendingByConversation(ctx context.Context, q Querier, conversationID, kind string) ([]Reminder, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone, kind, agent_id, attempts
		FROM reminders
		WHERE conversation_id = ? AND status = ? AND kind = ?
		ORDER BY fire_at ASC
	`, conversationID, ReminderStatusPending, kind)
	if err != nil {
		return nil, fmt.Errorf("liste des rappels de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	return r.collectReminders(rows)
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
		SET fire_at = ?, sent_at = NULL, attempts = 0
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

// RetryLater reprogramme une tentative de livraison après un échec :
// l'entrée reste pending, son échéance avance sur nextFireAt et son compteur
// de tentatives est porté à attempts. ok vaut false si l'entrée n'est plus
// pending (annulée pendant la livraison) — rien n'est alors écrasé, comme
// pour RescheduleNext.
func (r *ReminderRepository) RetryLater(ctx context.Context, q Querier, id ReminderID, nextFireAt string, attempts int) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE reminders
		SET fire_at = ?, attempts = ?
		WHERE id = ? AND status = ?
	`, nextFireAt, attempts, id, ReminderStatusPending)
	if err != nil {
		return false, fmt.Errorf("reprogrammation de la tentative du rappel %q: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reprogrammation de la tentative du rappel %q: %w", id, err)
	}

	return affected == 1, nil
}

func (r *ReminderRepository) scanReminder(row *sql.Row) (Reminder, error) {
	var rem Reminder
	if err := row.Scan(&rem.ID, &rem.OrgID, &rem.PrincipalID, &rem.ConversationID, &rem.Provider, &rem.ChannelID, &rem.Message, &rem.FireAt, &rem.Status, &rem.CreatedAt, &rem.SentAt, &rem.Recurrence, &rem.Timezone, &rem.Kind, &rem.AgentID, &rem.Attempts); err != nil {
		return rem, err
	}

	message, err := openContent(r.cipher, rem.Message)
	if err != nil {
		return rem, fmt.Errorf("rappel %q: %w", rem.ID, err)
	}
	rem.Message = message

	return rem, nil
}

func (r *ReminderRepository) collectReminders(rows *sql.Rows) ([]Reminder, error) {
	var reminders []Reminder
	for rows.Next() {
		var rem Reminder
		if err := rows.Scan(&rem.ID, &rem.OrgID, &rem.PrincipalID, &rem.ConversationID, &rem.Provider, &rem.ChannelID, &rem.Message, &rem.FireAt, &rem.Status, &rem.CreatedAt, &rem.SentAt, &rem.Recurrence, &rem.Timezone, &rem.Kind, &rem.AgentID, &rem.Attempts); err != nil {
			return nil, fmt.Errorf("lecture d'un rappel: %w", err)
		}

		message, err := openContent(r.cipher, rem.Message)
		if err != nil {
			return nil, fmt.Errorf("rappel %q: %w", rem.ID, err)
		}
		rem.Message = message

		reminders = append(reminders, rem)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des rappels: %w", err)
	}
	return reminders, nil
}

// ListRecentByConversation retourne les rappels de la conversation dont
// l'échéance tombe dans la fenêtre [since, now+horizon), quel que soit
// leur statut, du plus récent au plus ancien.
//
// C'est la vue dont l'assistant a besoin pour se relire : ListPending ne
// montre que ce qui reste à faire, et un rappel déjà envoyé en disparaît
// — ce qui lui faisait conclure qu'il n'avait jamais rien programmé, en
// contradiction avec ce que l'utilisateur venait de recevoir.
func (r *ReminderRepository) ListRecentByConversation(ctx context.Context, q Querier, conversationID string, since string, limit int) ([]Reminder, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone, kind, agent_id, attempts
		FROM reminders
		WHERE conversation_id = ? AND (fire_at >= ? OR status = ?)
		ORDER BY fire_at DESC
		LIMIT ?
	`, conversationID, since, ReminderStatusPending, limit)
	if err != nil {
		return nil, fmt.Errorf("historique des rappels de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	return r.collectReminders(rows)
}

// FindPendingDuplicate cherche un rappel en attente identique : même
// conversation, même nature, même échéance et même texte. Il n'existe pas
// de contrainte d'unicité en base — deux rappels identiques sont
// légitimes s'ils sont voulus — mais l'assistant qui reformule sa réponse
// au tour suivant en recrée un sans le vouloir.
func (r *ReminderRepository) FindPendingDuplicate(ctx context.Context, q Querier, conversationID, kind, fireAt, message string) (Reminder, bool, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, principal_id, conversation_id, provider, channel_id, message, fire_at, status, created_at, sent_at, recurrence, timezone, kind, agent_id, attempts
		FROM reminders
		WHERE conversation_id = ? AND kind = ? AND status = ? AND fire_at = ?
		ORDER BY created_at ASC
	`, conversationID, kind, ReminderStatusPending, fireAt)
	if err != nil {
		return Reminder{}, false, fmt.Errorf("recherche d'un rappel en double: %w", err)
	}
	defer rows.Close()

	candidates, err := r.collectReminders(rows)
	if err != nil {
		return Reminder{}, false, err
	}

	// La comparaison se fait sur le texte DÉCHIFFRÉ, d'où ce filtrage en
	// Go plutôt qu'en SQL : le chiffrement est probabiliste, deux
	// scellements du même message ne se ressemblent pas.
	for _, candidate := range candidates {
		if sameReminderMessage(candidate.Message, message) {
			return candidate, true, nil
		}
	}

	return Reminder{}, false, nil
}

// sameReminderMessage compare deux textes de rappel en ignorant la casse
// et les espaces de bord : « Pense à recommander des couches » et « pense
// à recommander des couches  » sont le même rappel.
func sameReminderMessage(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
