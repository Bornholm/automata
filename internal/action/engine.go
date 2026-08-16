package action

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/resource"
)

// Statuts du cycle de vie d'un plan d'actions (PLAN.md §10.2).
const (
	StatusAwaitingConfirmation = "awaiting_confirmation"
	StatusConfirmed            = "confirmed"
	StatusExecuting            = "executing"
	StatusSucceeded            = "succeeded"
	StatusPartiallySucceeded   = "partially_succeeded"
	StatusFailed               = "failed"
	StatusExpired              = "expired"
	StatusCancelled            = "cancelled"
)

// InternalServer est la valeur conventionnelle de Action.MCPServer pour une
// action exécutée par un exécuteur applicatif interne (voir Executor),
// plutôt que par un appel à un serveur MCP (PLAN.md §10, retrofit mémoire).
// Exportée pour que les producteurs de delegation.ProposedAction (ex :
// internal/agent.MemoryTools) puissent y référer sans dupliquer la chaîne
// littérale.
const InternalServer = "internal"

// MemoryForgetTool est le nom conventionnel de l'action interne de
// suppression de mémoire (delegation.ProposedAction.ToolName), utilisée par
// internal/agent.MemoryTools.
const MemoryForgetTool = "memory.forget"

// defaultPlanTTL est la durée de validité par défaut d'un plan d'actions
// avant expiration (PLAN.md §10.3), reprise de la valeur déjà choisie par
// les mécanismes ad-hoc des phases précédentes (agenda.go
// calendarProposalTTL, todo.go todoProposalTTL) : cinq minutes est
// raisonnable pour laisser le temps à un humain de répondre "confirmer"
// sans laisser une proposition confirmable indéfiniment.
const defaultPlanTTL = 5 * time.Minute

// Executor exécute réellement une action persistée, au moment de la
// confirmation d'un plan (PLAN.md §10.5 points 6-8). Une implémentation
// typique adapte soit un appel d'outil MCP (mcpExecutor, par défaut, pour
// tout Action.MCPServer autre que "internal"), soit une opération
// applicative interne (memoryForgetExecutor, pour MCPServer == "internal").
type Executor interface {
	// Execute exécute act pour identity (l'identité ACTUELLE du
	// confirmateur, jamais celle, potentiellement périmée, de la
	// proposition) au sein de plan, avec args reconstruits depuis
	// act.ArgumentsJSON. Retourne un texte de résultat lisible.
	Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error)
}

// Engine est le moteur central des plans d'actions (PLAN.md §10, Phase 15).
// Une valeur zéro n'est pas utilisable : construire via NewEngine.
type Engine struct {
	db          *persistence.DB
	plans       *persistence.ActionPlanRepository
	actions     *persistence.ActionRepository
	authorizer  *authorization.Authorizer
	mcpManager  *mcp.Manager
	cfg         *config.Config
	now         func() time.Time
	planTTL     time.Duration
	executors   map[string]Executor
	auditEvents *persistence.AuditEventRepository
	logger      *slog.Logger
	metrics     *observability.Metrics
}

// Option configure un Engine à la construction.
type Option func(*Engine)

// WithClock remplace l'horloge par défaut (time.Now) par now, pour les
// tests d'expiration.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithPlanTTL remplace la durée de validité par défaut d'un plan.
func WithPlanTTL(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.planTTL = d
		}
	}
}

// WithMemoryStore active l'exécution des actions internes de suppression de
// mémoire (MemoryForgetTool) au moment de la confirmation, en enregistrant
// l'exécuteur correspondant sous la clé internalServer.
func WithMemoryStore(store memory.Store) Option {
	return func(e *Engine) {
		if store != nil {
			e.executors[InternalServer] = &memoryForgetExecutor{store: store}
		}
	}
}

// WithExecutor enregistre (ou remplace) l'exécuteur utilisé pour les
// actions dont Action.MCPServer vaut mcpServer. Permet notamment aux tests
// d'isoler Engine du transport MCP réel avec un exécuteur factice (voir
// engine_test.go), et à un appelant réel d'étendre les serveurs internes
// supportés au-delà de la mémoire.
func WithExecutor(mcpServer string, ex Executor) Option {
	return func(e *Engine) {
		if mcpServer != "" && ex != nil {
			e.executors[mcpServer] = ex
		}
	}
}

// WithAuditEvents active l'enregistrement de l'événement d'audit
// "action_plan.confirmed" (PLAN.md §17, "auditer ... le confirmateur
// humain") à chaque confirmation de plan menée à exécution par confirmPlan.
// Dépendance optionnelle : si jamais fournie (repo nil, ou option omise),
// confirmPlan continue de fonctionner exactement comme avant cette phase,
// simplement sans écrire d'événement d'audit.
func WithAuditEvents(repo *persistence.AuditEventRepository) Option {
	return func(e *Engine) {
		if repo != nil {
			e.auditEvents = repo
		}
	}
}

// WithLogger fournit le logger utilisé par Engine.RecoverInterrupted
// (PLAN.md Phase 18) pour journaliser le nombre de plans et d'actions
// récupérés au démarrage. slog.Default() est utilisé si jamais fournie ou
// si logger est nil.
func WithLogger(logger *slog.Logger) Option {
	return func(e *Engine) {
		if logger != nil {
			e.logger = logger
		}
	}
}

// WithMetrics active l'observation du nombre de plans d'actions proposés
// (CreatePlan) et confirmés (confirmPlan) par l'Engine (PLAN.md §14.3,
// Phase 20). metrics nil (option omise) désactive l'observation.
func WithMetrics(metrics *observability.Metrics) Option {
	return func(e *Engine) {
		e.metrics = metrics
	}
}

// NewEngine construit un Engine. mcpManager peut être nil si aucune action
// exécutée par cet Engine ne passe par un serveur MCP (auquel cas seuls les
// exécuteurs explicitement enregistrés via WithMemoryStore/WithExecutor
// sont disponibles).
func NewEngine(db *persistence.DB, authorizer *authorization.Authorizer, mcpManager *mcp.Manager, cfg *config.Config, opts ...Option) *Engine {
	e := &Engine{
		db:         db,
		plans:      persistence.NewActionPlanRepository(),
		actions:    persistence.NewActionRepository(),
		authorizer: authorizer,
		mcpManager: mcpManager,
		cfg:        cfg,
		now:        time.Now,
		planTTL:    defaultPlanTTL,
		executors:  make(map[string]Executor),
		logger:     slog.Default(),
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// CreatePlan persiste un nouveau plan "awaiting_confirmation" à partir des
// actions proposées durant un tour de conversation (PLAN.md §10.2, §10.3).
// Retourne le plan et un texte prêt à être renvoyé à l'utilisateur, listant
// numériquement les actions proposées (PLAN.md §8.5, généralisé ici).
func (e *Engine) CreatePlan(ctx context.Context, identity model.ExecutionIdentity, proposed []delegation.ProposedAction) (persistence.ActionPlan, string, error) {
	if len(proposed) == 0 {
		return persistence.ActionPlan{}, "", fmt.Errorf("action: aucune action proposée à planifier")
	}

	now := e.now().UTC()
	nowText := now.Format(time.RFC3339)
	expiresAt := now.Add(e.planTTL).Format(time.RFC3339)

	plan := persistence.ActionPlan{
		ID:             persistence.ActionPlanID(uuid.NewString()),
		OrgID:          identity.OrgID,
		ConversationID: identity.ConversationID,
		CreatedBy:      identity.PrincipalID,
		Scope:          proposed[0].Scope,
		ScopeID:        proposed[0].ScopeID,
		Status:         StatusAwaitingConfirmation,
		ExpiresAt:      &expiresAt,
		CreatedAt:      nowText,
		UpdatedAt:      nowText,
	}

	rows := make([]persistence.Action, 0, len(proposed))
	for i, pa := range proposed {
		argsJSON, err := marshalArguments(pa.Arguments)
		if err != nil {
			return persistence.ActionPlan{}, "", fmt.Errorf("action: sérialisation des arguments de l'action %q: %w", pa.Summary, err)
		}

		rows = append(rows, persistence.Action{
			ID:                   persistence.ActionID(uuid.NewString()),
			PlanID:               plan.ID,
			Position:             i,
			AgentID:              pa.AgentID,
			MCPServer:            pa.MCPServer,
			ToolName:             pa.ToolName,
			ArgumentsJSON:        argsJSON,
			Summary:              pa.Summary,
			RequiredPermission:   pa.RequiredPermission,
			RequiresConfirmation: true,
			Status:               "proposed",
			CreatedAt:            nowText,
		})
	}

	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := e.plans.Insert(ctx, tx, plan); err != nil {
			return err
		}
		for _, a := range rows {
			if err := e.actions.Insert(ctx, tx, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return persistence.ActionPlan{}, "", fmt.Errorf("action: persistance du plan d'actions: %w", err)
	}

	e.metrics.IncActionProposed()

	return plan, formatPlanProposal(rows, e.planTTL), nil
}

// RecoverInterrupted recherche, au démarrage du processus, les plans
// d'actions restés bloqués en statut "executing" (PLAN.md §18, "reprendre
// les plans interrompus", "identifier les états ambigus"). Un tel plan ne
// peut résulter que d'un crash du processus AU MILIEU de confirmPlan, entre
// la transition initiale vers "executing" (ligne 323 environ) et la
// persistance de son statut final (succeeded/partially_succeeded/failed) :
// en usage normal, confirmPlan ne retourne jamais avant d'avoir écrit ce
// statut final.
//
// Pour chaque action de ces plans :
//
//   - "executing" SANS completed_at : l'appel externe (MCP ou interne) a
//     peut-être eu lieu — le crash a pu survenir n'importe où entre l'appel
//     de Executor.Execute et l'enregistrement de son résultat
//     (executeAction, points 8-9). Son issue réelle est donc INCONNUE : PAS
//     DE REJEU. Elle est marquée "failed", error_code
//     "interrupted_unknown_outcome".
//
//   - "proposed" (jamais commencée) dans un plan "executing" : cette action
//     n'a produit aucun effet externe (le crash a eu lieu avant même le
//     premier appel d'executeAction pour elle), mais elle N'EST PAS reprise
//     automatiquement pour autant. Choix délibéré, plus prudent qu'une
//     reprise automatique : PLAN.md §10.5 exige de recharger le plan,
//     revérifier son état, son expiration, l'identité du confirmateur, les
//     permissions et les ressources "au moment de l'exécution", juste avant
//     chaque action — c'est-à-dire re-vérifier un contexte de confiance
//     fourni par un humain (ou une identité de service) qui, ici, n'est
//     justement plus présent pour re-confirmer quoi que ce soit après un
//     redémarrage. Rejouer une écriture externe sans cette confirmation
//     fraîche romprait la garantie centrale de la Phase 15
//     ("ne jamais faire confiance uniquement à l'autorisation obtenue lors
//     de la proposition"). Elle est donc marquée "failed", error_code
//     "interrupted_not_started" ; un humain qui veut toujours cette action
//     doit la reformuler pour obtenir une nouvelle proposition et la
//     confirmer explicitement.
//
//   - toute action déjà "succeeded" ou "failed" est laissée inchangée.
//
// Le plan passe ensuite à "partially_succeeded" (si au moins une de ses
// actions avait déjà "succeeded" avant le crash) ou "failed" (sinon) :
// jamais laissé bloqué en "executing".
//
// Idempotente et sûre à rappeler même s'il n'y a rien à récupérer (aucun
// plan "executing" ne signifie aucune opération). Doit être appelée UNE
// FOIS au démarrage du processus, avant de traiter le moindre message ou
// tick de scheduler (voir internal/registry.Run).
func (e *Engine) RecoverInterrupted(ctx context.Context) error {
	var (
		recoveredPlans   int
		recoveredActions int
	)

	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		plans, err := e.plans.ListByStatus(ctx, tx, StatusExecuting)
		if err != nil {
			return fmt.Errorf("recherche des plans interrompus: %w", err)
		}

		now := e.now().UTC().Format(time.RFC3339)

		for _, plan := range plans {
			actions, err := e.actions.ListByPlanID(ctx, tx, plan.ID)
			if err != nil {
				return fmt.Errorf("liste des actions du plan interrompu %q: %w", plan.ID, err)
			}

			succeeded := 0
			for _, act := range actions {
				switch {
				case act.Status == StatusSucceeded:
					succeeded++
				case act.Status == "proposed":
					code := "interrupted_not_started"
					if err := e.actions.UpdateStatus(ctx, tx, act.ID, StatusFailed, nil, &now, &code); err != nil {
						return fmt.Errorf("récupération de l'action non commencée %q: %w", act.ID, err)
					}
					recoveredActions++
				case act.Status == StatusExecuting && act.CompletedAt == nil:
					code := "interrupted_unknown_outcome"
					if err := e.actions.UpdateStatus(ctx, tx, act.ID, StatusFailed, nil, &now, &code); err != nil {
						return fmt.Errorf("récupération de l'action interrompue %q: %w", act.ID, err)
					}
					recoveredActions++
				}
			}

			finalStatus := StatusFailed
			if succeeded > 0 {
				finalStatus = StatusPartiallySucceeded
			}

			if err := e.plans.UpdateStatus(ctx, tx, plan.ID, finalStatus, now); err != nil {
				return fmt.Errorf("mise à jour du statut du plan interrompu %q: %w", plan.ID, err)
			}
			recoveredPlans++
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("action: récupération des plans interrompus: %w", err)
	}

	if e.logger != nil {
		e.logger.InfoContext(ctx, "action: récupération des plans interrompus terminée",
			"plans_recovered", recoveredPlans, "actions_recovered", recoveredActions)
	}

	return nil
}

// HandleCommand traite une commande "confirmer"/"annuler" reçue dans la
// conversation conv (PLAN.md §10.4). Elle ne retrouve que les plans actifs
// (awaiting_confirmation) de CETTE conversation : un plan d'une autre
// conversation n'est jamais visible, ce qui répond à lui seul au cas
// "mauvaise conversation".
func (e *Engine) HandleCommand(ctx context.Context, identity model.ExecutionIdentity, conv model.Conversation, cmd Command) (string, error) {
	var activePlans []persistence.ActionPlan

	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		activePlans, err = e.plans.ListActiveByConversation(ctx, tx, conv.ID)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("action: recherche des plans actifs de la conversation %q: %w", conv.ID, err)
	}

	if len(activePlans) == 0 {
		return "Aucun plan d'actions en attente de confirmation dans cette conversation.", nil
	}

	sort.Slice(activePlans, func(i, j int) bool {
		if activePlans[i].CreatedAt != activePlans[j].CreatedAt {
			return activePlans[i].CreatedAt < activePlans[j].CreatedAt
		}
		return activePlans[i].ID < activePlans[j].ID
	})

	var target persistence.ActionPlan
	switch {
	case cmd.PlanNumber > 0:
		if cmd.PlanNumber > len(activePlans) {
			return fmt.Sprintf("Aucun plan numéro %d en attente de confirmation dans cette conversation.", cmd.PlanNumber), nil
		}
		target = activePlans[cmd.PlanNumber-1]
	case len(activePlans) == 1:
		target = activePlans[0]
	default:
		// PLAN.md §10.4 : "si plusieurs plans sont actifs, demander
		// explicitement lequel confirmer".
		return formatAmbiguousPlans(activePlans), nil
	}

	switch cmd.Kind {
	case CommandConfirm:
		return e.confirmPlan(ctx, identity, target)
	case CommandCancel:
		return e.cancelPlan(ctx, target)
	default:
		return "", fmt.Errorf("action: commande inconnue")
	}
}

// confirmPlan applique le cycle de vérification et d'exécution complet
// décrit par PLAN.md §10.5, dans l'ordre.
func (e *Engine) confirmPlan(ctx context.Context, identity model.ExecutionIdentity, ref persistence.ActionPlan) (string, error) {
	// 1. Recharger le plan depuis la persistance : jamais un état en
	// mémoire potentiellement périmé.
	plan, found, err := e.reloadPlan(ctx, ref.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return "Ce plan d'actions n'existe plus.", nil
	}

	// 2. Vérifier son état : seul un plan awaiting_confirmation peut être
	// confirmé (pas de double exécution).
	if plan.Status != StatusAwaitingConfirmation {
		return fmt.Sprintf("Ce plan d'actions a déjà été traité (statut actuel : %s).", plan.Status), nil
	}

	// 3. Vérifier l'expiration.
	if e.isExpired(plan) {
		if err := e.setPlanStatus(ctx, plan.ID, StatusExpired); err != nil {
			return "", err
		}
		return "Ce plan d'actions a expiré : reformulez votre demande pour la proposer à nouveau.", nil
	}

	// 4. Vérifier l'identité du confirmateur.
	if err := e.authorizeConfirmer(identity, plan); err != nil {
		return "Vous n'êtes pas autorisé à confirmer ce plan d'actions.", nil
	}

	// Verrou d'exécution : la vérification d'état de l'étape 2 a été faite
	// sur une lecture, donc sur un instantané. C'est cette transition gardée
	// par la base qui garantit réellement qu'un plan n'est exécuté qu'une
	// fois — voir ActionPlanRepository.CompareAndSwapStatus.
	swapped, err := e.swapPlanStatus(ctx, plan.ID, StatusAwaitingConfirmation, StatusConfirmed)
	if err != nil {
		return "", err
	}
	if !swapped {
		return "Ce plan d'actions vient d'être traité.", nil
	}

	e.metrics.IncActionConfirmed()

	if err := e.setPlanStatus(ctx, plan.ID, StatusExecuting); err != nil {
		return "", err
	}

	actions, err := e.listActions(ctx, plan.ID)
	if err != nil {
		return "", err
	}

	// 8. Exécuter séquentiellement, en continuant même si une action
	// échoue.
	outcomes := make([]actionOutcome, 0, len(actions))
	for _, act := range actions {
		outcomes = append(outcomes, e.executeAction(ctx, identity, plan, act))
	}

	finalStatus := finalPlanStatus(outcomes)
	if err := e.setPlanStatus(ctx, plan.ID, finalStatus); err != nil {
		return "", err
	}

	e.recordPlanConfirmedAudit(ctx, identity, plan, finalStatus, outcomes)

	return formatExecutionReport(finalStatus, outcomes), nil
}

// recordPlanConfirmedAudit journalise l'événement d'audit
// "action_plan.confirmed" (PLAN.md §17, "auditer ... le confirmateur
// humain") : le principal enregistré est identity.PrincipalID, c'est-à-dire
// le confirmateur ACTUEL, jamais plan.CreatedBy (l'auteur, potentiellement
// technique, de la proposition). No-op silencieux si aucun
// persistence.AuditEventRepository n'a été fourni via WithAuditEvents.
//
// L'écriture est best-effort : elle ne doit jamais faire échouer une
// confirmation par ailleurs réussie, les actions externes ayant déjà eu lieu
// à ce stade. Un échec est en revanche journalisé, faute de quoi la
// disparition d'une trace d'audit — précisément ce qu'on cherche à ne pas
// perdre — serait totalement silencieuse.
func (e *Engine) recordPlanConfirmedAudit(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, finalStatus string, outcomes []actionOutcome) {
	if e.auditEvents == nil {
		return
	}

	succeeded, failed := 0, 0
	for _, o := range outcomes {
		if o.ok {
			succeeded++
		} else {
			failed++
		}
	}

	metadata, err := json.Marshal(map[string]any{
		"plan_id":   string(plan.ID),
		"succeeded": succeeded,
		"failed":    failed,
	})
	if err != nil {
		e.logger.ErrorContext(ctx, "action: échec de la sérialisation des métadonnées d'audit action_plan.confirmed",
			"action_plan_id", plan.ID,
			"error", err,
		)

		return
	}
	metadataJSON := string(metadata)

	convID := plan.ConversationID

	event := persistence.AuditEvent{
		ID:              persistence.AuditEventID(uuid.NewString()),
		OrgID:           plan.OrgID,
		PrincipalID:     identity.PrincipalID,
		Trigger:         model.TriggerMessage,
		ConversationID:  &convID,
		EventType:       "action_plan.confirmed",
		ResourceKind:    "action_plan",
		ResourceScope:   plan.Scope,
		ResourceScopeID: plan.ScopeID,
		Outcome:         finalStatus,
		MetadataJSON:    &metadataJSON,
		CreatedAt:       e.now().UTC().Format(time.RFC3339),
	}

	if err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		return e.auditEvents.Insert(ctx, tx, event)
	}); err != nil {
		e.logger.ErrorContext(ctx, "action: échec de l'enregistrement de l'événement d'audit action_plan.confirmed",
			"action_plan_id", plan.ID,
			"org_id", plan.OrgID,
			"principal_id", identity.PrincipalID,
			"conversation_id", plan.ConversationID,
			"outcome", finalStatus,
			"error", err,
		)
	}
}

// cancelPlan marque ref comme annulé (PLAN.md §10.4). Comme confirmPlan,
// recharge le plan et vérifie son état pour éviter d'annuler un plan déjà
// terminé.
func (e *Engine) cancelPlan(ctx context.Context, ref persistence.ActionPlan) (string, error) {
	plan, found, err := e.reloadPlan(ctx, ref.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return "Ce plan d'actions n'existe plus.", nil
	}

	if plan.Status != StatusAwaitingConfirmation {
		return fmt.Sprintf("Ce plan d'actions a déjà été traité (statut actuel : %s), impossible de l'annuler.", plan.Status), nil
	}

	if err := e.setPlanStatus(ctx, plan.ID, StatusCancelled); err != nil {
		return "", err
	}

	return "Plan d'actions annulé, aucune action n'a été exécutée.", nil
}

// authorizeConfirmer vérifie l'identité du confirmateur (PLAN.md §10.5
// point 4, §10.3 "liée à l'auteur ou au rôle autorisé").
//
// Règle retenue, faute de précision supplémentaire dans PLAN.md : dans une
// conversation privée (portée personnelle), seul le principal auteur de la
// proposition peut la confirmer, puisque la ressource concernée lui est
// propre. Dans une conversation de groupe (portée group), n'importe quel
// principal de CETTE conversation peut confirmer, pas nécessairement le
// créateur : une conversation de groupe est par nature partagée entre ses
// membres, et HandleCommand a déjà restreint la recherche du plan à la
// conversation courante (identity.ConversationID == plan.ConversationID),
// ce qui suffit à garantir que le confirmateur est bien un participant de
// cette conversation.
func (e *Engine) authorizeConfirmer(identity model.ExecutionIdentity, plan persistence.ActionPlan) error {
	if identity.ConversationID != plan.ConversationID {
		return fmt.Errorf("action: confirmateur en dehors de la conversation du plan")
	}

	if plan.Scope == model.ScopePersonal && identity.PrincipalID != plan.CreatedBy {
		return fmt.Errorf("action: seul l'auteur de la proposition peut la confirmer dans une conversation privée")
	}

	return nil
}

// isExpired vérifie l'expiration du plan par rapport à l'horloge de
// l'Engine (PLAN.md §10.5 point 3). Un plan sans ExpiresAt n'expire jamais.
func (e *Engine) isExpired(plan persistence.ActionPlan) bool {
	if plan.ExpiresAt == nil || *plan.ExpiresAt == "" {
		return false
	}

	expiresAt, err := time.Parse(time.RFC3339, *plan.ExpiresAt)
	if err != nil {
		return false
	}

	return !e.now().Before(expiresAt)
}

// actionOutcome résume le résultat d'exécution d'une action pour le
// rapport final (PLAN.md §10.5 point 9).
type actionOutcome struct {
	action  persistence.Action
	ok      bool
	message string
}

// executeAction applique PLAN.md §10.5 points 5 à 9 pour une action donnée.
// Une erreur n'est jamais propagée à l'appelant : un échec (permission
// retirée, ressource introuvable, outil en erreur) est enregistré sur
// l'action elle-même (status=failed) et ne bloque jamais les actions
// suivantes du plan.
func (e *Engine) executeAction(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action) actionOutcome {
	startedAt := e.now().UTC().Format(time.RFC3339)
	if err := e.setActionStatus(ctx, act.ID, StatusExecuting, &startedAt, nil, nil); err != nil {
		return actionOutcome{action: act, ok: false, message: fmt.Sprintf("erreur interne: %v", err)}
	}

	// 5. Recalculer les permissions : jamais se fier à l'autorisation
	// obtenue lors de la proposition.
	targetScope, err := scopeFromPermission(act.RequiredPermission)
	if err != nil {
		return e.failAction(ctx, act, "invalid_permission", err)
	}

	if err := e.authorizer.Authorize(ctx, authorization.AuthorizationRequest{
		Identity:      identity,
		Permission:    act.RequiredPermission,
		TargetOrgID:   plan.OrgID,
		TargetScope:   targetScope,
		TargetScopeID: plan.ScopeID,
	}); err != nil {
		return e.failAction(ctx, act, "permission_denied", err)
	}

	var args map[string]any
	if strings.TrimSpace(act.ArgumentsJSON) != "" {
		if err := json.Unmarshal([]byte(act.ArgumentsJSON), &args); err != nil {
			return e.failAction(ctx, act, "invalid_arguments", err)
		}
	}

	// 6. Résoudre à nouveau les ressources externes. L'identifiant n'est
	// jamais lu depuis l'action persistée : le spécialiste le retire au
	// moment de la proposition (voir internal/agent, wrapWriteTool) et il est
	// réinjecté ici depuis la portée du plan, d'après ce que déclare
	// mcp_servers.<nom>.resource. Une action confirmée écrit donc toujours
	// dans la ressource courante de sa portée, jamais dans celle qu'un modèle
	// aurait pu suggérer ni dans celle qu'une configuration antérieure
	// désignait.
	args, err = resource.InjectResolved(e.cfg, act.MCPServer, plan.Scope, plan.ScopeID, args)
	if err != nil {
		return e.failAction(ctx, act, "resource_not_configured", err)
	}

	executor, ok := e.executorFor(act.MCPServer)
	if !ok {
		return e.failAction(ctx, act, "no_executor", fmt.Errorf("aucun exécuteur disponible pour le serveur %q", act.MCPServer))
	}

	// 7-8. Exécuter réellement l'action.
	resultText, err := executor.Execute(ctx, identity, plan, act, args)
	if err != nil {
		return e.failAction(ctx, act, "execution_failed", err)
	}

	// 9. Enregistrer le résultat.
	completedAt := e.now().UTC().Format(time.RFC3339)
	if err := e.setActionStatus(ctx, act.ID, StatusSucceeded, nil, &completedAt, nil); err != nil {
		return actionOutcome{action: act, ok: false, message: fmt.Sprintf("erreur interne: %v", err)}
	}

	return actionOutcome{action: act, ok: true, message: resultText}
}

// failAction enregistre l'échec d'une action et retourne l'outcome
// correspondant.
//
// L'échec est aussi journalisé : c'est le seul endroit où une écriture
// externe confirmée peut échouer silencieusement du point de vue de
// l'exploitant, le rapport détaillé ne partant qu'à l'utilisateur de la
// conversation. Seuls des identifiants et un code d'erreur court sont émis —
// ni arguments d'outil, ni résumé d'action, qui portent des données privées
// (PLAN.md §14.2).
func (e *Engine) failAction(ctx context.Context, act persistence.Action, code string, cause error) actionOutcome {
	completedAt := e.now().UTC().Format(time.RFC3339)
	errCode := code
	_ = e.setActionStatus(ctx, act.ID, StatusFailed, nil, &completedAt, &errCode)

	e.logger.ErrorContext(ctx, "action: échec de l'exécution d'une action confirmée",
		"action_plan_id", act.PlanID,
		"action_id", act.ID,
		"agent_id", act.AgentID,
		"mcp_server", act.MCPServer,
		"tool_name", act.ToolName,
		"error_code", code,
		"error", cause,
	)

	return actionOutcome{action: act, ok: false, message: cause.Error()}
}

// executorFor retourne l'exécuteur applicable pour mcpServer : un exécuteur
// explicitement enregistré (WithMemoryStore/WithExecutor), sinon un
// exécuteur MCP générique si un Manager est disponible.
func (e *Engine) executorFor(mcpServer string) (Executor, bool) {
	if ex, ok := e.executors[mcpServer]; ok {
		return ex, true
	}
	if e.mcpManager == nil {
		return nil, false
	}
	return &mcpExecutor{manager: e.mcpManager}, true
}

// reloadPlan recharge un plan depuis la persistance.
func (e *Engine) reloadPlan(ctx context.Context, id persistence.ActionPlanID) (persistence.ActionPlan, bool, error) {
	var plan persistence.ActionPlan
	var found bool

	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		plan, found, err = e.plans.FindByID(ctx, tx, id)
		return err
	})
	if err != nil {
		return persistence.ActionPlan{}, false, fmt.Errorf("action: rechargement du plan %q: %w", id, err)
	}

	return plan, found, nil
}

// listActions recharge les actions d'un plan, triées par position.
func (e *Engine) listActions(ctx context.Context, planID persistence.ActionPlanID) ([]persistence.Action, error) {
	var actions []persistence.Action

	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		actions, err = e.actions.ListByPlanID(ctx, tx, planID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("action: liste des actions du plan %q: %w", planID, err)
	}

	return actions, nil
}

// setPlanStatus persiste le nouveau statut du plan (PLAN.md §10.5 point 10,
// "persister tout au fur et à mesure").
func (e *Engine) setPlanStatus(ctx context.Context, id persistence.ActionPlanID, status string) error {
	now := e.now().UTC().Format(time.RFC3339)
	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		return e.plans.UpdateStatus(ctx, tx, id, status, now)
	})
	if err != nil {
		return fmt.Errorf("action: mise à jour du statut du plan %q vers %q: %w", id, status, err)
	}
	return nil
}

// swapPlanStatus applique une transition de statut gardée : elle n'a lieu que
// si le plan est encore dans fromStatus, et retourne false sinon.
func (e *Engine) swapPlanStatus(ctx context.Context, id persistence.ActionPlanID, fromStatus, toStatus string) (bool, error) {
	now := e.now().UTC().Format(time.RFC3339)

	var swapped bool

	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		swapped, err = e.plans.CompareAndSwapStatus(ctx, tx, id, fromStatus, toStatus, now)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("action: transition du plan %q de %q vers %q: %w", id, fromStatus, toStatus, err)
	}

	return swapped, nil
}

// setActionStatus persiste le nouveau statut d'une action.
func (e *Engine) setActionStatus(ctx context.Context, id persistence.ActionID, status string, startedAt, completedAt, errorCode *string) error {
	err := e.db.WithTx(ctx, func(tx *sql.Tx) error {
		return e.actions.UpdateStatus(ctx, tx, id, status, startedAt, completedAt, errorCode)
	})
	if err != nil {
		return fmt.Errorf("action: mise à jour du statut de l'action %q vers %q: %w", id, status, err)
	}
	return nil
}

// marshalArguments sérialise args en JSON, ou retourne "{}" si args est
// vide (Action.ArgumentsJSON est NOT NULL).
func marshalArguments(args map[string]any) (string, error) {
	if len(args) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// scopeFromPermission extrait le segment de portée d'une permission
// "<domaine>.<scope>.<action>" (même convention que
// internal/authorization.permissionScopeAction, dupliquée ici car non
// exportée par ce package).
func scopeFromPermission(permission string) (model.Scope, error) {
	parts := strings.Split(permission, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("action: format de permission invalide %q", permission)
	}
	return model.Scope(parts[1]), nil
}
