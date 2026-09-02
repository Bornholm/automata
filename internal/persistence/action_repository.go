package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ActionRepository donne accès à la table actions.
type ActionRepository struct{}

// NewActionRepository crée un ActionRepository.
func NewActionRepository() *ActionRepository {
	return &ActionRepository{}
}

// Insert insère une action.
func (r *ActionRepository) Insert(ctx context.Context, q Querier, a Action) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO actions (
			id, plan_id, position, agent_id, mcp_server, tool_name, arguments_json, summary,
			required_permission, requires_confirmation, status, error_code, created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.PlanID, a.Position, a.AgentID, a.MCPServer, a.ToolName, a.ArgumentsJSON, a.Summary,
		a.RequiredPermission, a.RequiresConfirmation, a.Status, a.ErrorCode, a.CreatedAt, a.StartedAt, a.CompletedAt)
	if err != nil {
		return fmt.Errorf("insertion de l'action %q: %w", a.ID, err)
	}
	return nil
}

// FindByID retourne l'action identifiée par id, ou (Action{}, false, nil)
// si elle n'existe pas.
func (r *ActionRepository) FindByID(ctx context.Context, q Querier, id ActionID) (Action, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, plan_id, position, agent_id, mcp_server, tool_name, arguments_json, summary,
			required_permission, requires_confirmation, status, error_code, created_at, started_at, completed_at
		FROM actions
		WHERE id = ?
	`, id)

	var a Action
	if err := row.Scan(&a.ID, &a.PlanID, &a.Position, &a.AgentID, &a.MCPServer, &a.ToolName, &a.ArgumentsJSON, &a.Summary,
		&a.RequiredPermission, &a.RequiresConfirmation, &a.Status, &a.ErrorCode, &a.CreatedAt, &a.StartedAt, &a.CompletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Action{}, false, nil
		}
		return Action{}, false, fmt.Errorf("lecture de l'action %q: %w", id, err)
	}

	return a, true, nil
}

// ListByPlanID retourne les actions du plan planID, triées par position
// croissante (plan de conception, §10.5 point 8, "exécuter séquentiellement").
func (r *ActionRepository) ListByPlanID(ctx context.Context, q Querier, planID ActionPlanID) ([]Action, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, plan_id, position, agent_id, mcp_server, tool_name, arguments_json, summary,
			required_permission, requires_confirmation, status, error_code, created_at, started_at, completed_at
		FROM actions
		WHERE plan_id = ?
		ORDER BY position ASC
	`, planID)
	if err != nil {
		return nil, fmt.Errorf("liste des actions du plan %q: %w", planID, err)
	}
	defer rows.Close()

	var actions []Action
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.ID, &a.PlanID, &a.Position, &a.AgentID, &a.MCPServer, &a.ToolName, &a.ArgumentsJSON, &a.Summary,
			&a.RequiredPermission, &a.RequiresConfirmation, &a.Status, &a.ErrorCode, &a.CreatedAt, &a.StartedAt, &a.CompletedAt); err != nil {
			return nil, fmt.Errorf("lecture d'une action du plan %q: %w", planID, err)
		}
		actions = append(actions, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des actions du plan %q: %w", planID, err)
	}

	return actions, nil
}

// UpdateStatus met à jour le statut d'une action et, lorsqu'ils sont non
// nil, ses horodatages de début/fin et son code d'erreur (plan de conception, §10.5
// points 7-9). Un pointeur nil laisse la colonne correspondante inchangée
// (COALESCE), pour permettre des mises à jour partielles successives
// (executing -> succeeded/failed) sans écraser started_at avec NULL lors de
// la seconde mise à jour.
func (r *ActionRepository) UpdateStatus(ctx context.Context, q Querier, id ActionID, status string, startedAt, completedAt, errorCode *string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE actions
		SET status = ?,
			started_at = COALESCE(?, started_at),
			completed_at = COALESCE(?, completed_at),
			error_code = COALESCE(?, error_code)
		WHERE id = ?
	`, status, startedAt, completedAt, errorCode, id)
	if err != nil {
		return fmt.Errorf("mise à jour du statut de l'action %q: %w", id, err)
	}
	return nil
}
