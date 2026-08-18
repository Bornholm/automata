package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaintenanceRunRepository donne accès à la table maintenance_runs : la
// dernière exécution réussie de chaque tâche de maintenance périodique
// (voir la migration 0006).
type MaintenanceRunRepository struct{}

// NewMaintenanceRunRepository crée un MaintenanceRunRepository.
func NewMaintenanceRunRepository() *MaintenanceRunRepository {
	return &MaintenanceRunRepository{}
}

// GetLastRun retourne l'horodatage de la dernière exécution réussie de la
// tâche, ou (time.Time{}, false, nil) si la tâche n'a jamais tourné.
func (r *MaintenanceRunRepository) GetLastRun(ctx context.Context, q Querier, task string) (time.Time, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT last_run_at FROM maintenance_runs WHERE task = ?
	`, task)

	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("lecture de la dernière exécution de %q: %w", task, err)
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("horodatage invalide pour la tâche %q: %w", task, err)
	}

	return at, true, nil
}

// SetLastRun enregistre at comme dernière exécution réussie de la tâche
// (insertion ou remplacement).
func (r *MaintenanceRunRepository) SetLastRun(ctx context.Context, q Querier, task string, at time.Time) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO maintenance_runs (task, last_run_at) VALUES (?, ?)
		ON CONFLICT(task) DO UPDATE SET last_run_at = excluded.last_run_at
	`, task, at.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("enregistrement de la dernière exécution de %q: %w", task, err)
	}

	return nil
}
