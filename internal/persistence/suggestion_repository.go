package persistence

import (
	"context"
	"fmt"
	"time"
)

// Suggestions d'amélioration produites par l'introspection hebdomadaire
// (migration 0027, internal/introspection).

// Statuts d'une suggestion.
const (
	// SuggestionStatusProposed : émise, visible sur la page de profil.
	SuggestionStatusProposed = "proposed"
	// SuggestionStatusDelivered : poussée dans la conversation privée.
	SuggestionStatusDelivered = "delivered"
	// SuggestionStatusAccepted : la personne l'a marquée comme suivie.
	SuggestionStatusAccepted = "accepted"
	// SuggestionStatusDismissed : écartée — ne jamais la reproposer.
	SuggestionStatusDismissed = "dismissed"
)

// Suggestion est une ligne de la table suggestions.
type Suggestion struct {
	ID       string
	OrgID    string
	MemberID string
	// Kind : automation | activation | fix | habit.
	Kind   string
	Title  string
	Body   string
	Status string
	// CreatedAt est l'instant d'émission.
	CreatedAt time.Time
	// DeliveredAt zéro = jamais poussée en conversation.
	DeliveredAt time.Time
}

// SuggestionRepository gère les suggestions.
type SuggestionRepository struct{}

// NewSuggestionRepository crée un SuggestionRepository.
func NewSuggestionRepository() *SuggestionRepository {
	return &SuggestionRepository{}
}

const suggestionColumns = `id, org_id, member_id, kind, title, body, status, created_at, delivered_at`

// Insert enregistre une suggestion.
func (r *SuggestionRepository) Insert(ctx context.Context, q Querier, s Suggestion) error {
	_, err := q.ExecContext(ctx, `INSERT INTO suggestions (`+suggestionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OrgID, s.MemberID, s.Kind, s.Title, s.Body, s.Status,
		formatTenantTime(s.CreatedAt), formatTenantTime(s.DeliveredAt))
	if err != nil {
		return fmt.Errorf("enregistrement de la suggestion %q: %w", s.ID, err)
	}

	return nil
}

// ListByMember retourne les suggestions d'un membre, de la plus récente à
// la plus ancienne.
func (r *SuggestionRepository) ListByMember(ctx context.Context, q Querier, orgID, memberID string, limit int) ([]Suggestion, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+suggestionColumns+` FROM suggestions
		WHERE org_id = ? AND member_id = ? ORDER BY created_at DESC LIMIT ?`,
		orgID, memberID, limit)
	if err != nil {
		return nil, fmt.Errorf("suggestions du membre %q: %w", memberID, err)
	}
	defer rows.Close()

	var suggestions []Suggestion
	for rows.Next() {
		var (
			s                      Suggestion
			createdAt, deliveredAt string
		)
		if err := rows.Scan(&s.ID, &s.OrgID, &s.MemberID, &s.Kind, &s.Title, &s.Body,
			&s.Status, &createdAt, &deliveredAt); err != nil {
			return nil, fmt.Errorf("lecture d'une suggestion: %w", err)
		}
		if s.CreatedAt, err = parseTenantTime(createdAt); err != nil {
			return nil, err
		}
		if s.DeliveredAt, err = parseTenantTime(deliveredAt); err != nil {
			return nil, err
		}
		suggestions = append(suggestions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des suggestions: %w", err)
	}

	return suggestions, nil
}

// UpdateStatus fait passer une suggestion D'UN MEMBRE PRÉCIS à un nouveau
// statut. Le filtrage par (org, membre) est le cloisonnement : un
// identifiant recopié depuis un autre profil ne modifie rien.
func (r *SuggestionRepository) UpdateStatus(ctx context.Context, q Querier, orgID, memberID, id, status string) (bool, error) {
	result, err := q.ExecContext(ctx, `UPDATE suggestions SET status = ?
		WHERE id = ? AND org_id = ? AND member_id = ?`,
		status, id, orgID, memberID)
	if err != nil {
		return false, fmt.Errorf("statut de la suggestion %q: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("statut de la suggestion %q: %w", id, err)
	}

	return affected > 0, nil
}

// MarkDelivered note qu'une suggestion a été poussée en conversation.
func (r *SuggestionRepository) MarkDelivered(ctx context.Context, q Querier, id string, now time.Time) error {
	if _, err := q.ExecContext(ctx, `UPDATE suggestions
		SET status = ?, delivered_at = ? WHERE id = ?`,
		SuggestionStatusDelivered, formatTenantTime(now), id); err != nil {
		return fmt.Errorf("remise de la suggestion %q: %w", id, err)
	}

	return nil
}

// CountProposedByMember compte les suggestions encore en attente d'un
// membre — le badge de l'onglet du profil.
func (r *SuggestionRepository) CountProposedByMember(ctx context.Context, q Querier, orgID, memberID string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM suggestions
		WHERE org_id = ? AND member_id = ? AND status = ?`,
		orgID, memberID, SuggestionStatusProposed).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("comptage des suggestions du membre %q: %w", memberID, err)
	}

	return count, nil
}

// CountByStatusSince agrège les suggestions par statut depuis cutoff —
// la synthèse mensuelle de l'exploitant, anonyme par construction.
func (r *SuggestionRepository) CountByStatusSince(ctx context.Context, q Querier, cutoff time.Time) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT status, COUNT(*) FROM suggestions
		WHERE created_at >= ? GROUP BY status`, formatTenantTime(cutoff))
	if err != nil {
		return nil, fmt.Errorf("agrégat des suggestions: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var (
			status string
			count  int
		)
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("lecture d'un agrégat: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des agrégats: %w", err)
	}

	return counts, nil
}

// DeleteBefore purge les suggestions antérieures à cutoff — le journal est
// un outil de non-répétition, pas un historique.
func (r *SuggestionRepository) DeleteBefore(ctx context.Context, q Querier, cutoff time.Time) (int64, error) {
	result, err := q.ExecContext(ctx,
		`DELETE FROM suggestions WHERE created_at < ?`, formatTenantTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge des suggestions: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge des suggestions: %w", err)
	}

	return deleted, nil
}
