package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Platform est le DTO de la table platforms (migration 0012) : un compte
// de messagerie de l'instance. Config est la configuration du provider,
// chiffrée au repos par l'appelant — le repository ne la lit jamais.
type Platform struct {
	ID          string
	Type        string
	DisplayName string
	Config      string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PlatformRepository donne accès à la table platforms.
type PlatformRepository struct{}

// NewPlatformRepository crée un PlatformRepository.
func NewPlatformRepository() *PlatformRepository {
	return &PlatformRepository{}
}

const platformColumns = `id, type, display_name, config, enabled, created_at, updated_at`

func scanPlatform(scan func(...any) error) (Platform, error) {
	var (
		p                    Platform
		enabled              int
		createdAt, updatedAt string
	)
	if err := scan(&p.ID, &p.Type, &p.DisplayName, &p.Config, &enabled, &createdAt, &updatedAt); err != nil {
		return Platform{}, err
	}

	p.Enabled = enabled != 0

	var err error
	if p.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return Platform{}, err
	}
	if p.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return Platform{}, err
	}

	return p, nil
}

// Insert enregistre p. ignoreExisting (migration depuis la configuration)
// transforme un conflit d'identifiant en non-opération : un compte déjà
// migré n'est jamais écrasé, sa session reste intacte.
func (r *PlatformRepository) Insert(ctx context.Context, q Querier, p Platform, ignoreExisting bool) error {
	verb := "INSERT"
	if ignoreExisting {
		verb = "INSERT OR IGNORE"
	}

	enabled := 0
	if p.Enabled {
		enabled = 1
	}

	_, err := q.ExecContext(ctx, verb+` INTO platforms (`+platformColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Type, p.DisplayName, p.Config, enabled,
		formatTenantTime(p.CreatedAt), formatTenantTime(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insertion de la plateforme %q: %w", p.ID, err)
	}

	return nil
}

// Update remplace le nom affiché, la configuration et l'activation.
func (r *PlatformRepository) Update(ctx context.Context, q Querier, p Platform) error {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}

	res, err := q.ExecContext(ctx, `UPDATE platforms
		SET display_name = ?, config = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		p.DisplayName, p.Config, enabled, formatTenantTime(p.UpdatedAt), p.ID)
	if err != nil {
		return fmt.Errorf("mise à jour de la plateforme %q: %w", p.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("plateforme %q introuvable", p.ID)
	}

	return nil
}

// FindByID retourne la plateforme, ou (Platform{}, false, nil).
func (r *PlatformRepository) FindByID(ctx context.Context, q Querier, id string) (Platform, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+platformColumns+` FROM platforms WHERE id = ?`, id)

	p, err := scanPlatform(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Platform{}, false, nil
		}
		return Platform{}, false, fmt.Errorf("lecture de la plateforme %q: %w", id, err)
	}

	return p, true, nil
}

// List retourne toutes les plateformes, triées par identifiant.
func (r *PlatformRepository) List(ctx context.Context, q Querier) ([]Platform, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+platformColumns+` FROM platforms ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("liste des plateformes: %w", err)
	}
	defer rows.Close()

	var platforms []Platform
	for rows.Next() {
		p, err := scanPlatform(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("lecture d'une plateforme: %w", err)
		}
		platforms = append(platforms, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des plateformes: %w", err)
	}

	return platforms, nil
}

// Delete supprime une plateforme. La session éventuellement stockée sur
// disque n'est pas touchée : la supprimer relève d'une décision explicite
// de l'exploitant, pas d'un clic dans une interface.
func (r *PlatformRepository) Delete(ctx context.Context, q Querier, id string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM platforms WHERE id = ?`, id); err != nil {
		return fmt.Errorf("suppression de la plateforme %q: %w", id, err)
	}

	return nil
}
