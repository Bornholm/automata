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
// que la mémoire existe toujours dans la portée du plan (plan de conception, §10.5
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

	// Le principal retenu est celui qui CONFIRME, pas l'auteur de la
	// proposition : c'est lui qui engage l'écriture, et c'est donc sa
	// connexion (son jeton) qui doit être utilisée.
	tools, err := e.manager.GetToolsFor(ctx, sessionKey, identity.PrincipalID, act.MCPServer, mcp.Limits{})
	if err != nil {
		return "", fmt.Errorf("récupération des outils du serveur %q: %w", act.MCPServer, err)
	}

	for _, tool := range tools {
		if tool.Name() != act.ToolName {
			continue
		}

		args = withIdempotencyKey(args, tool.Parameters(), string(act.ID))

		result, err := tool.Execute(ctx, args)
		if err != nil {
			return "", fmt.Errorf("exécution de l'outil %q: %w", act.ToolName, err)
		}

		return result.Text(), nil
	}

	return "", fmt.Errorf("outil %q introuvable sur le serveur %q", act.ToolName, act.MCPServer)
}

var _ Executor = &mcpExecutor{}

// idempotencyKeyPropertyNames énumère, par ordre de préférence, les noms de
// propriété conventionnels sous lesquels un serveur MCP peut déclarer
// accepter une clé d'idempotence (plan de conception, §18, "ajouter des clés
// d'idempotence aux actions MCP lorsque possible"). Le premier nom présent
// dans le schéma du outil l'emporte.
var idempotencyKeyPropertyNames = []string{"idempotency_key", "client_request_id", "request_id"}

// idempotencyKeyProperty inspecte schema (le JSON Schema retourné par
// llm.Tool.Parameters(), typiquement {"type":"object","properties":{...}})
// et retourne le premier nom de propriété conventionnel qui y est déclaré,
// s'il en existe un. Purement best-effort : de nombreux serveurs MCP réels
// ne déclarent aucune de ces propriétés, auquel cas ok vaut false et aucune
// injection n'a lieu — ce n'est en rien garanti par le protocole MCP.
func idempotencyKeyProperty(schema map[string]any) (string, bool) {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return "", false
	}

	for _, name := range idempotencyKeyPropertyNames {
		if _, ok := properties[name]; ok {
			return name, true
		}
	}

	return "", false
}

// withIdempotencyKey retourne args augmenté de la clé d'idempotence
// actionID sous la propriété conventionnelle déclarée par schema, si une
// telle propriété existe (voir idempotencyKeyProperty). N'alloue et ne
// modifie rien si schema n'en déclare aucune : args est alors retourné tel
// quel (plan de conception, §18, injection "lorsque possible" seulement).
func withIdempotencyKey(args map[string]any, schema map[string]any, actionID string) map[string]any {
	name, ok := idempotencyKeyProperty(schema)
	if !ok {
		return args
	}

	if args == nil {
		args = make(map[string]any, 1)
	}
	args[name] = actionID

	return args
}
