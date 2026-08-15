package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ActionPlanRepository donne accès à la table action_plans.
type ActionPlanRepository struct{}

// NewActionPlanRepository crée un ActionPlanRepository.
func NewActionPlanRepository() *ActionPlanRepository {
	return &ActionPlanRepository{}
}

// Insert insère un plan d'actions.
func (r *ActionPlanRepository) Insert(ctx context.Context, q Querier, p ActionPlan) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO action_plans (
			id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, p.OrgID, p.ConversationID, p.CreatedBy, p.Scope, p.ScopeID, p.Status, p.ExpiresAt, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insertion du plan d'actions %q: %w", p.ID, err)
	}
	return nil
}

// FindByID retourne le plan d'actions identifié par id, ou
// (ActionPlan{}, false, nil) s'il n'existe pas.
func (r *ActionPlanRepository) FindByID(ctx context.Context, q Querier, id ActionPlanID) (ActionPlan, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at
		FROM action_plans
		WHERE id = ?
	`, id)

	var p ActionPlan
	if err := row.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.CreatedBy, &p.Scope, &p.ScopeID, &p.Status, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ActionPlan{}, false, nil
		}
		return ActionPlan{}, false, fmt.Errorf("lecture du plan d'actions %q: %w", id, err)
	}

	return p, true, nil
}
