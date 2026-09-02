package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/resource"
)

// applyServerPolicy enveloppe les outils bruts d'un serveur MCP selon la
// politique déclarée pour ce serveur (config.MCPServer.Tools et .Resource).
//
// Toute la spécificité d'un domaine vit dans la configuration, pas ici :
// l'application ne connaît ni agenda, ni liste de tâches, ni aucun autre
// service. Elle sait seulement injecter une ressource, classer un outil en
// lecture ou en écriture, et transformer une écriture en action à confirmer.
//
// Trois comportements se combinent :
//
//   - injection de ressource, si le serveur en déclare une. L'identifiant
//     vient de la portée de la conversation, jamais des arguments du modèle ;
//   - exécution directe des outils de lecture ;
//   - transformation des outils d'écriture en delegation.ProposedAction,
//     lorsque tools.confirm_writes est vrai. Rien n'est exécuté dans le tour.
func applyServerPolicy(
	cfg *config.Config,
	serverName string,
	server config.MCPServer,
	tools []llm.Tool,
	req Request,
	collector *proposalCollector,
	agentName string,
	dedupe *writeDedupeSet,
) ([]llm.Tool, error) {
	// La résolution a lieu même si seuls des outils d'écriture seront
	// utilisés : une portée sans ressource configurée doit être refusée
	// d'emblée, pas au moment de confirmer un plan déjà annoncé.
	if server.Resource != nil {
		if _, err := resource.Resolve(cfg, server.Resource.Key, req.Conversation.Scope, req.Conversation.ScopeID); err != nil {
			return nil, err
		}
	}

	wrapped := make([]llm.Tool, 0, len(tools))

	for _, tool := range tools {
		if server.Tools.ConfirmWrites && isWriteTool(tool, server.Tools) {
			wrapped = append(wrapped, wrapWriteTool(tool, server, serverName, collector, dedupe, agentName, req))
			continue
		}

		wrapped = append(wrapped, wrapDirectTool(tool, cfg, serverName, server, req))
	}

	return wrapped, nil
}

// isWriteTool classe un outil, en croisant l'annotation readOnlyHint du
// serveur et la convention de nommage déclarée.
//
// L'annotation est déclarative et invérifiable : le serveur l'affirme sur
// lui-même. Elle est donc écoutée de façon asymétrique.
//
//   - « cet outil écrit » est toujours cru. Un serveur qui se déclare
//     dangereux ne gagne rien à mentir, et le croire ne coûte qu'une
//     confirmation supplémentaire.
//   - « cet outil ne fait que lire » n'est cru que si la configuration
//     l'autorise explicitement (trust_read_only_hint). Sinon un serveur
//     compromis annonçant une suppression comme lecture contournerait la
//     confirmation.
//
// Sans annotation exploitable, la classification retombe sur les préfixes de
// nom : tout ce qui ne commence pas par un préfixe de lecture est une
// écriture, position prudente plutôt que l'inverse.
func isWriteTool(tool llm.Tool, policy config.MCPTools) bool {
	if annotated, ok := tool.(llm.AnnotatedTool); ok {
		if readOnly, known := annotated.ReadOnly(); known {
			if !readOnly {
				return true
			}

			if policy.TrustReadOnlyHint {
				return false
			}
		}
	}

	for _, prefix := range policy.ReadPrefixes {
		if strings.HasPrefix(tool.Name(), prefix) {
			return false
		}
	}

	return true
}

// wrapDirectTool enveloppe un outil exécuté immédiatement, en y injectant la
// ressource de la portée courante si le serveur en déclare une.
func wrapDirectTool(tool llm.Tool, cfg *config.Config, serverName string, server config.MCPServer, req Request) llm.Tool {
	if server.Resource == nil {
		return tool
	}

	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params)+1)
		maps.Copy(args, params)

		// Écrase systématiquement la valeur fournie par le modèle sous ce
		// nom (plan de conception, §9.2).
		args, err := resource.InjectResolved(cfg, serverName, req.Conversation.Scope, req.Conversation.ScopeID, args)
		if err != nil {
			return llm.NewToolResult(fmt.Sprintf("erreur: %v.", err)), nil
		}

		return tool.Execute(ctx, args)
	}

	return llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)
}

// wrapWriteTool enveloppe un outil d'écriture de sorte qu'il n'exécute JAMAIS
// l'appel réel : il enregistre une action à confirmer, qui remonte à
// l'orchestrateur puis devient un plan persisté (internal/action.Engine).
//
// L'identifiant de ressource est retiré des arguments plutôt qu'y être figé :
// il est résolu à nouveau au moment de la confirmation, depuis la portée du
// plan (plan de conception, §10.5 point 6).
func wrapWriteTool(
	tool llm.Tool,
	server config.MCPServer,
	serverName string,
	collector *proposalCollector,
	dedupe *writeDedupeSet,
	agentName string,
	req Request,
) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params))
		for k, v := range params {
			if server.Resource != nil && k == server.Resource.Parameter {
				continue
			}
			args[k] = v
		}

		if err := validateRFC3339Params(args, server.Tools.RequireRFC3339); err != nil {
			return llm.NewToolResult(fmt.Sprintf(
				"erreur: %v. Fournis une date précise au format ISO8601/RFC3339 avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00) ; aucune action n'a été proposée.",
				err,
			)), nil
		}

		if server.Tools.DedupeWrites {
			if !dedupe.add(tool.Name() + "\x00" + dedupeKey(args)) {
				return llm.NewToolResult(
					"Une action identique a déjà été enregistrée pour ce tour : aucun doublon n'a été ajouté.",
				), nil
			}
		}

		collector.add(delegation.ProposedAction{
			Summary:            summarizeAction(tool.Name(), args),
			AgentID:            agentName,
			MCPServer:          serverName,
			ToolName:           tool.Name(),
			Arguments:          args,
			RequiredPermission: fmt.Sprintf("%s.%s.write", server.PermissionDomain, req.Conversation.Scope),
			Scope:              req.Conversation.Scope,
			ScopeID:            req.Conversation.ScopeID,
		})

		return llm.NewToolResult(
			"Action enregistrée, en attente de la confirmation de l'utilisateur : elle n'a PAS encore été exécutée. " +
				"Tu peux en enregistrer d'autres dans le même tour, elles seront confirmées ensemble. " +
				"N'essaie pas de les confirmer toi-même.",
		), nil
	}

	return llm.NewFuncTool(
		tool.Name(),
		tool.Description()+" (l'action est soumise à la confirmation de l'utilisateur avant toute exécution réelle)",
		tool.Parameters(),
		execute,
	)
}

// validateRFC3339Params refuse args si l'un des paramètres listés n'est pas
// une date RFC3339 avec fuseau explicite.
//
// Aucune tentative n'est faite pour deviner une date ambiguë : une erreur
// claire vaut mieux qu'un rendez-vous enregistré à la mauvaise heure.
func validateRFC3339Params(args map[string]any, required []string) error {
	for _, key := range required {
		raw, present := args[key]
		if !present {
			continue
		}

		value, ok := raw.(string)
		if !ok {
			return fmt.Errorf("le paramètre %q doit être une chaîne de date ISO8601/RFC3339 avec fuseau horaire explicite", key)
		}

		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("le paramètre %q (%q) n'est pas une date ISO8601/RFC3339 valide avec fuseau horaire explicite", key, value)
		}
	}

	return nil
}

// dedupeKey calcule une clé de déduplication à partir des arguments
// sérialisés en JSON (encoding/json trie les clés d'une map), normalisés par
// un TrimSpace et un passage en minuscules.
//
// Comparaison stricte uniquement : deux actions dont le contenu diffère par
// la ponctuation ne sont pas détectées comme doublons. Une similarité floue
// serait spéculative.
func dedupeKey(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}

	return strings.ToLower(strings.TrimSpace(string(raw)))
}

// summarizeAction produit la description humaine affichée dans la liste des
// actions à confirmer. C'est ce texte que l'utilisateur lit avant de
// confirmer : il ne doit jamais se réduire à un identifiant technique.
//
// Les noms de champs essayés sont les plus courants d'un service à l'autre.
// Sans correspondance, le nom de l'outil sert de description.
func summarizeAction(toolName string, args map[string]any) string {
	label := firstStringParam(args, "title", "summary", "name", "task", "label", "subject")
	when := firstStringParam(args, "start", "start_time", "starts_at", "date", "due")

	switch {
	case label != "" && when != "":
		return fmt.Sprintf("%s : %s (%s)", toolName, label, when)
	case label != "":
		return fmt.Sprintf("%s : %s", toolName, label)
	default:
		return toolName
	}
}

// firstStringParam retourne la première valeur textuelle non vide parmi keys.
func firstStringParam(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringParam(args, key); value != "" {
			return value
		}
	}

	return ""
}

// writeDedupeSet mémorise les clés des actions d'écriture déjà proposées
// durant un tour. Protégé par un mutex pour la même raison que
// proposalCollector : ne pas dépendre d'une hypothèse implicite sur l'ordre
// d'exécution des outils.
type writeDedupeSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newWriteDedupeSet() *writeDedupeSet {
	return &writeDedupeSet{seen: make(map[string]bool)}
}

// add enregistre key et retourne false si elle était déjà présente.
func (s *writeDedupeSet) add(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.seen[key] {
		return false
	}
	s.seen[key] = true

	return true
}
