package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repositories du socle de plugins (migration 0017). Les trois tables
// suivent la même convention que le reste du paquet : repositories sans
// état, Querier explicite, horodatages RFC3339 UTC.

// PluginActivation est une ligne de plugin_activations.
type PluginActivation struct {
	PluginName string
	OrgID      string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PluginActivationRepository gère l'activation des plugins par
// organisation.
type PluginActivationRepository struct{}

// NewPluginActivationRepository crée un PluginActivationRepository.
func NewPluginActivationRepository() *PluginActivationRepository {
	return &PluginActivationRepository{}
}

// Upsert enregistre l'état d'activation d'un plugin pour une organisation.
func (r *PluginActivationRepository) Upsert(ctx context.Context, q Querier, a PluginActivation) error {
	enabled := 0
	if a.Enabled {
		enabled = 1
	}

	_, err := q.ExecContext(ctx, `INSERT INTO plugin_activations
		(plugin_name, org_id, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(plugin_name, org_id) DO UPDATE SET
			enabled = excluded.enabled, updated_at = excluded.updated_at`,
		a.PluginName, a.OrgID, enabled, formatTenantTime(a.CreatedAt), formatTenantTime(a.UpdatedAt))
	if err != nil {
		return fmt.Errorf("activation du plugin %q pour %q: %w", a.PluginName, a.OrgID, err)
	}

	return nil
}

// IsEnabled indique si le plugin est actif pour l'organisation.
func (r *PluginActivationRepository) IsEnabled(ctx context.Context, q Querier, pluginName, orgID string) (bool, error) {
	row := q.QueryRowContext(ctx, `SELECT enabled FROM plugin_activations
		WHERE plugin_name = ? AND org_id = ?`, pluginName, orgID)

	var enabled int
	if err := row.Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lecture de l'activation de %q pour %q: %w", pluginName, orgID, err)
	}

	return enabled != 0, nil
}

// EnabledOrgs retourne les organisations où le plugin est actif.
func (r *PluginActivationRepository) EnabledOrgs(ctx context.Context, q Querier, pluginName string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT org_id FROM plugin_activations
		WHERE plugin_name = ? AND enabled = 1 ORDER BY org_id`, pluginName)
	if err != nil {
		return nil, fmt.Errorf("organisations du plugin %q: %w", pluginName, err)
	}
	defer rows.Close()

	var orgs []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return nil, fmt.Errorf("lecture d'une activation: %w", err)
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des activations: %w", err)
	}

	return orgs, nil
}

// EnabledPlugins retourne les plugins actifs pour une organisation.
func (r *PluginActivationRepository) EnabledPlugins(ctx context.Context, q Querier, orgID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT plugin_name FROM plugin_activations
		WHERE org_id = ? AND enabled = 1 ORDER BY plugin_name`, orgID)
	if err != nil {
		return nil, fmt.Errorf("plugins de l'organisation %q: %w", orgID, err)
	}
	defer rows.Close()

	var plugins []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("lecture d'une activation: %w", err)
		}
		plugins = append(plugins, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des activations: %w", err)
	}

	return plugins, nil
}

// PluginConfig est une ligne de plugin_configs. Config est stockée
// scellée ; le scellement est fait par l'appelant (internal/plugin), les
// repositories ne voient que la valeur opaque.
type PluginConfig struct {
	PluginName string
	OrgID      string
	MemberID   string
	Config     string
	UpdatedAt  time.Time
}

// PluginConfigRepository gère les configurations des plugins.
type PluginConfigRepository struct{}

// NewPluginConfigRepository crée un PluginConfigRepository.
func NewPluginConfigRepository() *PluginConfigRepository {
	return &PluginConfigRepository{}
}

// Upsert enregistre une configuration.
func (r *PluginConfigRepository) Upsert(ctx context.Context, q Querier, c PluginConfig) error {
	_, err := q.ExecContext(ctx, `INSERT INTO plugin_configs
		(plugin_name, org_id, member_id, config, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(plugin_name, org_id, member_id) DO UPDATE SET
			config = excluded.config, updated_at = excluded.updated_at`,
		c.PluginName, c.OrgID, c.MemberID, c.Config, formatTenantTime(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("écriture de la configuration du plugin %q: %w", c.PluginName, err)
	}

	return nil
}

// Get retourne la configuration, ou found=false si elle n'existe pas.
func (r *PluginConfigRepository) Get(ctx context.Context, q Querier, pluginName, orgID, memberID string) (PluginConfig, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT plugin_name, org_id, member_id, config, updated_at
		FROM plugin_configs WHERE plugin_name = ? AND org_id = ? AND member_id = ?`,
		pluginName, orgID, memberID)

	var (
		c         PluginConfig
		updatedAt string
	)
	if err := row.Scan(&c.PluginName, &c.OrgID, &c.MemberID, &c.Config, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PluginConfig{}, false, nil
		}
		return PluginConfig{}, false, fmt.Errorf("lecture de la configuration du plugin %q: %w", pluginName, err)
	}

	var err error
	if c.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return PluginConfig{}, false, err
	}

	return c, true, nil
}

// ListEnabled retourne les configurations du plugin restreintes aux
// organisations où il est actif : un plugin ne voit jamais la
// configuration d'une organisation qui l'a désactivé.
func (r *PluginConfigRepository) ListEnabled(ctx context.Context, q Querier, pluginName string) ([]PluginConfig, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.plugin_name, c.org_id, c.member_id, c.config, c.updated_at
		FROM plugin_configs c
		JOIN plugin_activations a ON a.plugin_name = c.plugin_name AND a.org_id = c.org_id AND a.enabled = 1
		WHERE c.plugin_name = ?
		ORDER BY c.org_id, c.member_id`, pluginName)
	if err != nil {
		return nil, fmt.Errorf("configurations actives du plugin %q: %w", pluginName, err)
	}
	defer rows.Close()

	var configs []PluginConfig
	for rows.Next() {
		var (
			c         PluginConfig
			updatedAt string
		)
		if err := rows.Scan(&c.PluginName, &c.OrgID, &c.MemberID, &c.Config, &updatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'une configuration: %w", err)
		}
		if c.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des configurations: %w", err)
	}

	return configs, nil
}

// PluginSecretRepository gère les secrets des plugins ; les valeurs
// arrivent déjà scellées.
type PluginSecretRepository struct{}

// NewPluginSecretRepository crée un PluginSecretRepository.
func NewPluginSecretRepository() *PluginSecretRepository {
	return &PluginSecretRepository{}
}

// Set enregistre un secret (valeur scellée).
func (r *PluginSecretRepository) Set(ctx context.Context, q Querier, pluginName, orgID, memberID, key, sealed string, at time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO plugin_secrets
		(plugin_name, org_id, member_id, key, value, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(plugin_name, org_id, member_id, key) DO UPDATE SET
			value = excluded.value, updated_at = excluded.updated_at`,
		pluginName, orgID, memberID, key, sealed, formatTenantTime(at))
	if err != nil {
		return fmt.Errorf("écriture d'un secret du plugin %q: %w", pluginName, err)
	}

	return nil
}

// Get retourne la valeur scellée, ou found=false.
func (r *PluginSecretRepository) Get(ctx context.Context, q Querier, pluginName, orgID, memberID, key string) (string, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT value FROM plugin_secrets
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND key = ?`,
		pluginName, orgID, memberID, key)

	var sealed string
	if err := row.Scan(&sealed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("lecture d'un secret du plugin %q: %w", pluginName, err)
	}

	return sealed, true, nil
}

// Delete supprime un secret.
func (r *PluginSecretRepository) Delete(ctx context.Context, q Querier, pluginName, orgID, memberID, key string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM plugin_secrets
		WHERE plugin_name = ? AND org_id = ? AND member_id = ? AND key = ?`,
		pluginName, orgID, memberID, key); err != nil {
		return fmt.Errorf("suppression d'un secret du plugin %q: %w", pluginName, err)
	}

	return nil
}
