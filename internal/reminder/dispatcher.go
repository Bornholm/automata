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

	sentAt := d.now().UTC().Format(time.RFC3339)
	d.metrics.IncReminderSent()
	d.logger.InfoContext(ctx, "reminder: rappel délivré", logCtx...)
	d.markFinal(ctx, rem, persistence.ReminderStatusSent, &sentAt, logCtx)
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
