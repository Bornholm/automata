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
// updated_at (plan de conception, §10.2, cycle de vie). Utilisée à chaque transition
// (confirmed, executing, succeeded, partially_succeeded, failed, expired,
// cancelled) : plan de conception, §10.5 point 10 exige que chaque étape soit persistée
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

// CompareAndSwapStatus fait passer un plan d'actions de fromStatus à
// toStatus et retourne false si le plan n'était pas (ou plus) dans
// fromStatus. C'est la transition à utiliser pour toute étape qui ne doit se
// produire qu'une fois — au premier chef le passage d'un plan à l'exécution
// (plan de conception, §10.5 point 2, "empêcher les doubles exécutions").
//
// Contrairement à UpdateStatus, la garde est portée par la base et non par
// une lecture préalable : un "lire le statut puis écrire" laisse une fenêtre
// entre les deux pendant laquelle un second confirmateur peut passer la même
// garde, et déclencher une seconde fois des écritures externes réelles. Cette
// fenêtre est aujourd'hui inatteignable (l'ingress traite les messages d'une
// conversation séquentiellement), mais l'invariant ne doit pas dépendre de
// cette seule propriété d'ordonnancement.
func (r *ActionPlanRepository) CompareAndSwapStatus(ctx context.Context, q Querier, id ActionPlanID, fromStatus, toStatus string, updatedAt string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE action_plans SET status = ?, updated_at = ? WHERE id = ? AND status = ?
	`, toStatus, updatedAt, id, fromStatus)
	if err != nil {
		return false, fmt.Errorf("transition du plan d'actions %q de %q vers %q: %w", id, fromStatus, toStatus, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("transition du plan d'actions %q: lecture du nombre de lignes affectées: %w", id, err)
	}

	return affected > 0, nil
}

// ListActiveByConversation retourne les plans d'actions de la conversation
// conversationID dont le statut est "awaiting_confirmation", triés par
// date de création croissante (plan de conception, §10.4 : c'est cet ordre qui numérote
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

// ListByStatus retourne tous les plans d'actions dans le statut status,
// triés par date de création croissante. Utilisée par
// action.Engine.RecoverInterrupted (plan de conception, Phase 18) pour retrouver, au
// redémarrage, les plans restés bloqués en "executing" par un crash du
// processus.
func (r *ActionPlanRepository) ListByStatus(ctx context.Context, q Querier, status string) ([]ActionPlan, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at
		FROM action_plans
		WHERE status = ?
		ORDER BY created_at ASC, id ASC
	`, status)
	if err != nil {
		return nil, fmt.Errorf("liste des plans d'actions de statut %q: %w", status, err)
	}
	defer rows.Close()

	var plans []ActionPlan
	for rows.Next() {
		var p ActionPlan
		if err := rows.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.CreatedBy, &p.Scope, &p.ScopeID, &p.Status, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un plan d'actions de statut %q: %w", status, err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des plans d'actions de statut %q: %w", status, err)
	}

	return plans, nil
}

// ListRecent retourne au plus limit plans d'actions, triés par date de
// création décroissante (les plus récents d'abord). Utilisée par la
// commande d'administration "automata admin inspect" (plan de conception, Phase 18),
// lecture seule.
func (r *ActionPlanRepository) ListRecent(ctx context.Context, q Querier, limit int) ([]ActionPlan, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at
		FROM action_plans
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("liste des plans d'actions récents: %w", err)
	}
	defer rows.Close()

	var plans []ActionPlan
	for rows.Next() {
		var p ActionPlan
		if err := rows.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.CreatedBy, &p.Scope, &p.ScopeID, &p.Status, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un plan d'actions récent: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des plans d'actions récents: %w", err)
	}

	return plans, nil
}

// ListRecentByConversation retourne les plans d'actions de la
// conversation créés depuis since, quel que soit leur statut, du plus
// récent au plus ancien. Sert au journal d'introspection de l'assistant
// (voir internal/agent, list_recent_activity) : un plan confirmé ou
// abandonné raconte ce qui s'est réellement passé.
func (r *ActionPlanRepository) ListRecentByConversation(ctx context.Context, q Querier, conversationID model.ConversationID, since string, limit int) ([]ActionPlan, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := q.QueryContext(ctx, `
		SELECT id, org_id, conversation_id, created_by, scope, scope_id, status, expires_at, created_at, updated_at
		FROM action_plans
		WHERE conversation_id = ? AND created_at >= ?
		ORDER BY created_at DESC
		LIMIT ?
	`, conversationID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("historique des plans de la conversation %q: %w", conversationID, err)
	}
	defer rows.Close()

	var plans []ActionPlan
	for rows.Next() {
		var p ActionPlan
		if err := rows.Scan(&p.ID, &p.OrgID, &p.ConversationID, &p.CreatedBy, &p.Scope, &p.ScopeID, &p.Status, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un plan de l'historique: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours de l'historique des plans: %w", err)
	}

	return plans, nil
}
