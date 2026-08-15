package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/resource"
)

// calendarIDParam est le nom de champ conventionnel utilisé par le serveur
// MCP google-calendar (réel ou fake, voir agenda_test.go) pour désigner
// l'identifiant de calendrier ciblé par un appel d'outil. Aucun véritable
// serveur MCP Google Calendar n'est disponible dans ce dépôt (voir
// AGENTS.md, PLAN.md Phase 13) : cette convention de nommage est celle
// choisie pour le serveur fake construit pour les tests, faute de référence
// externe. Quelle que soit la valeur fournie par le modèle sous ce nom,
// l'application l'écrase systématiquement (PLAN.md §9.2).
const calendarIDParam = "calendar_id"

// calendarProposalTTL est la durée de validité d'une proposition d'action
// d'écriture agenda avant expiration (PLAN.md §10, "Confirmation et
// exécution des actions"). 5 minutes est une valeur raisonnable pour
// laisser le temps à un humain de répondre "oui" dans une conversation
// applicative, sans laisser une proposition confirmable indéfiniment
// longtemps après le contexte qui l'a produite.
const calendarProposalTTL = 5 * time.Minute

// calendarReadPrefixes / calendarWritePrefixes classent les outils du
// serveur MCP google-calendar par convention de nommage plutôt que par une
// annotation protocolaire.
//
// Décision documentée (PLAN.md Phase 13) : le SDK MCP
// (modelcontextprotocol/go-sdk, via genai/mcp/common.Client) expose bien des
// annotations d'outil readOnlyHint côté protocole (goMCP.Tool.Annotations)
// et genai/mcp/common.Client les consulte pour un filtre optionnel côté
// connexion (common.WithReadOnlyHint, qui exclut purement et simplement les
// outils non readOnlyHint de GetTools). Mais cette information n'est PAS
// transportée jusqu'à llm.Tool pour les outils conservés :
// common.Client.toTool (github.com/bornholm/genai, mcp/common/client.go) ne
// transporte que le nom, la description, le schéma et une fonction
// d'exécution — jamais l'annotation elle-même. Une fois les outils obtenus
// via mcp.Manager.GetTools (internal/mcp/manager.go), aucune classification
// lecture/écriture native n'est donc disponible côté automata.
//
// La classification ci-dessous est donc un choix explicite par convention
// de nommage, assumé et documenté ici plutôt que dissimulé : les préfixes
// "list_"/"get_"/"search_"/"find_" sont des lectures exécutées directement
// (PLAN.md §10.1, "les recherches et lectures peuvent être exécutées
// directement") ; tout le reste, y compris un préfixe inconnu, est traité
// en écriture nécessitant confirmation — position prudente par défaut,
// jamais l'inverse.
var calendarReadPrefixes = []string{"list_", "get_", "search_", "find_"}

// isCalendarWriteTool implémente la classification documentée ci-dessus.
func isCalendarWriteTool(name string) bool {
	for _, prefix := range calendarReadPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// calendarDateLikeParams énumère les noms de paramètres traités comme des
// dates/heures devant respecter RFC3339 avec fuseau horaire explicite
// (PLAN.md Phase 13, "gestion des dates"). Convention pragmatique choisie
// en l'absence de schéma de serveur MCP Google Calendar réel : les noms
// usuels d'un outil de création/modification d'événement.
var calendarDateLikeParams = map[string]bool{
	"start":      true,
	"end":        true,
	"start_time": true,
	"end_time":   true,
	"starts_at":  true,
	"ends_at":    true,
	"start_date": true,
	"end_date":   true,
	"date":       true,
	"datetime":   true,
}

// validateCalendarDates refuse args si l'un des paramètres reconnus comme
// une date (calendarDateLikeParams) n'est pas une chaîne au format
// RFC3339/ISO8601 avec fuseau horaire explicite. Aucune tentative n'est
// faite pour deviner une date ambiguë ou sans fuseau (PLAN.md Phase 13,
// "jamais de tentative de deviner une date") : une erreur claire est
// retournée à la place.
//
// La détection de conflits d'agenda (chevauchement d'événements) n'est
// délibérément PAS implémentée ici : aucun calendrier réel n'est
// disponible dans ce dépôt pour la valider, ce serait spéculatif. Le
// system_prompt configuré pour l'agent agenda DOIT demander explicitement
// au modèle de consulter list_events avant de proposer une création,
// lorsque cet outil est disponible (voir le message de proposition retourné
// par wrapCalendarWriteTool, qui le rappelle également au modèle).
func validateCalendarDates(args map[string]any) error {
	for key, raw := range args {
		if !calendarDateLikeParams[strings.ToLower(key)] {
			continue
		}

		value, ok := raw.(string)
		if !ok {
			return fmt.Errorf("le paramètre %q doit être une chaîne de date ISO8601/RFC3339 avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00)", key)
		}

		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("le paramètre %q (%q) n'est pas une date ISO8601/RFC3339 valide avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00): %w", key, value, err)
		}
	}

	return nil
}

// Sentinelles retournées par CalendarProposalStore.consume. Le même message
// "introuvable" est utilisé pour un identifiant inconnu et pour un
// identifiant appartenant à une autre conversation : révéler la différence
// permettrait à une conversation de sonder l'existence de propositions
// d'une autre conversation.
var (
	errCalendarProposalNotFound  = errors.New("proposition introuvable ou déjà utilisée")
	errCalendarProposalExpired   = errors.New("proposition expirée, recommence la demande")
	errCalendarProposalWrongTool = errors.New("proposition introuvable pour cet outil")
)

// calendarProposal décrit une action d'écriture agenda proposée mais pas
// encore confirmée : la mécanique de confirmation ad-hoc, scopée, du même
// niveau de simplicité que forget_memory (internal/agent/memory_tools.go,
// Phase 10) — PAS le système ActionPlan général de la Phase 15
// (persistance SQLite, expiration formalisée, commandes conversationnelles
// confirmer/annuler), hors périmètre de cette phase.
type calendarProposal struct {
	toolName       string
	args           map[string]any
	conversationID model.ConversationID
	createdAt      time.Time
}

// CalendarProposalStore mémorise en RAM (aucune persistance SQLite à ce
// stade, voir calendarProposal) les propositions d'action d'écriture
// agenda en attente de confirmation, protégées par un mutex. Une valeur
// zéro n'est pas utilisable : construire via NewCalendarProposalStore.
type CalendarProposalStore struct {
	mu        sync.Mutex
	proposals map[string]*calendarProposal
	counter   uint64
	ttl       time.Duration
	// Now, si non nil, remplace time.Now pour déterminer l'heure courante
	// (horloge injectable pour les tests d'expiration, voir agenda_test.go).
	Now func() time.Time
}

// NewCalendarProposalStore construit un CalendarProposalStore vide, avec
// l'expiration par défaut (calendarProposalTTL).
func NewCalendarProposalStore() *CalendarProposalStore {
	return &CalendarProposalStore{
		proposals: make(map[string]*calendarProposal),
		ttl:       calendarProposalTTL,
	}
}

func (s *CalendarProposalStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// propose enregistre une nouvelle proposition et retourne son identifiant.
func (s *CalendarProposalStore) propose(conversationID model.ConversationID, toolName string, args map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counter++
	id := fmt.Sprintf("cal-proposal-%d", s.counter)

	s.proposals[id] = &calendarProposal{
		toolName:       toolName,
		args:           args,
		conversationID: conversationID,
		createdAt:      s.now(),
	}

	return id
}

// consume vérifie et retire la proposition id, à condition qu'elle existe,
// n'ait pas expiré, appartienne à conversationID et concerne toolName.
// Retourne ses arguments résolus (calendar_id déjà réinjecté par propose)
// en cas de succès.
func (s *CalendarProposalStore) consume(id string, conversationID model.ConversationID, toolName string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.proposals[id]
	if !ok {
		return nil, errCalendarProposalNotFound
	}

	// Pas de confirmation croisée entre conversations : voir le commentaire
	// des sentinelles ci-dessus.
	if p.conversationID != conversationID {
		return nil, errCalendarProposalNotFound
	}

	if p.toolName != toolName {
		return nil, errCalendarProposalWrongTool
	}

	if s.now().Sub(p.createdAt) > s.ttl {
		delete(s.proposals, id)
		return nil, errCalendarProposalExpired
	}

	delete(s.proposals, id)

	return p.args, nil
}

// augmentSchemaForConfirmation retourne une copie du schéma JSON tool
// (map[string]any au format produit par llm.NewJSONSchema/mcp) en y
// ajoutant deux propriétés optionnelles, proposal_id et confirm : c'est par
// ces deux champs, et non par un outil séparé, que le modèle confirme une
// action déjà proposée (choix documenté PLAN.md Phase 13 : "le plus simple
// à faire comprendre au modèle", même pattern que forget_memory/confirm en
// Phase 10).
func augmentSchemaForConfirmation(schema map[string]any) map[string]any {
	if schema == nil {
		schema = llm.NewJSONSchema()
	}

	augmented := make(map[string]any, len(schema))
	for k, v := range schema {
		augmented[k] = v
	}

	properties, _ := augmented["properties"].(map[string]any)
	newProperties := make(map[string]any, len(properties)+2)
	for k, v := range properties {
		newProperties[k] = v
	}

	newProperties["proposal_id"] = map[string]any{
		"type":        "string",
		"description": "Identifiant de proposition à fournir avec confirm=true pour exécuter réellement une action déjà proposée par un appel précédent de cet outil.",
	}
	newProperties["confirm"] = map[string]any{
		"type":        "boolean",
		"description": "true uniquement pour confirmer une action déjà proposée (avec 'proposal_id'). Ne jamais mettre à true sans confirmation explicite de l'utilisateur humain.",
	}

	augmented["properties"] = newProperties

	return augmented
}

// wrapCalendarReadTool enveloppe un outil de lecture (list_events,
// get_event, ...) pour réécrire systématiquement calendar_id vers
// calendarID, quelle que soit la valeur fournie par le modèle (PLAN.md
// §9.2). Aucune confirmation n'est requise (PLAN.md §10.1).
func wrapCalendarReadTool(tool llm.Tool, calendarID string) llm.Tool {
	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		args := make(map[string]any, len(params)+1)
		for k, v := range params {
			args[k] = v
		}
		args[calendarIDParam] = calendarID

		return tool.Execute(ctx, args)
	}

	return llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)
}

// wrapCalendarWriteTool enveloppe un outil d'écriture (create_event,
// update_event, delete_event, ...) dans le cycle proposition/confirmation
// décrit par PLAN.md §10 : le premier appel (confirm absent ou faux)
// n'exécute JAMAIS l'outil MCP réel, il enregistre une proposition dans
// store et retourne son identifiant au modèle ; un second appel avec
// proposal_id et confirm=true exécute réellement l'outil, avec les
// arguments résolus lors de la proposition (calendar_id compris), jamais
// avec de nouveaux arguments fournis sur l'appel de confirmation — ce qui
// empêche le modèle de faire glisser silencieusement les arguments entre la
// proposition et la confirmation.
func wrapCalendarWriteTool(tool llm.Tool, store *CalendarProposalStore, conversationID model.ConversationID, calendarID string) llm.Tool {
	schema := augmentSchemaForConfirmation(tool.Parameters())

	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		confirm, _ := params["confirm"].(bool)
		proposalID := stringParam(params, "proposal_id")

		if confirm {
			if proposalID == "" {
				return llm.NewToolResult("erreur: le paramètre 'proposal_id' est requis pour confirmer une action."), nil
			}

			args, err := store.consume(proposalID, conversationID, tool.Name())
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("erreur: %v.", err)), nil
			}

			return tool.Execute(ctx, args)
		}

		args := make(map[string]any, len(params))
		for k, v := range params {
			if k == "confirm" || k == "proposal_id" {
				continue
			}
			args[k] = v
		}
		// Réécriture systématique, quelle que soit la valeur fournie par le
		// modèle sous ce nom (PLAN.md §9.2).
		args[calendarIDParam] = calendarID

		if err := validateCalendarDates(args); err != nil {
			return llm.NewToolResult(fmt.Sprintf(
				"erreur: %v. Fournis une date précise au format ISO8601/RFC3339 avec fuseau horaire explicite (ex: 2026-09-12T14:00:00+02:00) ; aucune action n'a été proposée.",
				err,
			)), nil
		}

		id := store.propose(conversationID, tool.Name(), args)

		argsJSON, err := json.Marshal(args)
		if err != nil {
			argsJSON = []byte("{}")
		}

		return llm.NewToolResult(fmt.Sprintf(
			"Action proposée, en attente de confirmation (expire dans %s) :\nOutil: %s\nArguments résolus: %s\nID de proposition: %s\nPour confirmer, rappelle cet outil avec proposal_id=%q et confirm=true. Si un outil de lecture des événements existants est disponible, consulte-le d'abord pour éviter tout chevauchement avant de confirmer une création.",
			calendarProposalTTL, tool.Name(), string(argsJSON), id, id,
		)), nil
	}

	return llm.NewFuncTool(tool.Name(), tool.Description()+" (nécessite confirmation avant exécution réelle : voir les paramètres proposal_id/confirm)", schema, execute)
}

// newCalendarToolsRewriter construit un ToolsRewriterFunc (voir
// mcp_tool_agent.go) qui : résout calendarID pour la portée de la
// conversation courante (jamais une portée fournie par le modèle, PLAN.md
// §9.2) via resource.ResolveCalendarID, puis enveloppe chaque outil du
// serveur MCP google-calendar selon sa classification lecture/écriture
// (isCalendarWriteTool).
//
// Si aucune ressource "calendar" n'est configurée pour la portée courante,
// la résolution échoue et le rewriter retourne une erreur avant tout appel
// au modèle — même principe que l'échec de connexion à un serveur MCP
// (MCPToolAgent.Execute, Phase 12) : ne jamais deviner une ressource
// absente de la configuration.
func newCalendarToolsRewriter(cfg *config.Config, store *CalendarProposalStore) ToolsRewriterFunc {
	return func(ctx context.Context, req Request, tools []llm.Tool) ([]llm.Tool, error) {
		calendarID, err := resource.ResolveCalendarID(cfg, req.Conversation.Scope, req.Conversation.ScopeID)
		if err != nil {
			return nil, err
		}

		wrapped := make([]llm.Tool, len(tools))
		for i, tool := range tools {
			if isCalendarWriteTool(tool.Name()) {
				wrapped[i] = wrapCalendarWriteTool(tool, store, req.Conversation.ID, calendarID)
			} else {
				wrapped[i] = wrapCalendarReadTool(tool, calendarID)
			}
		}

		return wrapped, nil
	}
}

// NewAgendaToolAgent construit le spécialiste agenda : un MCPToolAgent
// (mcp_tool_agent.go) enrichi du comportement de résolution de ressource et
// de confirmation d'écriture décrit ci-dessus (PLAN.md §9.2, §10.1,
// Phase 13), plutôt qu'un nouveau type dupliquant runToolLoop/la
// récupération des outils mcp — voir ToolsRewriterFunc.
//
// store peut être nil : un CalendarProposalStore neuf est alors créé.
// Fournir explicitement un store (typiquement partagé par tous les agents
// "agenda" d'un même processus, comme pour mcpManager) permet aux tests
// d'injecter une horloge (CalendarProposalStore.Now) pour simuler
// l'expiration d'une proposition.
func NewAgendaToolAgent(
	client llm.ChatCompletionClient,
	systemPrompt, agentName, orgDisplayName string,
	mcpManager *mcp.Manager,
	mcpServerNames []string,
	mcpLimits mcp.Limits,
	maxSequentialToolCalls int,
	cfg *config.Config,
	store *CalendarProposalStore,
) *MCPToolAgent {
	if store == nil {
		store = NewCalendarProposalStore()
	}

	base := NewMCPToolAgent(client, systemPrompt, agentName, orgDisplayName, mcpManager, mcpServerNames, mcpLimits, maxSequentialToolCalls)

	return base.WithToolsRewriter(newCalendarToolsRewriter(cfg, store))
}
