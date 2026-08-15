package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
)

// MemoryTools regroupe les dépendances nécessaires pour exposer les outils
// de mémoire (search_memory, remember, forget_memory) à un
// OrchestratorAgent (PLAN.md §6.1, §8, Phase 10). Une valeur zéro (Store
// nil) n'expose aucun outil mémoire, quels que soient Search/Remember/Forget
// : voir WithMemoryTools.
type MemoryTools struct {
	Store      memory.Store
	Authorizer *authorization.Authorizer
	Search     bool
	Remember   bool
	Forget     bool
	// MaxResults borne le nombre de mémoires retournées par une recherche
	// (search_memory ou la liste de candidats de forget_memory). Une valeur
	// <= 0 retombe sur 5.
	MaxResults int
	// Metrics observe le nombre de recherches mémoire (search_memory,
	// PLAN.md §14.3, Phase 20). nil désactive l'observation.
	Metrics *observability.Metrics
}

func (t MemoryTools) maxResults() int {
	if t.MaxResults <= 0 {
		return 5
	}
	return t.MaxResults
}

// enabled indique si t décrit des outils mémoire exploitables : un Store et
// un Authorizer sont requis quel que soit l'état des booléens Search/
// Remember/Forget.
func (t MemoryTools) enabled() bool {
	return t.Store != nil && t.Authorizer != nil
}

// buildMemoryTools construit les outils LLM de mémoire autorisés pour
// identity, selon les capacités déclarées par la configuration de l'agent
// (t.Search/t.Remember/t.Forget). Reconstruit à chaque exécution, comme
// buildDelegationTools : l'identité n'est jamais décidée par le modèle.
// collector accumule les delegation.ProposedAction produites par
// forget_memory (PLAN.md §10, Phase 15) : voir proposalCollector
// (toolloop.go).
func (t MemoryTools) buildMemoryTools(identity model.ExecutionIdentity, collector *proposalCollector) []llm.Tool {
	if !t.enabled() {
		return nil
	}

	var tools []llm.Tool

	if t.Search {
		tools = append(tools, t.newSearchMemoryTool(identity))
	}
	if t.Remember {
		tools = append(tools, t.newRememberTool(identity))
	}
	if t.Forget {
		tools = append(tools, t.newForgetMemoryTool(identity, collector))
	}

	return tools
}

// readScopes énumère les portées qu'une identité d'exécution peut, sous
// réserve d'autorisation effective (vérifiée séparément via Authorizer),
// consulter en lecture (PLAN.md §8.3) :
//
//   - conversation privée : portée personnelle du principal + portée org ;
//   - conversation de groupe : portée du groupe courant + portée org ;
//   - déclenchement planifié (cron) : portée du groupe configuré (s'il y en
//     a un) + portée org ; jamais la portée personnelle (interdite par
//     défaut en V1 par les règles invariantes, voir
//     internal/authorization.checkCronRules).
//
// Cette liste est délibérément large : c'est Authorizer.Authorize, appelé
// pour chaque portée avant toute opération, qui décide en dernier ressort ce
// qui est effectivement accessible. Une portée non autorisée est simplement
// ignorée par l'appelant (voir searchAuthorizedScopes), jamais une erreur.
func readScopes(identity model.ExecutionIdentity) []model.Scope {
	if identity.Trigger == model.TriggerCron {
		if identity.ScopeID != "" {
			return []model.Scope{model.ScopeGroup, model.ScopeOrg}
		}
		return []model.Scope{model.ScopeOrg}
	}

	switch identity.ChannelKind {
	case model.ChannelPrivate:
		return []model.Scope{model.ScopePersonal, model.ScopeOrg}
	case model.ChannelGroup:
		return []model.Scope{model.ScopeGroup, model.ScopeOrg}
	}

	return nil
}

// writeScope détermine la portée d'écriture de la conversation courante
// pour remember (PLAN.md, Phase 10, "la portée est TOUJOURS celle de la
// conversation courante ... JAMAIS org"). ok vaut false si aucune écriture
// n'est possible dans ce contexte d'exécution (ex : déclenchement planifié).
func writeScope(identity model.ExecutionIdentity) (model.Scope, bool) {
	switch identity.ChannelKind {
	case model.ChannelPrivate:
		return model.ScopePersonal, true
	case model.ChannelGroup:
		return model.ScopeGroup, true
	}
	return "", false
}

// scopeID résout l'identifiant de portée applicable pour scope, à partir de
// identity. Pour org, c'est l'identifiant d'organisation courant (V1 :
// convention déterministe, une seule organisation par instance, voir
// internal/identity.EffectivePermissions) ; pour personal/group, c'est
// directement identity.ScopeID, déjà résolu par internal/identity.Resolver
// selon la même convention que celle vérifiée par
// internal/authorization.checkPrivateRules/checkGroupRules.
func scopeID(scope model.Scope, identity model.ExecutionIdentity) model.ScopeID {
	if scope == model.ScopeOrg {
		return model.ScopeID(identity.OrgID)
	}
	return identity.ScopeID
}

// searchAuthorizedScopes recherche text dans chaque portée de scopes pour
// laquelle identity est autorisée (permission "memory.<scope>.<action>"),
// en ignorant silencieusement les portées non autorisées (PLAN.md, Phase
// 10 : "ignore silencieusement les portées non autorisées plutôt que
// d'échouer tout l'appel").
func (t MemoryTools) searchAuthorizedScopes(ctx context.Context, identity model.ExecutionIdentity, scopes []model.Scope, action, text string) ([]memory.Memory, error) {
	var results []memory.Memory

	for _, scope := range scopes {
		sID := scopeID(scope, identity)

		if err := t.Authorizer.Authorize(ctx, authorization.AuthorizationRequest{
			Identity:      identity,
			Permission:    fmt.Sprintf("memory.%s.%s", scope, action),
			TargetOrgID:   identity.OrgID,
			TargetScope:   scope,
			TargetScopeID: sID,
		}); err != nil {
			continue
		}

		found, err := t.Store.Search(ctx, memory.Query{
			Text:       text,
			OrgID:      identity.OrgID,
			Scope:      scope,
			ScopeID:    sID,
			MaxResults: t.maxResults(),
		})
		if err != nil {
			return nil, err
		}

		results = append(results, found...)
	}

	return results, nil
}

// findByIDForDelete recherche la mémoire id dans chaque portée lisible par
// identity pour laquelle la permission de suppression est accordée. La
// première portée autorisée où id est trouvé l'emporte. Retourne
// (nil, "", nil) si aucune portée autorisée ne contient id (identifiant
// inconnu, ou connu seulement dans une portée où la suppression n'est pas
// permise à ce principal).
func (t MemoryTools) findByIDForDelete(ctx context.Context, identity model.ExecutionIdentity, id string) (*memory.Memory, model.Scope, error) {
	for _, scope := range readScopes(identity) {
		sID := scopeID(scope, identity)

		if err := t.Authorizer.Authorize(ctx, authorization.AuthorizationRequest{
			Identity:      identity,
			Permission:    fmt.Sprintf("memory.%s.delete", scope),
			TargetOrgID:   identity.OrgID,
			TargetScope:   scope,
			TargetScopeID: sID,
		}); err != nil {
			continue
		}

		mem, found, err := t.Store.GetByID(ctx, identity.OrgID, scope, sID, id)
		if err != nil {
			return nil, "", err
		}
		if found {
			return &mem, scope, nil
		}
	}

	return nil, "", nil
}

// formatMemoryList formate memories en liste numérotée (PLAN.md §8.5,
// "afficher une liste numérotée") : c'est cette liste qui sert de base à
// une suppression ultérieure par identifiant précis.
func formatMemoryList(memories []memory.Memory) string {
	if len(memories) == 0 {
		return "Aucune mémoire trouvée."
	}

	const excerptLimit = 200

	var b strings.Builder
	for i, m := range memories {
		excerpt := m.Content
		if len(excerpt) > excerptLimit {
			excerpt = excerpt[:excerptLimit] + "…"
		}
		fmt.Fprintf(&b, "%d. [id: %s] %s\n", i+1, m.ID, excerpt)
	}

	return b.String()
}

// newSearchMemoryTool construit l'outil "search_memory". La portée de
// recherche n'est jamais un paramètre du modèle : elle est entièrement
// déterminée par identity (PLAN.md, Phase 10).
func (t MemoryTools) newSearchMemoryTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("query", "Texte de recherche dans la mémoire.", "string")

	return llm.NewFuncTool(
		"search_memory",
		"Recherche dans la mémoire persistante de l'assistant, cloisonnée automatiquement selon la conversation courante.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			query, _ := params["query"].(string)
			if strings.TrimSpace(query) == "" {
				return llm.NewToolResult("erreur: le paramètre 'query' est requis et ne peut pas être vide."), nil
			}

			t.Metrics.IncMemorySearch()

			results, err := t.searchAuthorizedScopes(ctx, identity, readScopes(identity), "read", query)
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("erreur lors de la recherche mémoire: %v", err)), nil
			}

			return llm.NewToolResult(formatMemoryList(results)), nil
		},
	)
}

// newRememberTool construit l'outil "remember". La portée d'écriture n'est
// jamais un paramètre du modèle : elle est toujours celle de la conversation
// courante (jamais org, voir writeScope).
func (t MemoryTools) newRememberTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("content", "Contenu textuel à mémoriser durablement.", "string")

	return llm.NewFuncTool(
		"remember",
		"Enregistre une information dans la mémoire persistante de l'assistant, dans la portée de la conversation courante.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			content, _ := params["content"].(string)
			if strings.TrimSpace(content) == "" {
				return llm.NewToolResult("erreur: le paramètre 'content' est requis et ne peut pas être vide."), nil
			}

			scope, ok := writeScope(identity)
			if !ok {
				return llm.NewToolResult("erreur: la mémorisation n'est pas disponible dans ce contexte d'exécution."), nil
			}

			sID := scopeID(scope, identity)

			if err := t.Authorizer.Authorize(ctx, authorization.AuthorizationRequest{
				Identity:      identity,
				Permission:    fmt.Sprintf("memory.%s.write", scope),
				TargetOrgID:   identity.OrgID,
				TargetScope:   scope,
				TargetScopeID: sID,
			}); err != nil {
				return llm.NewToolResult(fmt.Sprintf("mémorisation refusée: %v", err)), nil
			}

			mem, err := t.Store.Remember(ctx, memory.NewMemory{
				Content:              content,
				Scope:                scope,
				ScopeID:              sID,
				OrgID:                identity.OrgID,
				OwnerPrincipalID:     identity.PrincipalID,
				CreatedBy:            identity.PrincipalID,
				SourceConversationID: identity.ConversationID,
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("échec de la mémorisation: %v", err)), nil
			}

			return llm.NewToolResult(fmt.Sprintf("Mémorisé avec succès (id: %s).", mem.ID)), nil
		},
	)
}

// newForgetMemoryTool construit l'outil "forget_memory" (PLAN.md §8.5,
// §10). Depuis la Phase 15, cet outil n'exécute plus jamais de suppression
// lui-même : une requête textuelle seule liste des candidats (inchangé), et
// un 'id' résolu sans ambiguïté produit une delegation.ProposedAction,
// accumulée dans collector, plutôt qu'un paramètre 'confirm' ad-hoc. C'est
// internal/action.Engine qui persiste cette proposition en
// persistence.ActionPlan (via internal/conversation.Handler, qui lit
// Result.ProposedActions après l'exécution de l'agent) et qui l'exécute
// réellement, seulement après confirmation explicite ("confirmer" dans la
// conversation, PLAN.md §10.4) et revérification complète (§10.5) — jamais
// au sein de ce tour d'outil.
func (t MemoryTools) newForgetMemoryTool(identity model.ExecutionIdentity, collector *proposalCollector) llm.Tool {
	schema := llm.NewJSONSchema().
		Property("query", "Texte décrivant la ou les mémoires à supprimer. À utiliser seul, sans 'id', pour obtenir une liste numérotée de candidats.", "string").
		Property("id", "Identifiant précis d'une mémoire à supprimer, obtenu via une recherche préalable (search_memory ou un appel précédent de forget_memory avec 'query').", "string")

	return llm.NewFuncTool(
		"forget_memory",
		"Propose la suppression d'une mémoire de la mémoire persistante. Une requête textuelle seule ne supprime jamais rien : elle ne fait que lister des candidats. Un 'id' précis propose une suppression, qui ne sera exécutée qu'après confirmation explicite de l'utilisateur dans la conversation (répondre \"confirmer\").",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			query := strings.TrimSpace(stringParam(params, "query"))
			id := strings.TrimSpace(stringParam(params, "id"))

			if id == "" {
				if query == "" {
					return llm.NewToolResult("erreur: fournir soit 'query' (pour lister des candidats), soit 'id' (pour proposer la suppression d'une mémoire précise)."), nil
				}

				// PLAN.md §8.5 : "une suppression par requête textuelle non
				// résolue est interdite". Cette branche ne propose jamais rien,
				// elle se contente de lister des candidats.
				results, err := t.searchAuthorizedScopes(ctx, identity, readScopes(identity), "read", query)
				if err != nil {
					return llm.NewToolResult(fmt.Sprintf("erreur lors de la recherche mémoire: %v", err)), nil
				}
				if len(results) == 0 {
					return llm.NewToolResult("Aucune mémoire correspondante."), nil
				}

				return llm.NewToolResult("Mémoires candidates (rappeler forget_memory avec le paramètre 'id' exact d'une de ces entrées) :\n" + formatMemoryList(results)), nil
			}

			mem, scope, err := t.findByIDForDelete(ctx, identity, id)
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("erreur lors de la vérification de la mémoire: %v", err)), nil
			}
			if mem == nil {
				return llm.NewToolResult(fmt.Sprintf("aucune mémoire accessible avec l'identifiant %q dans ce contexte.", id)), nil
			}

			collector.add(delegation.ProposedAction{
				Summary:            fmt.Sprintf("Supprimer la mémoire (portée %s) : %s", scope, mem.Content),
				MCPServer:          action.InternalServer,
				ToolName:           action.MemoryForgetTool,
				Arguments:          map[string]any{"id": mem.ID},
				RequiredPermission: fmt.Sprintf("memory.%s.delete", scope),
				Scope:              scope,
				ScopeID:            scopeID(scope, identity),
			})

			return llm.NewToolResult(fmt.Sprintf(
				"Suppression proposée (portée %s) : %s\nEn attente de confirmation : répondez \"confirmer\" dans la conversation pour l'exécuter, ou \"annuler\" pour abandonner.",
				scope, mem.Content,
			)), nil
		},
	)
}

// stringParam extrait un paramètre optionnel de type string, sans échouer
// s'il est absent ou d'un autre type.
func stringParam(params map[string]any, name string) string {
	s, _ := params[name].(string)
	return s
}
