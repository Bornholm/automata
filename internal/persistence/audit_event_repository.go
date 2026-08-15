package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AuditEventRepository donne accès à la table audit_events.
type AuditEventRepository struct{}

// NewAuditEventRepository crée un AuditEventRepository.
func NewAuditEventRepository() *AuditEventRepository {
	return &AuditEventRepository{}
}

// Insert insère un événement d'audit.
func (r *AuditEventRepository) Insert(ctx context.Context, q Querier, e AuditEvent) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, org_id, principal_id, trigger, conversation_id, event_type, resource_kind,
			resource_scope, resource_scope_id, outcome, metadata_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.ID, e.OrgID, e.PrincipalID, e.Trigger, e.ConversationID, e.EventType, e.ResourceKind,
		e.ResourceScope, e.ResourceScopeID, e.Outcome, e.MetadataJSON, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("insertion de l'événement d'audit %q: %w", e.ID, err)
	}
	return nil
}

// FindByID retourne l'événement d'audit identifié par id, ou
// (AuditEvent{}, false, nil) s'il n'existe pas.
func (r *AuditEventRepository) FindByID(ctx context.Context, q Querier, id AuditEventID) (AuditEvent, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, principal_id, trigger, conversation_id, event_type, resource_kind,
			resource_scope, resource_scope_id, outcome, metadata_json, created_at
		FROM audit_events
		WHERE id = ?
	`, id)

	var e AuditEvent
	if err := row.Scan(&e.ID, &e.OrgID, &e.PrincipalID, &e.Trigger, &e.ConversationID, &e.EventType, &e.ResourceKind,
		&e.ResourceScope, &e.ResourceScopeID, &e.Outcome, &e.MetadataJSON, &e.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AuditEvent{}, false, nil
		}
		return AuditEvent{}, false, fmt.Errorf("lecture de l'événement d'audit %q: %w", id, err)
	}

	return e, true, nil
}
