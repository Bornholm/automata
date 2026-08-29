package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repositories du catalogue de clients de modèles (migration 0022). Même
// convention que le reste du paquet : repositories sans état, Querier
// explicite, horodatages RFC3339 UTC.

// Familles de clients (llm_clients.kind).
const (
	LLMClientKindLLM   = "llm"
	LLMClientKindImage = "image"
)

// LLMClient est une ligne de llm_clients. APIKey est stockée SCELLÉE ; le
// scellement est fait par l'appelant (internal/llmclients), le repository
// ne voit qu'une valeur opaque — même contrat que PluginSecret.
type LLMClient struct {
	Name     string
	Kind     string
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	// ReasoningEffort règle le budget de réflexion des modèles qui en ont
	// un ; vide = valeur par défaut du modèle.
	ReasoningEffort string
	// Vision déclare si le modèle accepte les images en entrée.
	Vision bool
	// ExtraFields est un objet JSON ajouté au corps de chaque requête, ou
	// vide.
	ExtraFields string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LLMClientRepository donne accès au catalogue de clients de modèles.
type LLMClientRepository struct{}

// NewLLMClientRepository crée un LLMClientRepository.
func NewLLMClientRepository() *LLMClientRepository {
	return &LLMClientRepository{}
}

const llmClientColumns = `name, kind, provider, model, base_url, api_key,
	reasoning_effort, vision, extra_fields, created_at, updated_at`

func scanLLMClient(scan func(...any) error) (LLMClient, error) {
	var (
		client               LLMClient
		vision               int
		createdAt, updatedAt string
	)
	if err := scan(&client.Name, &client.Kind, &client.Provider, &client.Model,
		&client.BaseURL, &client.APIKey, &client.ReasoningEffort, &vision,
		&client.ExtraFields, &createdAt, &updatedAt); err != nil {
		return LLMClient{}, err
	}

	client.Vision = vision != 0

	var err error
	if client.CreatedAt, err = parseTenantTime(createdAt); err != nil {
		return LLMClient{}, err
	}
	if client.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
		return LLMClient{}, err
	}

	return client, nil
}

// Upsert enregistre un client. created_at n'est posé qu'à l'insertion : une
// mise à jour ne réécrit pas la date de création.
func (r *LLMClientRepository) Upsert(ctx context.Context, q Querier, client LLMClient) error {
	vision := 0
	if client.Vision {
		vision = 1
	}

	_, err := q.ExecContext(ctx, `INSERT INTO llm_clients
		(`+llmClientColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			kind = excluded.kind, provider = excluded.provider,
			model = excluded.model, base_url = excluded.base_url,
			api_key = excluded.api_key,
			reasoning_effort = excluded.reasoning_effort,
			vision = excluded.vision, extra_fields = excluded.extra_fields,
			updated_at = excluded.updated_at`,
		client.Name, client.Kind, client.Provider, client.Model, client.BaseURL,
		client.APIKey, client.ReasoningEffort, vision, client.ExtraFields,
		formatTenantTime(client.CreatedAt), formatTenantTime(client.UpdatedAt))
	if err != nil {
		return fmt.Errorf("écriture du client de modèle %q: %w", client.Name, err)
	}

	return nil
}

// Get retourne le client nommé, ou (LLMClient{}, false, nil) s'il n'existe
// pas.
func (r *LLMClientRepository) Get(ctx context.Context, q Querier, name string) (LLMClient, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+llmClientColumns+` FROM llm_clients WHERE name = ?`, name)

	client, err := scanLLMClient(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LLMClient{}, false, nil
		}
		return LLMClient{}, false, fmt.Errorf("lecture du client de modèle %q: %w", name, err)
	}

	return client, true, nil
}

// List retourne les clients du catalogue, triés par nom. kind, s'il n'est
// pas vide, restreint à une famille (LLMClientKindLLM ou LLMClientKindImage).
func (r *LLMClientRepository) List(ctx context.Context, q Querier, kind string) ([]LLMClient, error) {
	query := `SELECT ` + llmClientColumns + ` FROM llm_clients`
	args := []any{}
	if kind != "" {
		query += ` WHERE kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY name`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("liste des clients de modèles: %w", err)
	}
	defer rows.Close()

	var clients []LLMClient
	for rows.Next() {
		client, err := scanLLMClient(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("lecture d'un client de modèle: %w", err)
		}
		clients = append(clients, client)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des clients de modèles: %w", err)
	}

	return clients, nil
}

// Delete supprime un client du catalogue. L'appelant vérifie AVANT qu'aucun
// rôle ne le référence : le repository ne connaît pas les rôles de
// l'instance, qui viennent de la configuration.
func (r *LLMClientRepository) Delete(ctx context.Context, q Querier, name string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM llm_clients WHERE name = ?`, name); err != nil {
		return fmt.Errorf("suppression du client de modèle %q: %w", name, err)
	}

	return nil
}

// Count retourne le nombre de clients du catalogue.
func (r *LLMClientRepository) Count(ctx context.Context, q Querier) (int, error) {
	row := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_clients`)

	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("comptage des clients de modèles: %w", err)
	}

	return count, nil
}

// OrgAgentClient est une ligne de org_agent_clients : le client qu'une
// organisation a choisi pour un rôle.
type OrgAgentClient struct {
	OrgID      string
	Role       string
	ClientName string
	UpdatedAt  time.Time
}

// OrgAgentClientRepository donne accès aux choix de modèles par
// organisation.
type OrgAgentClientRepository struct{}

// NewOrgAgentClientRepository crée un OrgAgentClientRepository.
func NewOrgAgentClientRepository() *OrgAgentClientRepository {
	return &OrgAgentClientRepository{}
}

// Set enregistre le client choisi par une organisation pour un rôle.
func (r *OrgAgentClientRepository) Set(ctx context.Context, q Querier, choice OrgAgentClient) error {
	_, err := q.ExecContext(ctx, `INSERT INTO org_agent_clients
		(org_id, role, client_name, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(org_id, role) DO UPDATE SET
			client_name = excluded.client_name, updated_at = excluded.updated_at`,
		choice.OrgID, choice.Role, choice.ClientName, formatTenantTime(choice.UpdatedAt))
	if err != nil {
		return fmt.Errorf("choix du modèle du rôle %q pour %q: %w", choice.Role, choice.OrgID, err)
	}

	return nil
}

// Unset retire la surcharge d'un rôle : l'organisation revient au client
// par défaut de l'instance.
func (r *OrgAgentClientRepository) Unset(ctx context.Context, q Querier, orgID, role string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM org_agent_clients
		WHERE org_id = ? AND role = ?`, orgID, role); err != nil {
		return fmt.Errorf("retrait du modèle du rôle %q pour %q: %w", role, orgID, err)
	}

	return nil
}

// Get retourne le client choisi pour un rôle, ou ("", false, nil) si
// l'organisation s'en remet au défaut de l'instance.
func (r *OrgAgentClientRepository) Get(ctx context.Context, q Querier, orgID, role string) (string, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT client_name FROM org_agent_clients
		WHERE org_id = ? AND role = ?`, orgID, role)

	var name string
	if err := row.Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("lecture du modèle du rôle %q pour %q: %w", role, orgID, err)
	}

	return name, true, nil
}

// ListByOrg retourne les surcharges d'une organisation, indexées par rôle.
func (r *OrgAgentClientRepository) ListByOrg(ctx context.Context, q Querier, orgID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT role, client_name FROM org_agent_clients
		WHERE org_id = ? ORDER BY role`, orgID)
	if err != nil {
		return nil, fmt.Errorf("liste des modèles de l'organisation %q: %w", orgID, err)
	}
	defer rows.Close()

	choices := map[string]string{}
	for rows.Next() {
		var role, name string
		if err := rows.Scan(&role, &name); err != nil {
			return nil, fmt.Errorf("lecture d'un choix de modèle: %w", err)
		}
		choices[role] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des choix de modèles: %w", err)
	}

	return choices, nil
}

// UsedBy retourne les organisations qui ont choisi ce client, avec le rôle
// concerné. Sert à refuser une suppression qui laisserait une organisation
// sans modèle.
func (r *OrgAgentClientRepository) UsedBy(ctx context.Context, q Querier, clientName string) ([]OrgAgentClient, error) {
	rows, err := q.QueryContext(ctx, `SELECT org_id, role, client_name, updated_at
		FROM org_agent_clients WHERE client_name = ? ORDER BY org_id, role`, clientName)
	if err != nil {
		return nil, fmt.Errorf("organisations utilisant le client %q: %w", clientName, err)
	}
	defer rows.Close()

	var uses []OrgAgentClient
	for rows.Next() {
		var (
			use       OrgAgentClient
			updatedAt string
		)
		if err := rows.Scan(&use.OrgID, &use.Role, &use.ClientName, &updatedAt); err != nil {
			return nil, fmt.Errorf("lecture d'un choix de modèle: %w", err)
		}
		if use.UpdatedAt, err = parseTenantTime(updatedAt); err != nil {
			return nil, err
		}
		uses = append(uses, use)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("parcours des choix de modèles: %w", err)
	}

	return uses, nil
}

// DeleteByOrg efface les choix de modèles d'une organisation (purge RGPD).
func (r *OrgAgentClientRepository) DeleteByOrg(ctx context.Context, q Querier, orgID string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM org_agent_clients WHERE org_id = ?`, orgID); err != nil {
		return fmt.Errorf("suppression des modèles de l'organisation %q: %w", orgID, err)
	}

	return nil
}
