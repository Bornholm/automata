package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/resource"
)

// todoListIDParam est le nom de champ conventionnel utilisé par le serveur
// MCP "todo" (réel ou fake, voir todo_test.go) pour désigner l'identifiant
// de liste de tâches ciblé par un appel d'outil. Aucun véritable serveur MCP
// de gestion de tâches n'est disponible dans ce dépôt (comme pour
// google-calendar en Phase 13) : cette convention est celle choisie pour le
// serveur fake construit pour les tests, faute de référence externe. Quelle
// que soit la valeur fournie par le modèle sous ce nom, l'application
// l'écrase systématiquement (PLAN.md §9.2).
const todoListIDParam = "list_id"

// todoProposalTTL est la durée de validité d'une proposition d'action
// d'écriture todo avant expiration. Même valeur que calendarProposalTTL
// (agenda.go), redéfinie ici plutôt que partagée : les deux spécialistes
// n'ont aucune raison structurelle d'évoluer ensemble, et une constante
// locale de trois lignes ne justifie pas un couplage entre agenda.go et
// todo.go pour ce seul nombre.
const todoProposalTTL = 5 * time.Minute

// todoReadPrefixes / isTodoWriteTool : même classification par préfixe de
// nom que l'agenda (agenda.go, calendarReadPrefixes) — voir le commentaire
// détaillé associé, qui documente pourquoi aucune classification native
// (readOnlyHint MCP) n'est disponible côté automata pour l'instant.
var todoReadPrefixes = []string{"list_", "get_", "search_", "find_"}

func isTodoWriteTool(name string) bool {
	for _, prefix := range todoReadPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// Sentinelles retournées par TodoProposalStore. Même principe qu'en agenda :
// un identifiant inconnu et un identifiant appartenant à une autre
// conversation retournent le même message, pour ne jamais laisser une
// conversation sonder l'existence de propositions d'une autre conversation.
var errTodoGroupNotFound = errors.New("groupe de propositions introuvable, déjà confirmé, ou n'appartenant pas à cette conversation")

// todoProposal décrit une action d'écriture todo (create/update/delete)
// proposée mais pas encore confirmée. Contrairement à calendarProposal
// (agenda.go, confirmation individuelle), chaque proposition appartient à un
// groupe (groupID) : PLAN.md Phase 14 exige de pouvoir proposer PLUSIEURS
// actions dans le même tour et de les confirmer ENSEMBLE en un seul appel
// (§6.4, "plusieurs actions par tour" ; travaux 5-7 de la Phase 14) — un
// écart assumé par rapport au cycle proposal_id/confirm par appel de
// l'agenda (Phase 13), qui ne convient pas à une confirmation groupée.
type todoProposal struct {
	seq            uint64
	toolName       string
	args           map[string]any
	conversationID model.ConversationID
	groupID        string
	// dedupeKey est vide sauf pour une proposition de création (préfixe
	// "create_") : voir todoDedupeKey.
	dedupeKey string
	createdAt time.Time
}

// TodoProposalStore mémorise en RAM les propositions d'action d'écriture
// todo en attente de confirmation groupée, protégées par un mutex. Même
// squelette que CalendarProposalStore (agenda.go) : pas de persistance
// SQLite à ce stade, horloge injectable pour les tests d'expiration. Une
// valeur zéro n'est pas utilisable : construire via NewTodoProposalStore.
type TodoProposalStore struct {
	mu           sync.Mutex
	proposals    map[string]*todoProposal
	counter      uint64
	groupCounter uint64
	ttl          time.Duration
	// Now, si non nil, remplace time.Now (horloge injectable pour les
	// tests d'expiration, voir todo_test.go).
	Now func() time.Time
}

// NewTodoProposalStore construit un TodoProposalStore vide, avec
// l'expiration par défaut (todoProposalTTL).
func NewTodoProposalStore() *TodoProposalStore {
	return &TodoProposalStore{
		proposals: make(map[string]*todoProposal),
		ttl:       todoProposalTTL,
	}
}

func (s *TodoProposalStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NewGroupID génère un nouvel identifiant de groupe de propositions. Appelé
// une seule fois par tour (une exécution de MCPToolAgent.Execute pour le
// spécialiste todo), voir newTodoToolsRewriter : toutes les propositions
// d'écriture créées pendant ce même tour partagent ce groupID.
func (s *TodoProposalStore) NewGroupID() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.groupCounter++
	return fmt.Sprintf("todo-group-%d", s.groupCounter)
}

// propose enregistre une nouvelle proposition d'écriture todo pour
// conversationID dans le groupe groupID, et retourne son identifiant.
//
// Déduplication des créations (PLAN.md Phase 14, travail 4) : si dedupeKey
// est non vide et qu'une proposition en attente NON EXPIRÉE de cette même
// conversation partage déjà exactement ce dedupeKey, aucune nouvelle
// proposition n'est créée — l'identifiant existant est retourné avec
// reused=true. Portée volontairement limitée : seules les propositions
// encore en attente (de ce tour ou d'un tour précédent non confirmé/expiré)
// de LA MÊME conversation sont comparées ; aucune comparaison n'est faite
// contre des tâches déjà présentes côté serveur MCP réel, puisqu'aucun outil
// de lecture dédié ne permet de l'observer sans spéculation (voir
// todoDedupeKey pour la normalisation appliquée).
func (s *TodoProposalStore) propose(conversationID model.ConversationID, groupID, toolName string, args map[string]any, dedupeKey string) (id string, reused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if dedupeKey != "" {
		now := s.now()
		for existingID, p := range s.proposals {
			if p.conversationID != conversationID || p.dedupeKey != dedupeKey {
				continue
			}
			if now.Sub(p.createdAt) > s.ttl {
				continue
			}
			return existingID, true
		}
	}

	s.counter++
	id = fmt.Sprintf("todo-proposal-%d", s.counter)

	s.proposals[id] = &todoProposal{
		seq:            s.counter,
		toolName:       toolName,
		args:           args,
		conversationID: conversationID,
		groupID:        groupID,
		dedupeKey:      dedupeKey,
		createdAt:      s.now(),
	}

	return id, false
}

// PendingCountForTest retourne le nombre de propositions en attente pour
// conversationID, tous groupes confondus. Exportée uniquement pour permettre
// à todo_test.go de vérifier structurellement l'absence de doublon interne
// au store (PLAN.md Phase 14, déduplication) sans exposer autrement l'état
// interne du store.
func (s *TodoProposalStore) PendingCountForTest(conversationID model.ConversationID) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, p := range s.proposals {
		if p.conversationID == conversationID {
			count++
		}
	}
	return count
}

// todoGroupEntry est une proposition retirée du store par consumeGroup, prête
// à être exécutée (ou rapportée en échec si expired).
type todoGroupEntry struct {
	id       string
	toolName string
	args     map[string]any
	expired  bool
}

type matchedTodoProposal struct {
	id string
	p  *todoProposal
}

// consumeGroup retire et retourne, DANS L'ORDRE DE PROPOSITION (champ seq),
// toutes les propositions en attente pour (conversationID, groupID) — jamais
// de confirmation croisée entre conversations, même principe que
// CalendarProposalStore.consume. Si aucune proposition ne correspond (groupe
// inconnu, déjà confirmé, ou appartenant à une autre conversation),
// errTodoGroupNotFound est retournée.
//
// Une proposition individuellement expirée n'est PAS silencieusement
// ignorée : elle est retournée avec expired=true pour que l'appelant la
// reporte comme un échec explicite dans le rapport de confirmation groupée,
// plutôt que de disparaître sans explication (succès partiel explicite,
// jamais un "tout ou rien" silencieux — PLAN.md Phase 14, travail 7).
func (s *TodoProposalStore) consumeGroup(conversationID model.ConversationID, groupID string) ([]todoGroupEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var matched []matchedTodoProposal
	for id, p := range s.proposals {
		if p.conversationID != conversationID || p.groupID != groupID {
			continue
		}
		matched = append(matched, matchedTodoProposal{id: id, p: p})
	}

	if len(matched) == 0 {
		return nil, errTodoGroupNotFound
	}

	sort.Slice(matched, func(i, j int) bool { return matched[i].p.seq < matched[j].p.seq })

	now := s.now()
	entries := make([]todoGroupEntry, 0, len(matched))
	for _, m := range matched {
		entries = append(entries, todoGroupEntry{
			id:       m.id,
			toolName: m.p.toolName,
			args:     m.p.args,
			expired:  now.Sub(m.p.createdAt) > s.ttl,
		})
		delete(s.proposals, m.id)
	}

	return entries, nil
}

// todoDedupeKey calcule une clé de déduplication triviale pour une
// proposition de création de tâche : les arguments résolus (list_id compris)
// sérialisés en JSON (encoding/json trie les clés d'une map), puis normalisés
// par un simple TrimSpace + passage en minuscules. Aucune similarité floue
// n'est tentée (comparaison stricte uniquement, portée volontairement
// limitée documentée sur TodoProposalStore.propose) : deux créations dont le
// contenu diffère ne serait-ce que par la ponctuation ne sont pas détectées
// comme doublons, ce serait spéculatif.
func todoDedupeKey(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(raw)))
}

// wrapTodoReadTool enveloppe un outil de lecture (list_tasks, get_task,
// search_tasks, ...) pour réécrire systématiquement list_id vers listID,
// quelle que soit la valeur fournie par le modèle (PLAN.md §9.2). Aucune
// confirmation n'est requise (PLAN.md §10.1).
func wrapTodoReadTool(tool llm.Tool, listID string) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params)+1)
		maps.Copy(args, params)
		args[todoListIDParam] = listID

		return tool.Execute(ctx, args)
	}

	return llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)
}

// wrapTodoWriteTool enveloppe un outil d'écriture (create_task, update_task,
// delete_task, ...) : l'appel n'exécute JAMAIS l'outil MCP réel, il
// enregistre toujours une proposition dans store (groupée sous groupID) et
// retourne son identifiant au modèle. L'exécution réelle n'a lieu que via
// confirm_todo_actions (newConfirmTodoActionsTool), jamais depuis cet outil
// lui-même : c'est la différence structurelle avec wrapCalendarWriteTool
// (agenda.go), dont le cycle proposal_id/confirm est porté par l'outil
// d'écriture lui-même — un choix qui ne permet de confirmer qu'UNE action à
// la fois, alors que PLAN.md §6.4/Phase 14 exige une confirmation groupée de
// plusieurs actions proposées dans le même tour.
func wrapTodoWriteTool(tool llm.Tool, store *TodoProposalStore, conversationID model.ConversationID, groupID, listID string) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params)+1)
		maps.Copy(args, params)
		// Réécriture systématique, quelle que soit la valeur fournie par le
		// modèle sous ce nom (PLAN.md §9.2).
		args[todoListIDParam] = listID

		dedupeKey := ""
		if strings.HasPrefix(tool.Name(), "create_") {
			dedupeKey = todoDedupeKey(args)
		}

		id, reused := store.propose(conversationID, groupID, tool.Name(), args, dedupeKey)

		argsJSON, err := json.Marshal(args)
		if err != nil {
			argsJSON = []byte("{}")
		}

		if reused {
			return llm.NewToolResult(fmt.Sprintf(
				"Une proposition de contenu identique est déjà en attente dans ce groupe, aucun doublon n'a été créé.\nOutil: %s\nArguments résolus: %s\nID de proposition existant: %s\nID de groupe: %s\nPour exécuter TOUTES les actions en attente de ce groupe, appelle confirm_todo_actions avec group_id=%q et confirm=true, après confirmation explicite de l'utilisateur humain.",
				tool.Name(), string(argsJSON), id, groupID, groupID,
			)), nil
		}

		return llm.NewToolResult(fmt.Sprintf(
			"Action proposée, en attente de confirmation groupée (expire dans %s) :\nOutil: %s\nArguments résolus: %s\nID de proposition: %s\nID de groupe: %s\nD'autres actions peuvent être proposées dans ce même groupe avant confirmation. Pour exécuter TOUTES les actions en attente de ce groupe (celle-ci comprise), appelle confirm_todo_actions avec group_id=%q et confirm=true, après confirmation explicite de l'utilisateur humain.",
			todoProposalTTL, tool.Name(), string(argsJSON), id, groupID, groupID,
		)), nil
	}

	return llm.NewFuncTool(tool.Name(), tool.Description()+" (nécessite une confirmation groupée : voir l'outil confirm_todo_actions)", tool.Parameters(), execute)
}

// newConfirmTodoActionsTool construit l'outil confirm_todo_actions : le point
// d'entrée unique de confirmation groupée (PLAN.md Phase 14, travaux 5-7).
// Quand confirm=true, TOUTES les propositions en attente pour group_id (et
// pour conversationID — jamais de confirmation croisée entre conversations)
// sont exécutées SÉQUENTIELLEMENT dans leur ordre de proposition, en
// continuant même si l'une d'elles échoue : le rapport retourné distingue
// explicitement chaque succès et chaque échec (avec son motif), jamais un
// "tout ou rien" silencieux.
//
// executors associe le nom de chaque outil d'écriture todo brut (non
// enveloppé) à son llm.Tool réel, tel que récupéré du serveur MCP pour CE
// tour (voir newTodoToolsRewriter) : une proposition créée lors d'un tour
// précédent reste exécutable, puisque le nom d'outil suffit à retrouver
// l'exécuteur courant du même serveur MCP.
func newConfirmTodoActionsTool(store *TodoProposalStore, conversationID model.ConversationID, executors map[string]llm.Tool) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("group_id", "Identifiant du groupe de propositions à confirmer, retourné par un outil d'écriture todo précédent (create_task, update_task, delete_task, ...).", "string").
		RequiredProperty("confirm", "true uniquement pour exécuter réellement les actions du groupe. Ne jamais mettre à true sans confirmation explicite de l'utilisateur humain.", "boolean")

	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		groupID := stringParam(params, "group_id")
		confirm, _ := params["confirm"].(bool)

		if groupID == "" {
			return llm.NewToolResult("erreur: le paramètre 'group_id' est requis."), nil
		}
		if !confirm {
			return llm.NewToolResult("erreur: confirm doit être true pour exécuter les actions du groupe ; ne l'appelle qu'après confirmation explicite de l'utilisateur humain."), nil
		}

		entries, err := store.consumeGroup(conversationID, groupID)
		if err != nil {
			return llm.NewToolResult(fmt.Sprintf("erreur: %v.", err)), nil
		}

		var report strings.Builder
		fmt.Fprintf(&report, "Rapport d'exécution du groupe %s (ordre de proposition) :\n", groupID)

		succeeded, failed := 0, 0
		for i, entry := range entries {
			if entry.expired {
				failed++
				fmt.Fprintf(&report, "%d. %s : ÉCHEC (proposition expirée, à reproposer)\n", i+1, entry.toolName)
				continue
			}

			tool, ok := executors[entry.toolName]
			if !ok {
				failed++
				fmt.Fprintf(&report, "%d. %s : ÉCHEC (outil introuvable pour cette session)\n", i+1, entry.toolName)
				continue
			}

			result, err := tool.Execute(ctx, entry.args)
			if err != nil {
				failed++
				fmt.Fprintf(&report, "%d. %s : ÉCHEC (%v)\n", i+1, entry.toolName, err)
				continue
			}

			text := ""
			if result != nil {
				text = result.Text()
			}
			// Un outil MCP dont le handler retourne une erreur applicative
			// n'échoue pas au niveau du protocole (le SDK MCP la transforme
			// en CallToolResult avec IsError=true) : elle atterrit ici comme
			// un texte de résultat préfixé "ERROR:" (voir
			// genai/mcp/common.toToolResult), pas comme une erreur Go. C'est
			// le seul signal disponible pour détecter un échec applicatif
			// d'un outil réel (ou fake, voir todo_test.go).
			if strings.HasPrefix(strings.TrimSpace(text), "ERROR:") {
				failed++
				reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "ERROR:"))
				fmt.Fprintf(&report, "%d. %s : ÉCHEC (%s)\n", i+1, entry.toolName, reason)
				continue
			}

			succeeded++
			fmt.Fprintf(&report, "%d. %s : SUCCÈS (%s)\n", i+1, entry.toolName, text)
		}

		fmt.Fprintf(&report, "Bilan : %d action(s) réussie(s), %d échouée(s), sur %d au total.", succeeded, failed, len(entries))

		return llm.NewToolResult(report.String()), nil
	}

	return llm.NewFuncTool(
		"confirm_todo_actions",
		"Confirme et exécute séquentiellement TOUTES les actions todo (créations/modifications/suppressions) proposées et encore en attente dans un même groupe, même si l'une d'elles échoue en cours de route (les suivantes s'exécutent quand même). Retourne un rapport listant explicitement chaque succès et chaque échec.",
		schema,
		execute,
	)
}

// newTodoToolsRewriter construit un ToolsRewriterFunc (voir
// mcp_tool_agent.go) qui : résout listID pour la portée de la conversation
// courante (jamais une portée fournie par le modèle, PLAN.md §9.2) via
// resource.ResolveTodoListID, génère un groupID PARTAGÉ par toutes les
// propositions d'écriture de ce tour (une exécution de
// MCPToolAgent.Execute), enveloppe chaque outil du serveur MCP todo selon sa
// classification lecture/écriture (isTodoWriteTool), et ajoute
// confirm_todo_actions comme outil supplémentaire.
//
// Si aucune ressource "todo" n'est configurée pour la portée courante, la
// résolution échoue et le rewriter retourne une erreur avant tout appel au
// modèle — même principe que l'agenda (agenda.go, newCalendarToolsRewriter)
// et que l'échec de connexion à un serveur MCP (MCPToolAgent.Execute, Phase
// 12) : ne jamais deviner une ressource absente de la configuration.
func newTodoToolsRewriter(cfg *config.Config, store *TodoProposalStore) ToolsRewriterFunc {
	return func(ctx context.Context, req Request, tools []llm.Tool) ([]llm.Tool, error) {
		listID, err := resource.ResolveTodoListID(cfg, req.Conversation.Scope, req.Conversation.ScopeID)
		if err != nil {
			return nil, err
		}

		groupID := store.NewGroupID()

		executors := make(map[string]llm.Tool, len(tools))
		wrapped := make([]llm.Tool, 0, len(tools)+1)

		for _, tool := range tools {
			if isTodoWriteTool(tool.Name()) {
				executors[tool.Name()] = tool
				wrapped = append(wrapped, wrapTodoWriteTool(tool, store, req.Conversation.ID, groupID, listID))
			} else {
				wrapped = append(wrapped, wrapTodoReadTool(tool, listID))
			}
		}

		wrapped = append(wrapped, newConfirmTodoActionsTool(store, req.Conversation.ID, executors))

		return wrapped, nil
	}
}

// NewTodoToolAgent construit le spécialiste todo : un MCPToolAgent
// (mcp_tool_agent.go) enrichi du comportement de résolution de ressource et
// de confirmation groupée décrit ci-dessus (PLAN.md §9.2, §10.1, §6.4, Phase
// 14), même schéma de composition que NewAgendaToolAgent (agenda.go).
//
// store peut être nil : un TodoProposalStore neuf est alors créé. Fournir
// explicitement un store (typiquement partagé par tous les agents "todo"
// d'un même processus) permet aux tests d'injecter une horloge
// (TodoProposalStore.Now) pour simuler l'expiration d'une proposition, et de
// vérifier l'absence de doublon interne au store.
func NewTodoToolAgent(
	client llm.ChatCompletionClient,
	systemPrompt, agentName, orgDisplayName string,
	mcpManager *mcp.Manager,
	mcpServerNames []string,
	mcpLimits mcp.Limits,
	maxSequentialToolCalls int,
	cfg *config.Config,
	store *TodoProposalStore,
) *MCPToolAgent {
	if store == nil {
		store = NewTodoProposalStore()
	}

	base := NewMCPToolAgent(client, systemPrompt, agentName, orgDisplayName, mcpManager, mcpServerNames, mcpLimits, maxSequentialToolCalls)

	return base.WithToolsRewriter(newTodoToolsRewriter(cfg, store))
}
