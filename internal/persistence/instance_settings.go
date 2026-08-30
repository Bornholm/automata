package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Réglages d'instance et journal d'alertes (migration 0026).

// SettingOperatorMemberID désigne le membre qui reçoit les alertes
// d'exploitation. Vide ou absent = personne n'est alerté.
const SettingOperatorMemberID = "operator_member_id"

// InstanceSettingRepository lit et écrit les réglages d'instance.
type InstanceSettingRepository struct{}

// NewInstanceSettingRepository crée un InstanceSettingRepository.
func NewInstanceSettingRepository() *InstanceSettingRepository {
	return &InstanceSettingRepository{}
}

// Get retourne la valeur d'un réglage ; une clé absente rend la chaîne vide
// sans erreur — un réglage jamais posé et un réglage vidé se valent.
func (r *InstanceSettingRepository) Get(ctx context.Context, q Querier, key string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM instance_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lecture du réglage %q: %w", key, err)
	}

	return value, nil
}

// Set enregistre un réglage.
func (r *InstanceSettingRepository) Set(ctx context.Context, q Querier, key, value string, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO instance_settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, formatTenantTime(now))
	if err != nil {
		return fmt.Errorf("écriture du réglage %q: %w", key, err)
	}

	return nil
}

// Alert est une alerte d'exploitation.
type Alert struct {
	ID      string
	Kind    string
	Subject string
	Message string
	// CreatedAt est l'instant où le problème a été constaté.
	CreatedAt time.Time
	// DeliveredAt zéro = jamais remise à l'exploitant.
	DeliveredAt time.Time
}

// Delivered indique si l'alerte a atteint son destinataire.
func (a Alert) Delivered() bool { return !a.DeliveredAt.IsZero() }

// AlertRepository gère le journal des alertes.
type AlertRepository struct{}

// NewAlertRepository crée un AlertRepository.
func NewAlertRepository() *AlertRepository {
	return &AlertRepository{}
}

const alertColumns = `id, kind, subject, message, created_at, delivered_at`

// Insert enregistre une alerte.
func (r *AlertRepository) Insert(ctx context.Context, q Querier, a Alert) error {
	_, err := q.ExecContext(ctx, `INSERT INTO alerts (`+alertColumns+`)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.Kind, a.Subject, a.Message,
		formatTenantTime(a.CreatedAt), formatTenantTime(a.DeliveredAt))
	if err != nil {
		return fmt.Errorf("enregistrement de l'alerte %q: %w", a.ID, err)
	}

	return nil
}

// MarkDelivered note qu'une alerte a été remise.
func (r *AlertRepository) MarkDelivered(ctx context.Context, q Querier, id string, now time.Time) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE alerts SET delivered_at = ? WHERE id = ?`, formatTenantTime(now), id); err != nil {
		return fmt.Errorf("remise de l'alerte %q: %w", id, err)
	}

	return nil
}

// LastOf retourne la date de la dernière alerte de même nature et même
// sujet, ou le temps zéro. C'est l'assiette de la déduplication : elle
// survit au redémarrage, là où un compteur en mémoire recommencerait à
// zéro — et un processus qui redémarre en boucle enverrait alors une
// alerte à chaque tour.
func (r *AlertRepository) LastOf(ctx context.Context, q Querier, kind, subject string) (time.Time, error) {
	var raw string
	err := q.QueryRowContext(ctx,
		`SELECT created_at FROM alerts WHERE kind = ? AND subject = ?
		 ORDER BY created_at DESC LIMIT 1`, kind, subject).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("dernière alerte %q/%q: %w", kind, subject, err)
	}

	return parseTenantTime(raw)
}

// ListPending retourne les alertes jamais remises, de la plus ancienne à la
// plus récente.
func (r *AlertRepository) ListPending(ctx context.Context, q Querier, limit int) ([]Alert, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+alertColumns+` FROM alerts
		WHERE delivered_at = '' ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("alertes en attente: %w", err)
	}
	defer rows.Close()

	return scanAlerts(rows)
}

// ListRecent retourne les dernières alertes, de la plus récente à la plus
// ancienne — l'écran d'administration.
func (r *AlertRepository) ListRecent(ctx context.Context, q Querier, limit int) ([]Alert, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+alertColumns+` FROM alerts
		ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("alertes récentes: %w", err)
	}
	defer rows.Close()

	return scanAlerts(rows)
}

// DeleteBefore purge les alertes antérieures à cutoff, et retourne le
// nombre supprimé.
func (r *AlertRepository) DeleteBefore(ctx context.Context, q Querier, cutoff time.Time) (int64, error) {
	result, err := q.ExecContext(ctx,
		`DELETE FROM alerts WHERE created_at < ?`, formatTenantTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge des alertes: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge des alertes: %w", err)
	}

	return deleted, nil
}

func scanAlerts(rows *sql.Rows) ([]Alert, error) {
	var alerts []Alert
	for rows.Next() {
		var (
			a                      Alert
			createdAt, deliveredAt string
		)
		if err := rows.Scan(&a.ID, &a.Kind, &a.Subject, &a.Message, &createdAt, &deliveredAt); err != nil {
			return nil, fmt.Errorf("lecture d'une alerte: %w", err)
		}

		var err error
		if a.CreatedAt, err = parseTenantTime(createdAt); err != nil {
			return nil, err
		}
		if a.DeliveredAt, err = parseTenantTime(deliveredAt); err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des alertes: %w", err)
	}

	return alerts, nil
}
