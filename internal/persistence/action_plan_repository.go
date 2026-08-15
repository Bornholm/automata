package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
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

// UpdateStatus met à jour le statut d'un plan d'actions et son
// updated_at (PLAN.md §10.2, cycle de vie). Utilisée à chaque transition
// (confirmed, executing, succeeded, partially_succeeded, failed, expired,
// cancelled) : PLAN.md §10.5 point 10 exige que chaque étape soit persistée
// au fur et à mesure, pas seulement à la fin.
func (r *ActionPlanRepository) UpdateStatus(ctx context.Context, q Querier, id ActionPlanID, status string, updatedAt string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE action_plans SET status = ?, updated_at = ? WHERE id = ?
	`, status, updatedAt, id)
	if err != nil {
		return fmt.Errorf("mise à jour du statut du plan d'actions %q: %w", id, err)
	}
	return nil
}

// ListActiveByConversation retourne les plans d'actions de la conversation
// conversationID dont le statut est "awaiting_confirmation", triés par
// date de création croissante (PLAN.md §10.4 : c'est cet ordre qui numérote
// les plans lors d'une désambiguïsation).
func (r *ActionPlanRepository) ListActiveByConversation(ctx context.Context, q Querier, conversationID model.ConversationID) ([]ActionPlan, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at
		FROM action_plans
		WHERE conversation_id = ? AND status = 'awaiting_confirmation'
		ORDER BY created_at ASC, id ASC
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("liste des plans d'actions actifs de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	var plans []ActionPlan
	for rows.Next() {
		var p ActionPlan
		if err := rows.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.CreatedBy, &p.Scope, &p.ScopeID, &p.Status, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un plan d'actions actif de la conversation %q: %w", conversationID, err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des plans d'actions actifs de la conversation %q: %w", conversationID, err)
	}

	return plans, nil
}
