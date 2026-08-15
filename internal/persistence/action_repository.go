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
