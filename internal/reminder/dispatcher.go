// Package reminder délivre les rappels ponctuels créés
// conversationnellement (outil create_reminder, internal/agent) : une
// boucle périodique liste les rappels échus en base et envoie leur message
// sur le canal où ils ont été demandés.
//
// Contrairement au scheduler (internal/scheduler), rien ici n'exécute
// d'agent : un rappel est un texte figé à la création, délivré tel quel.
// Le message d'un rappel est du contenu privé : il n'apparaît jamais dans
// les journaux (AGENTS.md).
package reminder

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/go-courier"
	cron "github.com/robfig/cron/v3"

	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// tickInterval est la période de la boucle de livraison. 30 s donnent une
// ponctualité largement suffisante pour un rappel humain, sans peser sur la
// base.
const tickInterval = 30 * time.Second

// maxDuePerTick borne le nombre de rappels traités par tick : après un long
// arrêt du processus, les rappels accumulés sont délivrés par vagues plutôt
// que d'un bloc.
const maxDuePerTick = 50

// sendMaxAttempts et sendRetryBaseDelay gouvernent le retry de l'envoi,
// comme dans internal/ingress : une micro-coupure ne doit pas classer un
// rappel failed.
const (
	sendMaxAttempts    = 3
	sendRetryBaseDelay = 1 * time.Second
	sendTimeout        = 30 * time.Second
)

// Dispatcher délivre les rappels échus. Il est mono-instance, comme tout le
// processus (voir docs/security-model.md §4, pas de verrouillage
// inter-processus).
type Dispatcher struct {
	db      *persistence.DB
	repo    *persistence.ReminderRepository
	senders map[string]courier.Provider
	logger  *slog.Logger
	metrics *observability.Metrics
	now     func() time.Time
}

// NewDispatcher construit un Dispatcher. senders associe chaque nom de
// fournisseur courier (tel que figé sur les rappels à leur création) au
// provider correspondant — la même table que celle du scheduler.
func NewDispatcher(db *persistence.DB, senders map[string]courier.Provider, logger *slog.Logger, metrics *observability.Metrics) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}

	return &Dispatcher{
		db:      db,
		repo:    persistence.NewReminderRepository(),
		senders: senders,
		logger:  logger,
		metrics: metrics,
		now:     time.Now,
	}
}

// WithClock remplace l'horloge (tests).
func (d *Dispatcher) WithClock(now func() time.Time) *Dispatcher {
	d.now = now
	return d
}

// Run exécute la boucle de livraison jusqu'à l'annulation de ctx. Un tick a
// lieu immédiatement au démarrage : les rappels devenus échus pendant un
// arrêt du processus partent sans attendre la première période.
func (d *Dispatcher) Run(ctx context.Context) error {
	if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
		d.logger.ErrorContext(ctx, "reminder: échec du tick initial", "error", err)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
				d.logger.ErrorContext(ctx, "reminder: échec du tick", "error", err)
			}
		}
	}
}

// Tick délivre tous les rappels échus à l'instant courant, bornés à
// maxDuePerTick. Une erreur sur un rappel n'interrompt jamais les suivants.
func (d *Dispatcher) Tick(ctx context.Context) error {
	nowUTC := d.now().UTC().Format(time.RFC3339)

	var due []persistence.Reminder
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		due, err = d.repo.ListDue(ctx, tx, nowUTC, maxDuePerTick)
		return err
	})
	if err != nil {
		return fmt.Errorf("reminder: liste des rappels dus: %w", err)
	}

	for _, rem := range due {
		d.deliver(ctx, rem)
	}

	return nil
}

// deliver envoie un rappel et enregistre son statut final. L'ordre est
// délibéré : envoi d'abord, statut ensuite. Un crash entre les deux
// renverra le même rappel au redémarrage (doublon visible sur le canal),
// préférable à l'inverse — un statut sent sans envoi perdrait le rappel
// silencieusement.
func (d *Dispatcher) deliver(ctx context.Context, rem persistence.Reminder) {
	// Uniquement des identifiants : jamais rem.Message.
	logCtx := []any{
		"reminder_id", rem.ID,
		"org_id", rem.OrgID,
		"principal_id", rem.PrincipalID,
		"conversation_id", rem.ConversationID,
		"provider", rem.Provider,
		"channel_id", rem.ChannelID,
		"fire_at", rem.FireAt,
	}

	provider, ok := d.senders[rem.Provider]
	if !ok {
		d.logger.ErrorContext(ctx, "reminder: fournisseur courier introuvable, rappel classé failed", logCtx...)
		d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
		return
	}

	outgoing := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(rem.ChannelID)),
		courier.NewUser(courier.UserID(rem.PrincipalID), string(rem.PrincipalID)),
		courier.WithMessageMainPart("⏰ Rappel : "+rem.Message),
	)

	if err := d.send(ctx, provider, outgoing); err != nil {
		d.metrics.IncDeliveryError()
		d.logger.ErrorContext(ctx, "reminder: échec de l'envoi, rappel classé failed", append(logCtx, "error", err)...)
		d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
		return
	}

	d.metrics.IncReminderSent()

	// Un rappel récurrent ne se termine jamais par un envoi : son échéance
	// avance sur l'occurrence suivante et il reste pending, jusqu'à son
	// annulation. Le calcul part de l'instant courant, pas de l'échéance
	// passée : après un long arrêt du processus, une seule livraison de
	// rattrapage, jamais une rafale.
	if rem.Recurrence != "" {
		if next, ok := d.nextOccurrence(ctx, rem, logCtx); ok {
			d.logger.InfoContext(ctx, "reminder: rappel récurrent délivré et réarmé", append(logCtx, "next_fire_at", next)...)
			d.reschedule(ctx, rem, next, logCtx)
			return
		}
		// Expression devenue inexploitable (donnée corrompue, fuseau
		// disparu) : la série s'arrête proprement, déjà journalisé par
		// nextOccurrence.
	}

	sentAt := d.now().UTC().Format(time.RFC3339)
	d.logger.InfoContext(ctx, "reminder: rappel délivré", logCtx...)
	d.markFinal(ctx, rem, persistence.ReminderStatusSent, &sentAt, logCtx)
}

// nextOccurrence calcule l'occurrence suivante d'un rappel récurrent, en
// RFC 3339 UTC. ok vaut false si l'expression cron ou le fuseau stockés sont
// inexploitables — cas anormal (validés à la création), journalisé en erreur.
func (d *Dispatcher) nextOccurrence(ctx context.Context, rem persistence.Reminder, logCtx []any) (string, bool) {
	schedule, err := cron.ParseStandard(rem.Recurrence)
	if err != nil {
		d.logger.ErrorContext(ctx, "reminder: expression de récurrence inexploitable, série terminée", append(logCtx, "error", err)...)
		return "", false
	}

	loc := time.Local
	if rem.Timezone != "" {
		loc, err = time.LoadLocation(rem.Timezone)
		if err != nil {
			d.logger.ErrorContext(ctx, "reminder: fuseau de récurrence inexploitable, série terminée", append(logCtx, "error", err)...)
			return "", false
		}
	}

	next := schedule.Next(d.now().In(loc))
	if next.IsZero() {
		d.logger.ErrorContext(ctx, "reminder: aucune occurrence suivante, série terminée", logCtx...)
		return "", false
	}

	return next.UTC().Format(time.RFC3339), true
}

// reschedule avance l'échéance d'un rappel récurrent. Si le rappel n'est
// plus pending (annulé pendant la livraison), la série s'arrête là — rien
// n'est écrasé.
func (d *Dispatcher) reschedule(ctx context.Context, rem persistence.Reminder, nextFireAt string, logCtx []any) {
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		ok, err := d.repo.RescheduleNext(ctx, tx, rem.ID, nextFireAt)
		if err != nil {
			return err
		}
		if !ok {
			d.logger.WarnContext(ctx, "reminder: réarmement ignoré (rappel plus en pending)", logCtx...)
		}
		return nil
	})
	if err != nil {
		d.logger.ErrorContext(ctx, "reminder: échec du réarmement du rappel récurrent", append(logCtx, "error", err)...)
	}
}

// send tente l'envoi jusqu'à sendMaxAttempts fois avec backoff doublé,
// chaque tentative bornée par sendTimeout.
func (d *Dispatcher) send(ctx context.Context, provider courier.Provider, outgoing courier.Message) error {
	var err error
	for attempt := 1; attempt <= sendMaxAttempts; attempt++ {
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		err = provider.Send(sendCtx, outgoing)
		cancel()
		if err == nil {
			return nil
		}

		if ctx.Err() != nil || attempt == sendMaxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return err
		case <-time.After(sendRetryBaseDelay << (attempt - 1)):
		}
	}

	return err
}

// markFinal fait passer le rappel de pending à status. Si le rappel n'est
// plus pending (annulé pendant l'envoi), le résultat de la course est
// journalisé mais rien n'est écrasé.
func (d *Dispatcher) markFinal(ctx context.Context, rem persistence.Reminder, status string, sentAt *string, logCtx []any) {
	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		ok, err := d.repo.UpdateStatus(ctx, tx, rem.ID, persistence.ReminderStatusPending, status, sentAt)
		if err != nil {
			return err
		}
		if !ok {
			d.logger.WarnContext(ctx, "reminder: statut final non enregistré (rappel plus en pending)", append(logCtx, "status", status)...)
		}
		return nil
	})
	if err != nil {
		d.logger.ErrorContext(ctx, "reminder: échec de l'enregistrement du statut final", append(logCtx, "status", status, "error", err)...)
	}
}
