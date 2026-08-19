package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OrgSettings porte la personnalisation d'une organisation (migration
// 0016). Sa valeur zéro décrit le comportement par défaut de l'instance.
type OrgSettings struct {
	OrgID string
	// PromptExtra est ajouté au prompt système de l'agent généraliste.
	PromptExtra string
	// DisabledAgents nomme les spécialistes retirés à cette organisation.
	DisabledAgents []string
	// MaxToolCalls plafonne les appels d'outils par tour ; 0 = défaut.
	MaxToolCalls int
	UpdatedAt    time.Time
}

// AgentDisabled indique si un spécialiste est retiré.
func (s OrgSettings) AgentDisabled(name string) bool {
	for _, disabled := range s.DisabledAgents {
		if disabled == name {
			return true
		}
	}
	return false
}

// OrgSettingsRepository donne accès à la table org_settings.
type OrgSettingsRepository struct{}

// NewOrgSettingsRepository crée un OrgSettingsRepository.
func NewOrgSettingsRepository() *OrgSettingsRepository {
	return &OrgSettingsRepository{}
}

// Get retourne la personnalisation d'une organisation ; found est faux si
// elle n'a jamais été réglée.
func (r *OrgSettingsRepository) Get(ctx context.Context, q Querier, orgID string) (OrgSettings, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT org_id, prompt_extra, disabled_agents, max_tool_calls, updated_at
		FROM org_settings WHERE org_id = ?`, orgID)

	var (
		settings  OrgSettings
		disabled  string
		updatedAt string
	)
	if err := row.Scan(&settings.OrgID, &settings.PromptExtra, &disabled, &settings.MaxToolCalls, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OrgSettings{OrgID: orgID}, false, nil
		}
		return OrgSettings{}, false, fmt.Errorf("lecture de la personnalisation de %q: %w", orgID, err)
	}

	if disabled != "" {
		settings.DisabledAgents = strings.Split(disabled, ",")
	}

	var err error
	if settings.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return OrgSettings{}, false, err
	}

	return settings, true, nil
}

// Upsert enregistre la personnalisation.
func (r *OrgSettingsRepository) Upsert(ctx context.Context, q Querier, settings OrgSettings) error {
	_, err := q.ExecContext(ctx, `INSERT INTO org_settings
		(org_id, prompt_extra, disabled_agents, max_tool_calls, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(org_id) DO UPDATE SET
			prompt_extra = excluded.prompt_extra,
			disabled_agents = excluded.disabled_agents,
			max_tool_calls = excluded.max_tool_calls,
			updated_at = excluded.updated_at`,
		settings.OrgID, settings.PromptExtra, strings.Join(settings.DisabledAgents, ","),
		settings.MaxToolCalls, formatTenantTime(settings.UpdatedAt))
	if err != nil {
		return fmt.Errorf("enregistrement de la personnalisation de %q: %w", settings.OrgID, err)
	}

	return nil
}
