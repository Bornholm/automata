// Package scheduler déclenche les agents configurés selon les expressions
// cron déclarées dans cfg.Schedules (PLAN.md §11, Phase 16 et §17).
//
// Politique "read_only" (Phase 16) : toute action proposée par l'agent
// durant une exécution planifiée est journalisée puis ignorée, jamais
// transformée en plan de confirmation.
//
// Politique "require_confirmation" (Phase 17) : toute action proposée par
// l'agent est transformée en plan d'actions persisté (internal/action),
// livré au canal configuré, confirmable par un humain compétent de ce canal
// exactement comme une proposition née d'une conversation (même moteur, même
// revérification de permission par action au moment de la confirmation).
//
// Chaque occurrence est enregistrée avant son exécution (déduplication
// native via UNIQUE(schedule_id, scheduled_for) en base, PLAN.md §11.5), et
// la livraison du résultat via Courier est une étape strictement séparée de
// l'exécution : une erreur de livraison ne réexécute jamais l'agent (PLAN.md
// §11.6).
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/bits"
	"time"

	"github.com/google/uuid"
	cron "github.com/robfig/cron/v3"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// Statuts persistés dans scheduled_runs.status.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Statuts persistés dans delivery_attempts.status et
// scheduled_runs.delivery_status.
const (
	DeliveryStatusSucceeded = "succeeded"
	DeliveryStatusFailed    = "failed"
)

// Codes d'erreur courts persistés dans scheduled_runs.error_code /
// delivery_attempts.error_code (même convention que internal/action.Engine).
const (
	errCodeAgentNotFound      = "agent_not_found"
	errCodeExecutionError     = "execution_error"
	errCodeUnsupportedPolicy  = "unsupported_actions_policy"
	errCodeProviderNotFound   = "provider_not_found"
	errCodeSendFailed         = "send_failed"
	errCodePlanCreationError  = "action_plan_creation_error"
	errCodeStaleLockRecovered = "stale_lock_recovered"
)

// Types d'événements d'audit émis par le scheduler (PLAN.md §17, "auditer
// l'auteur technique et le confirmateur humain"). Le second événement
// ("action_plan.confirmed") est émis par internal/action.Engine lui-même au
// moment de la confirmation, pas ici.
const auditEventPlanProposed = "action_plan.proposed"

// Clock abstrait l'horloge système, pour des tests déterministes.
type Clock interface {
	Now() time.Time
}

// RealClock est l'implémentation de production de Clock.
type RealClock struct{}

// Now implémente Clock.
func (RealClock) Now() time.Time { return time.Now() }

var _ Clock = RealClock{}

// ValidateSchedules vérifie, au démarrage, que chaque déclenchement
// planifié activé déclare une politique d'actions supportée : "read_only"
// (Phase 16) ou "require_confirmation" (Phase 17, PLAN.md §11.3). Toute
// autre valeur reste une erreur de configuration claire.
func ValidateSchedules(cfg *config.Config) error {
	for _, sched := range cfg.Schedules {
		if !sched.Enabled {
			continue
		}

		switch sched.Execution.Actions.Policy {
		case config.ActionsPolicyReadOnly, config.ActionsPolicyRequireConfirmation:
		default:
			return fmt.Errorf("scheduler: schedule %q: politique d'actions %q non supportée (attendu %q ou %q)",
				sched.ID, sched.Execution.Actions.Policy, config.ActionsPolicyReadOnly, config.ActionsPolicyRequireConfirmation)
		}
	}

	return nil
}

// Scheduler déclenche les agents configurés selon leurs expressions cron et
// livre le résultat via Courier.
type Scheduler struct {
	cfg          *config.Config
	clock        Clock
	db           *persistence.DB
	agents       *agent.Registry
	senders      map[string]courier.Provider
	actionEngine *action.Engine

	scheduledRuns    *persistence.ScheduledRunRepository
	deliveryAttempts *persistence.DeliveryAttemptRepository
	conversations    *persistence.ConversationRepository
	auditEvents      *persistence.AuditEventRepository

	logger  *slog.Logger
	metrics *observability.Metrics
}

// NewScheduler construit un Scheduler. senders doit contenir un
// courier.Provider par nom de fournisseur déclaré dans cfg.Courier.Providers
// (la même map que celle utilisée pour l'ingress, voir internal/registry).
// actionEngine est utilisé pour transformer les actions proposées par un
// agent exécuté sous la politique "require_confirmation" en plan de
// confirmation (PLAN.md §17) ; il peut être nil si aucun schedule activé ne
// déclare cette politique (ValidateSchedules ne l'impose pas explicitement,
// mais une exécution require_confirmation avec actionEngine nil échoue
// proprement, voir executeAndDeliver).
func NewScheduler(cfg *config.Config, clock Clock, db *persistence.DB, agents *agent.Registry, senders map[string]courier.Provider, actionEngine *action.Engine, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}

	return &Scheduler{
		cfg:              cfg,
		clock:            clock,
		db:               db,
		agents:           agents,
		senders:          senders,
		actionEngine:     actionEngine,
		scheduledRuns:    persistence.NewScheduledRunRepository(),
		deliveryAttempts: persistence.NewDeliveryAttemptRepository(),
		conversations:    persistence.NewConversationRepository(),
		auditEvents:      persistence.NewAuditEventRepository(),
		logger:           logger,
	}
}

// WithMetrics attache metrics à s : chaque occurrence planifiée déclenchée
// et chaque erreur de livraison sont comptabilisées dès le prochain Tick
// (PLAN.md §14.3, Phase 20). metrics nil désactive l'observation
// (comportement par défaut de NewScheduler). Retourne s pour permettre le
// chaînage à la construction.
func (s *Scheduler) WithMetrics(metrics *observability.Metrics) *Scheduler {
	s.metrics = metrics
	return s
}

// Run boucle indéfiniment en appelant Tick(ctx, clock.Now()) toutes les
// minutes, jusqu'à annulation de ctx. Utilisé en production ; les tests
// appellent Tick directement avec une horloge fake pour rester
// déterministes.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Tick(ctx, s.clock.Now()); err != nil && ctx.Err() == nil {
				s.logger.ErrorContext(ctx, "scheduler: échec du tick", "error", err)
			}
		}
	}
}

// Tick déclenche toutes les occurrences dues au plus tard à l'instant at,
// pour tous les schedules activés. Une erreur affectant un schedule est
// journalisée et n'interrompt jamais le traitement des autres.
func (s *Scheduler) Tick(ctx context.Context, at time.Time) error {
	for _, sched := range s.cfg.Schedules {
		if !sched.Enabled {
			continue
		}

		if err := s.tickSchedule(ctx, sched, at); err != nil {
			s.logger.ErrorContext(ctx, "scheduler: échec du traitement du schedule", "schedule_id", sched.ID, "error", err)
		}
	}

	return nil
}

// tickSchedule calcule et déclenche toutes les occurrences dues de sched
// jusqu'à at inclus, en rattrapant d'éventuelles occurrences manquées
// depuis la dernière connue en base.
func (s *Scheduler) tickSchedule(ctx context.Context, sched config.Schedule, at time.Time) error {
	loc, err := time.LoadLocation(sched.Schedule.Timezone)
	if err != nil {
		return fmt.Errorf("fuseau horaire %q invalide: %w", sched.Schedule.Timezone, err)
	}

	cronSchedule, err := cron.ParseStandard(sched.Schedule.Cron)
	if err != nil {
		return fmt.Errorf("expression cron %q invalide: %w", sched.Schedule.Cron, err)
	}

	anchor, anchorFromRun, err := s.anchorTime(ctx, sched, at)
	if err != nil {
		return fmt.Errorf("calcul de l'ancre d'occurrence: %w", err)
	}

	// Voir hasFixedHour : seules les expressions ancrées sur une heure du
	// jour unique doivent être protégées contre l'heure murale répétée.
	guardRepeatedHour := hasFixedHour(cronSchedule)

	for {
		next := cronSchedule.Next(anchor.In(loc))

		// Retour à l'heure d'hiver : l'heure murale visée par l'expression
		// cron est vécue deux fois (ex. 02:30 le 27/10/2024 en Europe/Paris,
		// à 00:30Z puis 01:30Z). cron.Next produit bien les deux instants et,
		// scheduled_for étant stocké en UTC, la contrainte
		// UNIQUE(schedule_id, scheduled_for) ne les confond pas : sans cette
		// garde, un "0 7 * * *" serait exécuté deux fois cette nuit-là, ce
		// qu'interdisent PLAN.md §2.3 (règle 10) et §11.7.
		//
		// La comparaison ne vaut que contre une ancre issue d'une occurrence
		// réellement enregistrée : l'ancre artificielle du premier Tick
		// porte une heure murale arbitraire, avec laquelle une collision
		// ferait sauter une occurrence légitime.
		if guardRepeatedHour && anchorFromRun && sameWallClock(anchor, next, loc) {
			s.logger.InfoContext(ctx, "scheduler: occurrence ignorée (heure murale répétée par le retour à l'heure d'hiver)",
				"schedule_id", sched.ID,
				"timezone", sched.Schedule.Timezone,
				"scheduled_for", next.UTC().Format(time.RFC3339),
			)

			anchor = next

			continue
		}

		if next.After(at) {
			return nil
		}

		if !s.triggerOccurrence(ctx, sched, next) {
			// Occurrence bloquée (concurrence "forbid") ou erreur
			// d'enregistrement : l'ancre n'avance pas, pour que le prochain
			// Tick retente cette même occurrence. Toute occurrence
			// ultérieure de CE schedule serait bloquée pour la même
			// raison, donc inutile de continuer à rattraper ce tick-ci.
			return nil
		}

		anchor = next
		anchorFromRun = true
	}
}

// hasFixedHour indique si schedule désigne une heure du jour unique (ex.
// "30 2 * * *"), par opposition à une expression qui balaie plusieurs heures
// ("0 * * * *", "*/30 * * * *", "@every 1h").
//
// Cette distinction décide du traitement de l'heure murale répétée par le
// retour à l'heure d'hiver, et reprend la convention de cron(8) et de
// systemd :
//
//   - heure unique : l'expression exprime "une fois par jour, à telle heure".
//     Ses deux passages sont le même rendez-vous quotidien vécu deux fois, et
//     le déclencher deux fois trahirait l'intention (un résumé du matin
//     envoyé en double). Le second passage est ignoré.
//   - heures multiples : l'expression exprime une cadence. Ses occurrences
//     suivent l'écoulement réel du temps, la journée en compte légitimement
//     une de plus, et toutes doivent être déclenchées.
//
// Le cas intermédiaire d'une liste d'heures fixes ("30 2,14 * * *") est
// traité comme une cadence : rare, et le sur-déclenchement y est moins
// dommageable que l'escamotage d'une occurrence.
//
// Une expression que cron ne représente pas par un *cron.SpecSchedule
// (typiquement "@every") est par nature une cadence : non protégée.
func hasFixedHour(schedule cron.Schedule) bool {
	spec, ok := schedule.(*cron.SpecSchedule)
	if !ok {
		return false
	}

	// Les 24 bits de poids faible portent les heures 0 à 23 ; les bits
	// supérieurs (dont le marqueur "*" interne à cron) sont hors sujet.
	const hoursMask = 1<<24 - 1

	return bits.OnesCount64(spec.Hour&hoursMask) == 1
}

// sameWallClock indique si a et b désignent la même heure murale (date et
// heure locales identiques) dans loc tout en étant deux instants distincts.
// C'est exactement la signature d'une heure répétée par un retour à l'heure
// d'hiver.
func sameWallClock(a, b time.Time, loc *time.Location) bool {
	const wallClockLayout = "2006-01-02T15:04:05"

	if a.Equal(b) {
		return false
	}

	return a.In(loc).Format(wallClockLayout) == b.In(loc).Format(wallClockLayout)
}

// anchorTime retourne l'instant à partir duquel chercher la prochaine
// occurrence de sched : la dernière occurrence connue en base, ou, si
// aucune n'existe encore, un instant juste avant at. Ce second cas évite de
// rattraper un historique arbitrairement long pour un schedule qui n'a
// jamais tourné (rien avant le premier Tick ne peut être considéré comme
// "manqué").
//
// Le booléen retourné indique laquelle des deux ancres a été produite : il
// vaut true lorsqu'elle provient d'une occurrence réellement enregistrée, et
// false pour l'ancre artificielle du premier Tick. Seule la première peut
// servir de référence pour détecter une heure murale répétée (voir
// tickSchedule) ; comparer une heure murale arbitraire ferait sauter une
// occurrence légitime.
func (s *Scheduler) anchorTime(ctx context.Context, sched config.Schedule, at time.Time) (time.Time, bool, error) {
	var (
		latest persistence.ScheduledRun
		found  bool
	)

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		latest, found, err = s.scheduledRuns.FindLatestByScheduleID(ctx, tx, sched.ID)
		return err
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("lecture de la dernière occurrence de %q: %w", sched.ID, err)
	}

	if !found {
		return at.Add(-time.Minute), false, nil
	}

	parsed, err := time.Parse(time.RFC3339, latest.ScheduledFor)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("horodatage d'occurrence invalide %q pour %q: %w", latest.ScheduledFor, sched.ID, err)
	}

	return parsed, true, nil
}

// triggerOccurrence traite une unique occurrence due de sched, à l'instant
// occurrence. Elle retourne false lorsque l'occurrence n'a pas pu être
// traitée (concurrence "forbid" ou erreur d'enregistrement) : dans ce cas
// l'appelant ne doit pas avancer son ancre de rattrapage (voir
// tickSchedule). Toute erreur est journalisée ; elle n'interrompt jamais le
// traitement des autres schedules.
func (s *Scheduler) triggerOccurrence(ctx context.Context, sched config.Schedule, occurrence time.Time) bool {
	logCtx := []any{
		"trigger", model.TriggerCron,
		"schedule_id", sched.ID,
		"scheduled_for", occurrence.UTC().Format(time.RFC3339),
		"org_id", sched.Execution.OrgID,
		"principal_id", sched.Execution.PrincipalID,
		"agent_id", sched.Execution.Agent,
	}

	if sched.Concurrency.Policy != config.ConcurrencyPolicyAllow {
		// Défaut (y compris valeur vide) : forbid, voir PLAN.md §11.4.
		var (
			running      persistence.ScheduledRun
			runningFound bool
		)
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			running, runningFound, err = s.scheduledRuns.FindRunningByScheduleID(ctx, tx, sched.ID)
			return err
		})
		if err != nil {
			s.logger.ErrorContext(ctx, "scheduler: échec de la vérification de concurrence", append(logCtx, "error", err)...)
			return false
		}

		if runningFound {
			if !s.isStaleLock(sched, running) {
				s.logger.InfoContext(ctx, "scheduler: occurrence ignorée (exécution déjà en cours, politique forbid)", logCtx...)
				return false
			}

			// Verrou périmé (PLAN.md §18, "verrou périmé") : l'exécution
			// "running" bloquée depuis plus longtemps que
			// sched.Concurrency.Timeout ne peut être due qu'à un crash du
			// PROCESSUS entier (voir isStaleLock) — elle est récupérée et
			// cette occurrence se déclenche normalement.
			s.recoverStaleLock(ctx, sched, running, logCtx)
		}
	}

	runID, inserted, err := s.recordOccurrence(ctx, sched, occurrence)
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement de l'occurrence", append(logCtx, "error", err)...)
		return false
	}

	if !inserted {
		// Occurrence déjà enregistrée (déduplication, PLAN.md §11.5) : no-op
		// silencieux, mais l'ancre doit tout de même avancer.
		return true
	}

	s.metrics.IncCronOccurrence(sched.ID)

	s.executeAndDeliver(ctx, sched, runID)

	return true
}

// isStaleLock détecte un verrou de concurrence "forbid" périmé (PLAN.md §18,
// "verrou périmé") : une exécution planifiée "running" dont la durée
// écoulée depuis started_at (jusqu'à s.clock.Now(), pas jusqu'à l'occurrence
// candidate, qui peut être un rattrapage dans le passé) dépasse déjà
// sched.Concurrency.Timeout — le MÊME délai déjà utilisé comme timeout
// d'exécution de l'agent (context.WithTimeout, voir executeAndDeliver).
//
// Aucune marge de sécurité supplémentaire n'est ajoutée au-delà de ce
// timeout : il borne déjà précisément la durée maximale légitime d'une
// exécution normale, puisque c'est exactement le délai passé à
// context.WithTimeout pour CETTE MÊME exécution. Ce cas ne devrait
// normalement jamais se produire si ce timeout applicatif fonctionne
// correctement : son defer/cancel garantit un appel à succeedRun/failRun
// avant que Tick ne revienne. isStaleLock est un filet de sécurité pour un
// crash du PROCESSUS ENTIER (kill -9, panique non récupérée, coupure
// d'alimentation), où même ce defer n'a pas pu s'exécuter, laissant
// l'exécution bloquée indéfiniment en "running" et donc bloquant, sous
// politique "forbid", toute occurrence future de ce schedule.
func (s *Scheduler) isStaleLock(sched config.Schedule, running persistence.ScheduledRun) bool {
	timeout := sched.Concurrency.Timeout.Duration()
	if timeout <= 0 || running.StartedAt == nil {
		return false
	}

	startedAt, err := time.Parse(time.RFC3339, *running.StartedAt)
	if err != nil {
		return false
	}

	return s.clock.Now().Sub(startedAt) >= timeout
}

// recoverStaleLock marque running (déjà détectée comme périmée par
// isStaleLock) comme "failed", error_code "stale_lock_recovered". Une
// erreur d'écriture est journalisée mais ne bloque pas le déclenchement de
// la nouvelle occurrence par l'appelant : au pire, la prochaine tentative de
// récupération réessaiera au tick suivant.
func (s *Scheduler) recoverStaleLock(ctx context.Context, sched config.Schedule, running persistence.ScheduledRun, logCtx []any) {
	completedAt := s.clock.Now().UTC().Format(time.RFC3339)
	code := errCodeStaleLockRecovered

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.scheduledRuns.UpdateStatus(ctx, tx, running.ID, StatusFailed, &completedAt, &code)
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de la récupération du verrou périmé", append(logCtx, "stale_scheduled_run_id", running.ID, "error", err)...)
		return
	}

	s.logger.WarnContext(ctx, "scheduler: verrou de concurrence périmé récupéré (crash de processus suspecté)",
		append(logCtx, "stale_scheduled_run_id", running.ID, "started_at", derefOr(running.StartedAt, ""))...)
}

// derefOr retourne *p, ou fallback si p est nil.
func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// recordOccurrence enregistre immédiatement, au sein d'une transaction,
// l'occurrence (sched.ID, occurrence) en base avec le statut "running", sauf
// si elle y figure déjà (inserted == false dans ce cas, no-op).
func (s *Scheduler) recordOccurrence(ctx context.Context, sched config.Schedule, occurrence time.Time) (persistence.ScheduledRunID, bool, error) {
	runID := persistence.ScheduledRunID(uuid.NewString())
	scheduledFor := occurrence.UTC().Format(time.RFC3339)
	inserted := false

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, found, err := s.scheduledRuns.FindByScheduleAndScheduledFor(ctx, tx, sched.ID, scheduledFor)
		if err != nil {
			return err
		}

		if found {
			return nil
		}

		now := s.clock.Now().UTC().Format(time.RFC3339)

		if err := s.scheduledRuns.Insert(ctx, tx, persistence.ScheduledRun{
			ID:           runID,
			ScheduleID:   sched.ID,
			ScheduledFor: scheduledFor,
			StartedAt:    &now,
			Status:       StatusRunning,
			PrincipalID:  model.PrincipalID(sched.Execution.PrincipalID),
			OrgID:        model.OrgID(sched.Execution.OrgID),
			Scope:        model.Scope(sched.Execution.Scope),
			ScopeID:      model.ScopeID(sched.Execution.ScopeID),
			AgentID:      sched.Execution.Agent,
			CreatedAt:    now,
		}); err != nil {
			return err
		}

		inserted = true

		return nil
	})
	if err != nil {
		return "", false, err
	}

	return runID, inserted, nil
}

// executeAndDeliver exécute l'agent pour l'exécution planifiée runID, marque
// son résultat, puis décide et effectue la livraison (étape séparée,
// PLAN.md §11.6).
func (s *Scheduler) executeAndDeliver(ctx context.Context, sched config.Schedule, runID persistence.ScheduledRunID) {
	logCtx := []any{
		"trigger", model.TriggerCron,
		"schedule_id", sched.ID,
		"scheduled_run_id", runID,
		"org_id", sched.Execution.OrgID,
		"principal_id", sched.Execution.PrincipalID,
		"agent_id", sched.Execution.Agent,
	}

	switch sched.Execution.Actions.Policy {
	case config.ActionsPolicyReadOnly, config.ActionsPolicyRequireConfirmation:
	default:
		// Défendu dès le démarrage par ValidateSchedules : ce cas ne
		// devrait jamais se produire en usage normal.
		s.failRun(ctx, runID, errCodeUnsupportedPolicy)
		s.logger.ErrorContext(ctx, "scheduler: politique d'actions non supportée", logCtx...)
		return
	}

	identity, conversation := s.buildIdentity(sched, runID)

	a, err := s.agents.Get(sched.Execution.Agent)
	if err != nil {
		s.failRun(ctx, runID, errCodeAgentNotFound)
		s.logger.ErrorContext(ctx, "scheduler: agent introuvable", append(logCtx, "error", err)...)
		s.deliverIfNeeded(ctx, sched, runID, "", true)
		return
	}

	execCtx, cancel := context.WithTimeout(ctx, sched.Concurrency.Timeout.Duration())
	defer cancel()

	result, err := a.Execute(execCtx, agent.Request{
		Identity:     identity,
		Conversation: conversation,
		Input:        sched.Execution.Prompt,
	})
	if err != nil {
		s.failRun(ctx, runID, errCodeExecutionError)
		s.logger.ErrorContext(ctx, "scheduler: échec de l'exécution de l'agent", append(logCtx, "error", err)...)
		s.deliverIfNeeded(ctx, sched, runID, "", true)
		return
	}

	reply := result.Reply

	if len(result.ProposedActions) > 0 {
		switch sched.Execution.Actions.Policy {
		case config.ActionsPolicyRequireConfirmation:
			// PLAN.md §17 : les actions proposées deviennent un plan de
			// confirmation persisté, au nom du principal de service (auteur
			// technique), livré au canal configuré pour qu'un humain
			// compétent de ce canal puisse le confirmer.
			planReply, ok := s.proposeActionPlan(ctx, sched, identity, conversation, result, logCtx)
			if !ok {
				s.failRun(ctx, runID, errCodePlanCreationError)
				s.deliverIfNeeded(ctx, sched, runID, "", true)
				return
			}
			reply = planReply
		default:
			// Politique read_only (PLAN.md §11.3) : toute action proposée
			// est journalisée puis ignorée, jamais exécutée ni transformée
			// en plan de confirmation.
			s.logger.InfoContext(ctx, "scheduler: actions proposées ignorées (politique read_only)", append(logCtx, "count", len(result.ProposedActions))...)
			reply = fmt.Sprintf("%s\n\n(%d action(s) proposée(s) ignorée(s) : lecture seule)", reply, len(result.ProposedActions))
		}
	}

	if err := s.succeedRun(ctx, runID); err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement du succès de l'exécution", append(logCtx, "error", err)...)
	}

	s.deliverIfNeeded(ctx, sched, runID, reply, false)
}

// buildIdentity construit l'identité d'exécution et la conversation d'une
// occurrence planifiée (PLAN.md §9.3, §11.2). Aucun accès personnel
// implicite : Scope/ScopeID proviennent exclusivement de la configuration de
// confiance (sched.Execution), jamais d'un contenu fourni par l'utilisateur
// ou le LLM.
//
// ConversationID est délibérément identique à celui que calculerait
// internal/identity.Resolver.ResolveMessage pour un message entrant RÉEL sur
// le canal de livraison (provider + ":" + channelID) : c'est ce qui permet à
// un humain tapant "confirmer" dans ce canal de retrouver, via
// internal/action.Engine.HandleCommand, le plan créé par une exécution
// planifiée require_confirmation (PLAN.md §17). Une valeur dérivée de
// l'exécution (schedule_id + scheduled_for, comme utilisé PLAN.md §9.3 pour
// l'isolation des sessions MCP) serait ici une ERREUR : elle rendrait le
// plan invisible à toute confirmation humaine, puisque HandleCommand ne
// retrouve que les plans de identity.ConversationID de l'appelant.
func (s *Scheduler) buildIdentity(sched config.Schedule, runID persistence.ScheduledRunID) (model.ExecutionIdentity, model.Conversation) {
	conversationID := model.ConversationID(sched.Delivery.Provider + ":" + sched.Delivery.ChannelID)

	channelKind := model.ChannelPrivate
	for _, ch := range s.cfg.Channels {
		if ch.Provider == sched.Delivery.Provider && ch.ChannelID == sched.Delivery.ChannelID {
			if ch.Kind == config.ChannelKindGroup {
				channelKind = model.ChannelGroup
			}
			break
		}
	}

	identity := model.ExecutionIdentity{
		Trigger:        model.TriggerCron,
		PrincipalID:    model.PrincipalID(sched.Execution.PrincipalID),
		OrgID:          model.OrgID(sched.Execution.OrgID),
		ConversationID: conversationID,
		Provider:       sched.Delivery.Provider,
		ChannelID:      sched.Delivery.ChannelID,
		ChannelKind:    channelKind,
		Scope:          model.Scope(sched.Execution.Scope),
		ScopeID:        model.ScopeID(sched.Execution.ScopeID),
	}

	conversation := model.Conversation{
		ID:        conversationID,
		OrgID:     identity.OrgID,
		Provider:  identity.Provider,
		ChannelID: identity.ChannelID,
		Kind:      identity.ChannelKind,
		Scope:     identity.Scope,
		ScopeID:   identity.ScopeID,
	}

	return identity, conversation
}

// proposeActionPlan transforme les actions proposées par l'agent en plan de
// confirmation persisté (PLAN.md §17). Retourne le texte à livrer et true en
// cas de succès ; false si le plan n'a pas pu être créé (auquel cas
// l'appelant doit traiter l'occurrence comme un échec d'exécution).
func (s *Scheduler) proposeActionPlan(ctx context.Context, sched config.Schedule, identity model.ExecutionIdentity, conversation model.Conversation, result agent.Result, logCtx []any) (string, bool) {
	if s.actionEngine == nil {
		s.logger.ErrorContext(ctx, "scheduler: politique require_confirmation mais aucun moteur d'actions configuré", logCtx...)
		return "", false
	}

	if err := s.ensureConversation(ctx, conversation); err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement de la conversation de livraison", append(logCtx, "error", err)...)
		return "", false
	}

	plan, planText, err := s.actionEngine.CreatePlan(ctx, identity, result.ProposedActions)
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de la création du plan d'actions", append(logCtx, "error", err)...)
		return "", false
	}

	s.logger.InfoContext(ctx, "scheduler: plan d'actions proposé, en attente de confirmation humaine",
		append(logCtx, "action_plan_id", plan.ID, "action_count", len(result.ProposedActions))...)

	s.recordPlanProposedAudit(ctx, identity, plan, len(result.ProposedActions), logCtx)

	reply := result.Reply
	if reply != "" {
		reply = reply + "\n\n" + planText
	} else {
		reply = planText
	}

	return reply, true
}

// ensureConversation insère conv si elle n'existe pas encore, identifiée par
// (provider, external_channel_id) — même logique que
// internal/conversation.Handler.ensureConversation, dupliquée ici car non
// exportée par ce package. Nécessaire pour satisfaire la contrainte de clé
// étrangère action_plans.conversation_id avant tout appel à
// action.Engine.CreatePlan.
func (s *Scheduler) ensureConversation(ctx context.Context, conv model.Conversation) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, found, err := s.conversations.FindByProviderAndExternalChannelID(ctx, tx, conv.Provider, conv.ChannelID)
		if err != nil {
			return err
		}
		if found {
			return nil
		}

		now := s.clock.Now().UTC().Format(time.RFC3339)

		return s.conversations.Insert(ctx, tx, persistence.Conversation{
			ID:                conv.ID,
			OrgID:             conv.OrgID,
			Provider:          conv.Provider,
			ExternalChannelID: conv.ChannelID,
			Kind:              conv.Kind,
			Scope:             conv.Scope,
			ScopeID:           conv.ScopeID,
			CreatedAt:         now,
			UpdatedAt:         now,
		})
	})
}

// recordPlanProposedAudit journalise l'événement d'audit "action_plan.proposed"
// (PLAN.md §17, "auditer l'auteur technique") : le principal auteur est
// celui de l'identité de service ayant créé le plan (identity.PrincipalID),
// jamais un humain. Une erreur d'écriture est journalisée mais n'affecte
// jamais le déroulement normal de l'exécution planifiée : l'audit est un
// enregistrement best-effort, pas une condition de succès.
func (s *Scheduler) recordPlanProposedAudit(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, actionCount int, logCtx []any) {
	metadata, err := json.Marshal(map[string]any{
		"plan_id":       string(plan.ID),
		"actions_count": actionCount,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de la sérialisation des métadonnées d'audit", append(logCtx, "error", err)...)
		return
	}
	metadataJSON := string(metadata)

	convID := plan.ConversationID

	event := persistence.AuditEvent{
		ID:              persistence.AuditEventID(uuid.NewString()),
		OrgID:           plan.OrgID,
		PrincipalID:     identity.PrincipalID,
		Trigger:         model.TriggerCron,
		ConversationID:  &convID,
		EventType:       auditEventPlanProposed,
		ResourceKind:    "action_plan",
		ResourceScope:   plan.Scope,
		ResourceScopeID: plan.ScopeID,
		Outcome:         "proposed",
		MetadataJSON:    &metadataJSON,
		CreatedAt:       s.clock.Now().UTC().Format(time.RFC3339),
	}

	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.auditEvents.Insert(ctx, tx, event)
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement de l'événement d'audit action_plan.proposed", append(logCtx, "error", err)...)
	}
}

// succeedRun marque runID comme terminé avec succès.
func (s *Scheduler) succeedRun(ctx context.Context, runID persistence.ScheduledRunID) error {
	completedAt := s.clock.Now().UTC().Format(time.RFC3339)
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.scheduledRuns.UpdateStatus(ctx, tx, runID, StatusSucceeded, &completedAt, nil)
	})
}

// failRun marque runID comme terminé en échec, avec le code errCode.
func (s *Scheduler) failRun(ctx context.Context, runID persistence.ScheduledRunID, errCode string) {
	completedAt := s.clock.Now().UTC().Format(time.RFC3339)
	code := errCode
	if err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		return s.scheduledRuns.UpdateStatus(ctx, tx, runID, StatusFailed, &completedAt, &code)
	}); err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement de l'échec de l'exécution", "scheduled_run_id", runID, "error", err)
	}
}

// deliverIfNeeded décide, selon sched.Delivery.Mode, si le résultat doit
// être livré, puis effectue la livraison le cas échéant (PLAN.md §11.6 :
// étape strictement séparée de l'exécution, déjà marquée terminée à ce
// stade). reply est le texte de réponse de l'agent (vide si failed==true ou
// si l'agent n'a rien produit) ; failed indique si l'exécution a échoué.
func (s *Scheduler) deliverIfNeeded(ctx context.Context, sched config.Schedule, runID persistence.ScheduledRunID, reply string, failed bool) {
	if !shouldDeliver(sched.Delivery.Mode, reply, failed) {
		return
	}

	s.deliver(ctx, sched, runID, reply)
}

// shouldDeliver applique la politique de livraison (PLAN.md §11.6).
func shouldDeliver(mode config.DeliveryMode, reply string, failed bool) bool {
	switch mode {
	case config.DeliveryModeAlways:
		return true
	case config.DeliveryModeOnFailure:
		return failed
	case config.DeliveryModeOnContent:
		return reply != ""
	default:
		return false
	}
}

// deliver envoie reply au canal de livraison configuré et enregistre la
// tentative correspondante. Une erreur de livraison ne réexécute jamais
// l'agent ni ne recrée l'exécution planifiée : elle échoue uniquement la
// tentative de livraison.
func (s *Scheduler) deliver(ctx context.Context, sched config.Schedule, runID persistence.ScheduledRunID, reply string) {
	provider, ok := s.senders[sched.Delivery.Provider]
	if !ok {
		s.recordDeliveryAttempt(ctx, runID, sched, DeliveryStatusFailed, ptr(errCodeProviderNotFound))
		s.logger.ErrorContext(ctx, "scheduler: fournisseur courier de livraison introuvable", "schedule_id", sched.ID, "provider", sched.Delivery.Provider)
		return
	}

	outgoing := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(sched.Delivery.ChannelID)),
		courier.NewUser(courier.UserID(sched.Execution.PrincipalID), sched.Execution.PrincipalID),
		courier.WithMessageMainPart(reply),
	)

	if err := provider.Send(ctx, outgoing); err != nil {
		s.recordDeliveryAttempt(ctx, runID, sched, DeliveryStatusFailed, ptr(errCodeSendFailed))
		s.logger.ErrorContext(ctx, "scheduler: échec de l'envoi de la livraison", "schedule_id", sched.ID, "provider", sched.Delivery.Provider, "error", err)
		return
	}

	s.recordDeliveryAttempt(ctx, runID, sched, DeliveryStatusSucceeded, nil)
}

// recordDeliveryAttempt persiste la tentative de livraison et met à jour
// scheduled_runs.delivery_status en conséquence.
func (s *Scheduler) recordDeliveryAttempt(ctx context.Context, runID persistence.ScheduledRunID, sched config.Schedule, status string, errorCode *string) {
	if status == DeliveryStatusFailed {
		s.metrics.IncDeliveryError()
	}

	now := s.clock.Now().UTC().Format(time.RFC3339)

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		attemptNumber, err := s.deliveryAttempts.CountByScheduledRunID(ctx, tx, runID)
		if err != nil {
			return err
		}

		attempt := persistence.DeliveryAttempt{
			ID:             persistence.DeliveryAttemptID(uuid.NewString()),
			ScheduledRunID: runID,
			Provider:       sched.Delivery.Provider,
			ChannelID:      sched.Delivery.ChannelID,
			Attempt:        attemptNumber + 1,
			Status:         status,
			ErrorCode:      errorCode,
			CreatedAt:      now,
			CompletedAt:    &now,
		}

		if err := s.deliveryAttempts.Insert(ctx, tx, attempt); err != nil {
			return err
		}

		return s.scheduledRuns.UpdateDeliveryStatus(ctx, tx, runID, status)
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement de la tentative de livraison", "scheduled_run_id", runID, "error", err)
	}
}

// RetryDelivery relit l'exécution planifiée déjà terminée id et retente
// uniquement l'envoi de sa livraison, sans jamais réexécuter l'agent
// (PLAN.md §11.6). id doit correspondre à une exécution déjà marquée
// succeeded ou failed ; sched est le schedule d'origine (pour retrouver le
// fournisseur et le canal de livraison, non persistés sur scheduled_runs).
//
// RetryDelivery ne relit pas la réponse de l'agent (non persistée) : elle
// n'est utile que pour les livraisons dont le contenu ne dépend pas du
// texte de réponse (ex : notification "échec de l'exécution"), ou lorsque
// l'appelant fournit lui-même reply.
func (s *Scheduler) RetryDelivery(ctx context.Context, sched config.Schedule, runID persistence.ScheduledRunID, reply string) error {
	var (
		run   persistence.ScheduledRun
		found bool
	)

	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		run, found, err = s.scheduledRuns.FindByID(ctx, tx, runID)
		return err
	})
	if err != nil {
		return fmt.Errorf("lecture de l'exécution planifiée %q: %w", runID, err)
	}

	if !found {
		return fmt.Errorf("exécution planifiée %q introuvable", runID)
	}

	if run.Status != StatusSucceeded && run.Status != StatusFailed {
		return fmt.Errorf("exécution planifiée %q non terminée (statut %q), nouvelle tentative de livraison refusée", runID, run.Status)
	}

	s.deliver(ctx, sched, runID, reply)

	return nil
}

func ptr(s string) *string { return &s }
