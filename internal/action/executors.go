package action

import (
	"context"
	"fmt"

	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// memoryForgetExecutor exécute l'action interne MemoryForgetTool
// ("memory.forget") en appelant memory.Store.Forget, après avoir revérifié
// que la mémoire existe toujours dans la portée du plan (PLAN.md §10.5
// point 6, "résoudre à nouveau les ressources" — pour la mémoire, la
// "ressource" à résoudre à nouveau est son existence même dans la portée
// visée, puisqu'aucun identifiant de ressource externe n'entre en jeu).
type memoryForgetExecutor struct {
	store memory.Store
}

func (e *memoryForgetExecutor) Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error) {
	if act.ToolName != MemoryForgetTool {
		return "", fmt.Errorf("action: outil interne %q non supporté par l'exécuteur mémoire", act.ToolName)
	}

	id, _ := args["id"].(string)
	if id == "" {
		return "", fmt.Errorf("action: argument 'id' manquant pour memory.forget")
	}

	if _, found, err := e.store.GetByID(ctx, plan.OrgID, plan.Scope, plan.ScopeID, id); err != nil {
		return "", fmt.Errorf("vérification de l'existence de la mémoire: %w", err)
	} else if !found {
		return "", fmt.Errorf("mémoire %q introuvable ou déjà supprimée", id)
	}

	if err := e.store.Forget(ctx, id); err != nil {
		return "", fmt.Errorf("suppression de la mémoire: %w", err)
	}

	return "Mémoire supprimée.", nil
}

var _ Executor = &memoryForgetExecutor{}

// mcpExecutor exécute une action en appelant l'outil act.ToolName sur le
// serveur MCP act.MCPServer, via manager. Utilisé par défaut pour toute
// action dont Action.MCPServer n'est pas internalServer et pour laquelle
// aucun exécuteur n'a été explicitement enregistré (WithExecutor).
type mcpExecutor struct {
	manager *mcp.Manager
}

func (e *mcpExecutor) Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error) {
	sessionKey := mcp.SessionKey(plan.ConversationID)

	tools, err := e.manager.GetTools(ctx, sessionKey, act.MCPServer, mcp.Limits{})
	if err != nil {
		return "", fmt.Errorf("récupération des outils du serveur %q: %w", act.MCPServer, err)
	}

	for _, tool := range tools {
		if tool.Name() != act.ToolName {
			continue
		}

		result, err := tool.Execute(ctx, args)
		if err != nil {
			return "", fmt.Errorf("exécution de l'outil %q: %w", act.ToolName, err)
		}

		return result.Text(), nil
	}

	return "", fmt.Errorf("outil %q introuvable sur le serveur %q", act.ToolName, act.MCPServer)
}

var _ Executor = &mcpExecutor{}
