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
// OrchestratorAgent (plan de conception, §6.1, §8, Phase 10). Une valeur zéro (Store
// nil) n'expose aucun outil mémoire, quels que soient Search/Remember/Forget
// : voir WithMemoryTools.
type MemoryTools struct {
	Store      memory.Store
	Authorizer *authorization.Authorizer
	Search     bool
	Remember   bool
	Forget     bool
	// Episodes est le store de la mémoire épisodique (fragments verbatim de
	// conversations passées, conservés à la compaction). nil désactive
	// l'outil search_conversation_history, quel que soit History.
	Episodes memory.EpisodeStore
	// History expose l'outil search_conversation_history.
	History bool
	// Recall active le rappel automatique : à chaque tour, une recherche
	// mémoire sur le message entrant injecte les souvenirs les plus
	// pertinents dans le message système, sans attendre que le modèle pense
	// à appeler search_memory. L'outil reste disponible pour une recherche
	// dirigée.
	Recall bool
	// RecallMax borne le nombre de souvenirs injectés par tour. Une valeur
	// <= 0 retombe sur 3.
	RecallMax int
	// MaxResults borne le nombre de mémoires retournées par une recherche
	// (search_memory ou la liste de candidats de forget_memory). Une valeur
	// <= 0 retombe sur 5.
	MaxResults int
	// Metrics observe le nombre de recherches mémoire (search_memory,
	// plan de conception, §14.3, Phase 20). nil désactive l'observation.
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
// forget_memory (plan de conception, §10, Phase 15) : voir proposalCollector
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
	if t.History && t.Episodes != nil {
		tools = append(tools, t.newSearchHistoryTool(identity))
	}

	return tools
}

// readScopes énumère les portées qu'une identité d'exécution peut, sous
// réserve d'autorisation effective (vérifiée séparément via Authorizer),
// consulter en lecture (plan de conception, §8.3) :
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
// pour remember (plan de conception, Phase 10, "la portée est TOUJOURS celle de la
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
// en ignorant silencieusement les portées non autorisées (le plan de conception, Phase
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

// formatMemoryList formate memories en liste numérotée (plan de conception, §8.5,
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
		// La date d'enregistrement accompagne chaque souvenir : sans elle,
		// le modèle ne peut pas arbitrer entre deux faits contradictoires ni
		// juger qu'une information est probablement périmée.
		if m.CreatedAt.IsZero() {
			fmt.Fprintf(&b, "%d. [id: %s] %s\n", i+1, m.ID, excerpt)
		} else {
			fmt.Fprintf(&b, "%d. [id: %s, recorded %s] %s\n", i+1, m.ID, m.CreatedAt.Format("2006-01-02"), excerpt)
		}
	}

	return b.String()
}

// newSearchMemoryTool construit l'outil "search_memory". La portée de
// recherche n'est jamais un paramètre du modèle : elle est entièrement
// déterminée par identity (plan de conception, Phase 10).
func (t MemoryTools) newSearchMemoryTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("query", "Search text.", "string")

	return llm.NewFuncTool(
		"search_memory",
		"Search the assistant's persistent memory. Scoping to the current conversation is automatic.",
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

// recallQueryLimit borne la taille du texte de requête du rappel
// automatique : un message très long dilue la recherche plus qu'il ne la
// sert.
const recallQueryLimit = 500

// recallExcerptLimit borne chaque souvenir injecté : le rappel se paie à
// chaque tour, banalités comprises, il doit rester léger.
const recallExcerptLimit = 300

// recallNote construit la section de rappel automatique du message système :
// les souvenirs les plus pertinents pour input, dans les portées lisibles
// par identity. Retourne "" si le rappel est désactivé, sans requête
// exploitable, ou en échec — le rappel n'est JAMAIS bloquant, et une portée
// non autorisée est ignorée comme pour search_memory.
func (t MemoryTools) recallNote(ctx context.Context, identity model.ExecutionIdentity, input string) string {
	if !t.Recall || !t.enabled() {
		return ""
	}

	query := strings.TrimSpace(input)
	if query == "" {
		// Exécution planifiée ou tour sans texte : rien à chercher.
		return ""
	}
	if runes := []rune(query); len(runes) > recallQueryLimit {
		query = string(runes[:recallQueryLimit])
	}

	results, err := t.searchAuthorizedScopes(ctx, identity, readScopes(identity), "read", query)
	if err != nil {
		return ""
	}

	limit := t.RecallMax
	if limit <= 0 {
		limit = 3
	}
	if len(results) > limit {
		results = results[:limit]
	}
	if len(results) == 0 {
		return ""
	}

	t.Metrics.IncMemoryRecall()

	var b strings.Builder
	b.WriteString("## Possibly relevant memories\n\n")
	b.WriteString("Retrieved automatically from your persistent memory based on the incoming message. They may be irrelevant: judge for yourself, and use search_memory for anything deeper.\n\n")
	for _, m := range results {
		excerpt := m.Content
		if runes := []rune(excerpt); len(runes) > recallExcerptLimit {
			excerpt = string(runes[:recallExcerptLimit]) + "…"
		}
		if m.CreatedAt.IsZero() {
			fmt.Fprintf(&b, "- %s\n", excerpt)
		} else {
			fmt.Fprintf(&b, "- (recorded %s) %s\n", m.CreatedAt.Format("2006-01-02"), excerpt)
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// newSearchHistoryTool construit l'outil "search_conversation_history".
// Contrairement à search_memory, qui balaie toutes les portées lisibles, la
// recherche épisodique est bornée à la portée de la conversation COURANTE :
// un épisode est du verbatim, il n'a de sens (et de légitimité) que là où
// il a été prononcé. La permission requise est la même lecture mémoire que
// search_memory.
func (t MemoryTools) newSearchHistoryTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("query", "Search text.", "string")

	return llm.NewFuncTool(
		"search_conversation_history",
		"Search verbatim excerpts of older messages from this conversation, beyond the recent history you already see. Useful to recall what was actually said or decided in past discussions. Results are dated fragments of the original exchange.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			query, _ := params["query"].(string)
			if strings.TrimSpace(query) == "" {
				return llm.NewToolResult("erreur: le paramètre 'query' est requis et ne peut pas être vide."), nil
			}

			scope := identity.Scope
			if scope != model.ScopePersonal && scope != model.ScopeGroup {
				return llm.NewToolResult("No conversation history is available in this context."), nil
			}

			if err := t.Authorizer.Authorize(ctx, authorization.AuthorizationRequest{
				Identity:      identity,
				Permission:    fmt.Sprintf("memory.%s.read", scope),
				TargetOrgID:   identity.OrgID,
				TargetScope:   scope,
				TargetScopeID: identity.ScopeID,
			}); err != nil {
				return llm.NewToolResult("No conversation history is available in this context."), nil
			}

			episodes, err := t.Episodes.SearchEpisodes(ctx, memory.EpisodeQuery{
				Text:       query,
				OrgID:      identity.OrgID,
				Scope:      scope,
				ScopeID:    identity.ScopeID,
				MaxResults: t.maxResults(),
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("erreur lors de la recherche d'historique: %v", err)), nil
			}

			return llm.NewToolResult(formatEpisodeList(episodes)), nil
		},
	)
}

// episodeExcerptLimit borne la taille d'un épisode restitué au modèle : un
// fragment est potentiellement une vague de compaction entière, bien plus
// long qu'un souvenir.
const episodeExcerptLimit = 2000

// formatEpisodeList formate episodes en liste datée.
func formatEpisodeList(episodes []memory.Episode) string {
	if len(episodes) == 0 {
		return "No matching conversation history found."
	}

	var b strings.Builder
	for i, ep := range episodes {
		excerpt := ep.Content
		if len(excerpt) > episodeExcerptLimit {
			excerpt = excerpt[:episodeExcerptLimit] + "…"
		}

		period := ""
		switch {
		case !ep.From.IsZero() && !ep.To.IsZero():
			period = fmt.Sprintf(" [%s → %s]", ep.From.Format("2006-01-02"), ep.To.Format("2006-01-02"))
		case !ep.From.IsZero():
			period = fmt.Sprintf(" [%s]", ep.From.Format("2006-01-02"))
		}

		fmt.Fprintf(&b, "%d.%s\n%s\n\n", i+1, period, excerpt)
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// newRememberTool construit l'outil "remember". La portée d'écriture n'est
// jamais un paramètre du modèle : elle est toujours celle de la conversation
// courante (jamais org, voir writeScope).
func (t MemoryTools) newRememberTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("content", "Text to store durably.", "string")

	return llm.NewFuncTool(
		"remember",
		"Store a piece of information in the assistant's persistent memory, within the scope of the current conversation.",
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

// newForgetMemoryTool construit l'outil "forget_memory" (plan de conception, §8.5,
// §10). Depuis la Phase 15, cet outil n'exécute plus jamais de suppression
// lui-même : une requête textuelle seule liste des candidats (inchangé), et
// un 'id' résolu sans ambiguïté produit une delegation.ProposedAction,
// accumulée dans collector, plutôt qu'un paramètre 'confirm' ad-hoc. C'est
// internal/action.Engine qui persiste cette proposition en
// persistence.ActionPlan (via internal/conversation.Handler, qui lit
// Result.ProposedActions après l'exécution de l'agent) et qui l'exécute
// réellement, seulement après confirmation explicite ("confirmer" dans la
// conversation, plan de conception, §10.4) et revérification complète (§10.5) — jamais
// au sein de ce tour d'outil.
func (t MemoryTools) newForgetMemoryTool(identity model.ExecutionIdentity, collector *proposalCollector) llm.Tool {
	schema := llm.NewJSONSchema().
		Property("query", "Text describing the entries to delete. Use it alone, without 'id', to get a numbered list of candidates.", "string").
		Property("id", "Exact id of the entry to delete, obtained from a prior search (search_memory, or an earlier forget_memory call with 'query').", "string")

	return llm.NewFuncTool(
		"forget_memory",
		"Propose deleting an entry from persistent memory. A text query alone never deletes anything: it only lists candidates. A precise 'id' proposes a deletion, carried out only after the user explicitly confirms in the conversation (replying \"confirmer\").",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			query := strings.TrimSpace(stringParam(params, "query"))
			id := strings.TrimSpace(stringParam(params, "id"))

			if id == "" {
				if query == "" {
					return llm.NewToolResult("erreur: fournir soit 'query' (pour lister des candidats), soit 'id' (pour proposer la suppression d'une mémoire précise)."), nil
				}

				// plan de conception, §8.5 : "une suppression par requête textuelle non
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
