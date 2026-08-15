// Package scheduler déclenche les agents configurés selon les expressions
// cron déclarées dans cfg.Schedules (PLAN.md §11, Phase 16).
//
// V1 est strictement en lecture seule : toute action proposée par l'agent
// durant une exécution planifiée est journalisée puis ignorée, jamais
// transformée en plan de confirmation (internal/action n'est pas invoqué
// ici, voir Phase 17). Chaque occurrence est enregistrée avant son exécution
// (déduplication native via UNIQUE(schedule_id, scheduled_for) en base,
// PLAN.md §11.5), et la livraison du résultat via Courier est une étape
// strictement séparée de l'exécution : une erreur de livraison ne réexécute
// jamais l'agent (PLAN.md §11.6).
package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	cron "github.com/robfig/cron/v3"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
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
	errCodeAgentNotFound     = "agent_not_found"
	errCodeExecutionError    = "execution_error"
	errCodeUnsupportedPolicy = "unsupported_actions_policy"
	errCodeProviderNotFound  = "provider_not_found"
	errCodeSendFailed        = "send_failed"
)

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
// planifié activé déclare une politique d'actions supportée par cette
// phase. V1 est strictement en lecture seule (PLAN.md §11.3) : la politique
// "require_confirmation" n'est pas silencieusement traitée comme
// "read_only", elle est refusée explicitement (la Phase 17 l'ajoutera).
func ValidateSchedules(cfg *config.Config) error {
	for _, sched := range cfg.Schedules {
		if !sched.Enabled {
			continue
		}

		if sched.Execution.Actions.Policy != config.ActionsPolicyReadOnly {
			return fmt.Errorf("scheduler: schedule %q: politique d'actions %q non supportée par cette phase (seul %q est supporté)",
				sched.ID, sched.Execution.Actions.Policy, config.ActionsPolicyReadOnly)
		}
	}

	return nil
}

// Scheduler déclenche les agents configurés selon leurs expressions cron et
// livre le résultat via Courier.
type Scheduler struct {
	cfg     *config.Config
	clock   Clock
	db      *persistence.DB
	agents  *agent.Registry
	senders map[string]courier.Provider

	scheduledRuns    *persistence.ScheduledRunRepository
	deliveryAttempts *persistence.DeliveryAttemptRepository

	logger *slog.Logger
}

// NewScheduler construit un Scheduler. senders doit contenir un
// courier.Provider par nom de fournisseur déclaré dans cfg.Courier.Providers
// (la même map que celle utilisée pour l'ingress, voir internal/registry).
func NewScheduler(cfg *config.Config, clock Clock, db *persistence.DB, agents *agent.Registry, senders map[string]courier.Provider, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}

	return &Scheduler{
		cfg:              cfg,
		clock:            clock,
		db:               db,
		agents:           agents,
		senders:          senders,
		scheduledRuns:    persistence.NewScheduledRunRepository(),
		deliveryAttempts: persistence.NewDeliveryAttemptRepository(),
		logger:           logger,
	}
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

	anchor, err := s.anchorTime(ctx, sched, at)
	if err != nil {
		return fmt.Errorf("calcul de l'ancre d'occurrence: %w", err)
	}

	for {
		next := cronSchedule.Next(anchor.In(loc))
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
	}
}

// anchorTime retourne l'instant à partir duquel chercher la prochaine
// occurrence de sched : la dernière occurrence connue en base, ou, si
// aucune n'existe encore, un instant juste avant at. Ce second cas évite de
// rattraper un historique arbitrairement long pour un schedule qui n'a
// jamais tourné (rien avant le premier Tick ne peut être considéré comme
// "manqué").
func (s *Scheduler) anchorTime(ctx context.Context, sched config.Schedule, at time.Time) (time.Time, error) {
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
		return time.Time{}, fmt.Errorf("lecture de la dernière occurrence de %q: %w", sched.ID, err)
	}

	if !found {
		return at.Add(-time.Minute), nil
	}

	parsed, err := time.Parse(time.RFC3339, latest.ScheduledFor)
	if err != nil {
		return time.Time{}, fmt.Errorf("horodatage d'occurrence invalide %q pour %q: %w", latest.ScheduledFor, sched.ID, err)
	}

	return parsed, nil
}

// triggerOccurrence traite une unique occurrence due de sched, à l'instant
// occurrence. Elle retourne false lorsque l'occurrence n'a pas pu être
// traitée (concurrence "forbid" ou erreur d'enregistrement) : dans ce cas
// l'appelant ne doit pas avancer son ancre de rattrapage (voir
// tickSchedule). Toute erreur est journalisée ; elle n'interrompt jamais le
// traitement des autres schedules.
func (s *Scheduler) triggerOccurrence(ctx context.Context, sched config.Schedule, occurrence time.Time) bool {
	logCtx := []any{"schedule_id", sched.ID, "scheduled_for", occurrence.UTC().Format(time.RFC3339)}

	if sched.Concurrency.Policy != config.ConcurrencyPolicyAllow {
		// Défaut (y compris valeur vide) : forbid, voir PLAN.md §11.4.
		var running bool
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			_, found, err := s.scheduledRuns.FindRunningByScheduleID(ctx, tx, sched.ID)
			running = found
			return err
		})
		if err != nil {
			s.logger.ErrorContext(ctx, "scheduler: échec de la vérification de concurrence", append(logCtx, "error", err)...)
			return false
		}

		if running {
			s.logger.InfoContext(ctx, "scheduler: occurrence ignorée (exécution déjà en cours, politique forbid)", logCtx...)
			return false
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

	s.executeAndDeliver(ctx, sched, runID)

	return true
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
	logCtx := []any{"schedule_id", sched.ID, "scheduled_run_id", runID}

	if sched.Execution.Actions.Policy != config.ActionsPolicyReadOnly {
		// Défendu dès le démarrage par ValidateSchedules : ce cas ne
		// devrait jamais se produire en usage normal.
		s.failRun(ctx, runID, errCodeUnsupportedPolicy)
		s.logger.ErrorContext(ctx, "scheduler: politique d'actions non supportée par cette phase", logCtx...)
		return
	}

	identity, conversation := s.buildIdentity(sched, runID)

	a, err := s.agents.Get(sched.Execution.Agent)
	if err != nil {
		s.failRun(ctx, runID, errCodeAgentNotFound)
		s.logger.ErrorContext(ctx, "scheduler: agent introuvable", append(logCtx, "agent", sched.Execution.Agent, "error", err)...)
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

	// Politique read_only (PLAN.md §11.3) : toute action proposée est
	// journalisée puis ignorée, jamais exécutée ni transformée en plan de
	// confirmation (internal/action n'est pas invoqué ici).
	if len(result.ProposedActions) > 0 {
		s.logger.InfoContext(ctx, "scheduler: actions proposées ignorées (politique read_only)", append(logCtx, "count", len(result.ProposedActions))...)
		reply = fmt.Sprintf("%s\n\n(%d action(s) proposée(s) ignorée(s) : lecture seule)", reply, len(result.ProposedActions))
	}

	if err := s.succeedRun(ctx, runID); err != nil {
		s.logger.ErrorContext(ctx, "scheduler: échec de l'enregistrement du succès de l'exécution", append(logCtx, "error", err)...)
	}

	s.deliverIfNeeded(ctx, sched, runID, reply, false)
}

// buildIdentity construit l'identité d'exécution et la conversation
// synthétique d'une occurrence planifiée (PLAN.md §9.3, §11.2). Aucun accès
// personnel implicite : Scope/ScopeID proviennent exclusivement de la
// configuration de confiance (sched.Execution), jamais d'un contenu fourni
// par l'utilisateur ou le LLM.
func (s *Scheduler) buildIdentity(sched config.Schedule, runID persistence.ScheduledRunID) (model.ExecutionIdentity, model.Conversation) {
	conversationID := model.ConversationID(fmt.Sprintf("schedule:%s:%s", sched.ID, runID))

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
