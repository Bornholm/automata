package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/bornholm/automata/internal/model"
)

// PrincipalRepository donne accès à la table principals.
type PrincipalRepository struct{}

// NewPrincipalRepository crée un PrincipalRepository.
func NewPrincipalRepository() *PrincipalRepository {
	return &PrincipalRepository{}
}

// Insert insère un principal.
func (r *PrincipalRepository) Insert(ctx context.Context, q Querier, p Principal) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO principals (id, org_id, kind, display_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, p.ID, p.OrgID, p.Kind, p.DisplayName, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insertion du principal %q: %w", p.ID, err)
	}
	return nil
}

// FindByID retourne le principal identifié par id, ou (Principal{}, false, nil)
// s'il n'existe pas.
func (r *PrincipalRepository) FindByID(ctx context.Context, q Querier, id model.PrincipalID) (Principal, bool, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, org_id, kind, display_name, created_at, updated_at
		FROM principals
		WHERE id = ?
	`, id)

	var p Principal
	if err := row.Scan(&p.ID, &p.OrgID, &p.Kind, &p.DisplayName, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, false, nil
		}
		return Principal{}, false, fmt.Errorf("lecture du principal %q: %w", id, err)
	}

	return p, true, nil
}
