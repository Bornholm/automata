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
