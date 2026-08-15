package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ScheduledRunRepository donne accès à la table scheduled_runs.
type ScheduledRunRepository struct{}

// NewScheduledRunRepository crée un ScheduledRunRepository.
func NewScheduledRunRepository() *ScheduledRunRepository {
	return &ScheduledRunRepository{}
}

// Insert insère une exécution planifiée.
func (r *ScheduledRunRepository) Insert(ctx context.Context, q Querier, s ScheduledRun) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO scheduled_runs (
			id, schedule_id, scheduled_for, started_at, completed_at, status, principal_id, org_id,
			scope, scope_id, agent_id, error_code, delivery_status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, s.ID, s.ScheduleID, s.ScheduledFor, s.StartedAt, s.CompletedAt, s.Status, s.PrincipalID, s.OrgID,
		s.Scope, s.ScopeID, s.AgentID, s.ErrorCode, s.DeliveryStatus, s.CreatedAt)
	if err != nil {
		return fmt.Errorf("insertion de l'exécution planifiée %q: %w", s.ID, err)
	}
	return nil
}

// FindByID retourne l'exécution planifiée identifiée par id, ou
// (ScheduledRun{}, false, nil) si elle n'existe pas.
func (r *ScheduledRunRepository) FindByID(ctx context.Context, q Querier, id ScheduledRunID) (ScheduledRun, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, schedule_id, scheduled_for, started_at, completed_at, status, principal_id, org_id,
			scope, scope_id, agent_id, error_code, delivery_status, created_at
		FROM scheduled_runs
		WHERE id = ?
	`, id)

	return scanScheduledRun(row)
}

// FindByScheduleAndScheduledFor retourne l'exécution planifiée associée à
// (scheduleID, scheduledFor), ou (ScheduledRun{}, false, nil) si elle
// n'existe pas.
func (r *ScheduledRunRepository) FindByScheduleAndScheduledFor(ctx context.Context, q Querier, scheduleID, scheduledFor string) (ScheduledRun, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, schedule_id, scheduled_for, started_at, completed_at, status, principal_id, org_id,
			scope, scope_id, agent_id, error_code, delivery_status, created_at
		FROM scheduled_runs
		WHERE schedule_id = ? AND scheduled_for = ?
	`, scheduleID, scheduledFor)

	return scanScheduledRun(row)
}

// FindLatestByScheduleID retourne l'exécution planifiée la plus récente
// (par scheduled_for) pour scheduleID, ou (ScheduledRun{}, false, nil) si
// aucune n'existe encore. Utilisé par internal/scheduler pour reprendre le
// calcul des occurrences après la dernière connue (PLAN.md §11.5).
func (r *ScheduledRunRepository) FindLatestByScheduleID(ctx context.Context, q Querier, scheduleID string) (ScheduledRun, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, schedule_id, scheduled_for, started_at, completed_at, status, principal_id, org_id,
			scope, scope_id, agent_id, error_code, delivery_status, created_at
		FROM scheduled_runs
		WHERE schedule_id = ?
		ORDER BY scheduled_for DESC
		LIMIT 1
	`, scheduleID)

	return scanScheduledRun(row)
}

// FindRunningByScheduleID retourne une exécution planifiée non terminale
// (status = "running") pour scheduleID, ou (ScheduledRun{}, false, nil) si
// aucune n'est en cours. Utilisé pour appliquer la politique de concurrence
// "forbid" (PLAN.md §11.4).
func (r *ScheduledRunRepository) FindRunningByScheduleID(ctx context.Context, q Querier, scheduleID string) (ScheduledRun, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, schedule_id, scheduled_for, started_at, completed_at, status, principal_id, org_id,
			scope, scope_id, agent_id, error_code, delivery_status, created_at
		FROM scheduled_runs
		WHERE schedule_id = ? AND status = 'running'
		LIMIT 1
	`, scheduleID)

	return scanScheduledRun(row)
}

// UpdateStatus marque le statut final d'une exécution planifiée déjà
// insérée (succeeded ou failed), avec sa date de complétion et un éventuel
// code d'erreur.
func (r *ScheduledRunRepository) UpdateStatus(ctx context.Context, q Querier, id ScheduledRunID, status string, completedAt *string, errorCode *string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE scheduled_runs
		SET status = ?, completed_at = ?, error_code = ?
		WHERE id = ?
	`, status, completedAt, errorCode, id)
	if err != nil {
		return fmt.Errorf("mise à jour du statut de l'exécution planifiée %q: %w", id, err)
	}
	return nil
}

// UpdateDeliveryStatus met à jour le statut de livraison d'une exécution
// planifiée déjà terminée, sans toucher à son statut d'exécution
// (PLAN.md §11.6 : exécution et livraison sont deux étapes séparées).
func (r *ScheduledRunRepository) UpdateDeliveryStatus(ctx context.Context, q Querier, id ScheduledRunID, deliveryStatus string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE scheduled_runs
		SET delivery_status = ?
		WHERE id = ?
	`, deliveryStatus, id)
	if err != nil {
		return fmt.Errorf("mise à jour du statut de livraison de l'exécution planifiée %q: %w", id, err)
	}
	return nil
}

func scanScheduledRun(row *sql.Row) (ScheduledRun, bool, error) {
	var s ScheduledRun
	if err := row.Scan(&s.ID, &s.ScheduleID, &s.ScheduledFor, &s.StartedAt, &s.CompletedAt, &s.Status, &s.PrincipalID, &s.OrgID,
		&s.Scope, &s.ScopeID, &s.AgentID, &s.ErrorCode, &s.DeliveryStatus, &s.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ScheduledRun{}, false, nil
		}
		return ScheduledRun{}, false, fmt.Errorf("lecture de l'exécution planifiée: %w", err)
	}

	return s, true, nil
}
