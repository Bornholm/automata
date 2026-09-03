package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// ActionExecutor rejoue une action de plugin confirmée par l'utilisateur.
// Enregistré auprès d'internal/action.Engine sous la clé
// "plugin:<nom>" : la persistance du plan, le TTL, la commande littérale
// « confirmer », la re-vérification de permission et l'audit viennent du
// moteur existant — rien n'est réimplémenté ici.
type ActionExecutor struct {
	manager *Manager
	db      dbTx
	plugin  string
}

// NewActionExecutor crée l'exécuteur du plugin nommé.
func NewActionExecutor(manager *Manager, db dbTx, pluginName string) *ActionExecutor {
	return &ActionExecutor{manager: manager, db: db, plugin: pluginName}
}

// subAgentFromAgentID extrait l'entrée de catalogue d'un identifiant
// d'agent "plugin:<plugin>:<sous-agent>". Vide pour "plugin:<plugin>",
// forme des plugins à sous-agent unique et de toutes les actions
// proposées avant l'arrivée des catalogues : une confirmation en attente
// au moment de la mise à jour reste exécutable.
func subAgentFromAgentID(agentID string) string {
	parts := strings.SplitN(agentID, ":", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// Execute implémente action.Executor.
func (e *ActionExecutor) Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error) {
	// L'activation est re-vérifiée au moment de la confirmation : un
	// plugin désactivé entre la proposition et le « confirmer » n'exécute
	// rien — même fenêtre de re-vérification que la permission.
	var enabled bool
	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		enabled, err = e.manager.hostService.activations.IsEnabled(ctx, tx, e.plugin, string(plan.OrgID))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("plugin: vérification de l'activation: %w", err)
	}
	if !enabled {
		return "", fmt.Errorf("le plugin %q n'est plus actif pour cette organisation", e.plugin)
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("plugin: arguments non sérialisables: %w", err)
	}

	// L'identité est celle du CONFIRMATEUR (motif mcpExecutor), et la clé
	// d'idempotence est l'identifiant de l'action : une confirmation
	// rejouée ne répète pas l'effet de bord.
	result, isError, err := e.manager.CallTool(ctx, e.plugin, act.ToolName, CallContext{
		OrgID:          string(plan.OrgID),
		MemberID:       string(identity.PrincipalID),
		Scope:          string(plan.Scope),
		ScopeID:        string(plan.ScopeID),
		IdempotencyKey: string(act.ID),
		// Le sous-agent qui a proposé l'action, relu de l'identifiant
		// d'agent : un plugin à catalogue peut exposer le même nom d'outil
		// dans deux entrées, et une confirmation doit revenir à la bonne.
		SubAgent: subAgentFromAgentID(act.AgentID),
		// Timeout par défaut de l'hôte : l'exécution d'une action confirmée
		// se fait hors du tour de conversation, elle n'a pas la même
		// contrainte de latence qu'un outil appelé pendant la réponse.
	}, string(argsJSON), 0)
	if err != nil {
		return "", err
	}
	if isError {
		return "", fmt.Errorf("%s", result)
	}

	return result, nil
}
