// Package reminder délivre les rappels ponctuels créés
// conversationnellement (outil create_reminder, internal/agent) : une
// boucle périodique liste les rappels échus en base et envoie leur message
// sur le canal où ils ont été demandés.
//
// La même boucle porte les tâches planifiées (outil schedule_task) : une
// entrée de nature "task" ne délivre pas son texte, elle le donne comme
// consigne à un agent — via TaskRunner — et délivre SA réponse. Tout le
// reste est commun : échéance, récurrence, réarmement, annulation,
// cloisonnement par conversation.
//
// Le message d'un rappel comme la consigne d'une tâche sont du contenu
// privé : ils n'apparaissent jamais dans les journaux (AGENTS.md).
package reminder

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
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

// Rattrapage des échéances manquées (machine éteinte, réseau coupé) : un
// échec de livraison ne classe plus l'entrée failed d'office, sa tentative
// suivante est reprogrammée avec un délai doublé à chaque échec. La fenêtre
// de rattrapage suit la règle demandée par William : sans limite de temps
// pour une entrée à déclenchement unique (mais bornée en tentatives), et
// jusqu'au déclenchement suivant pour une récurrente — un bulletin du matin
// rattrapé l'après-midi a de la valeur, le même bulletin délivré après
// celui du lendemain n'en a plus.
const (
	// retryBaseDelay espace les tentatives : le premier échec retente 5
	// minutes plus tard, puis 10, 20… jusqu'à retryMaxDelay. Une panne de
	// réseau dure rarement moins ; retenter à chaque tick (30 s) ferait
	// jusqu'à 2 880 exécutions d'agent par jour sur une tâche cassée.
	retryBaseDelay = 5 * time.Minute
	retryMaxDelay  = 1 * time.Hour
	// oneShotMaxAttempts borne les tentatives d'une entrée à déclenchement
	// unique, qui n'a pas d'occurrence suivante pour fermer sa fenêtre de
	// rattrapage : sans plafond, une tâche durablement cassée serait
	// retentée pour toujours. 8 tentatives couvrent environ 4 h de panne.
	oneShotMaxAttempts = 8
)

// Dispatcher délivre les rappels échus. Il est mono-instance, comme tout le
// processus (voir docs/security-model.md §4, pas de verrouillage
// inter-processus).
type Dispatcher struct {
	db      *persistence.DB
	repo    *persistence.ReminderRepository
	senders map[string]courier.Provider
	runner  TaskRunner
	logger  *slog.Logger
	metrics *observability.Metrics
	now     func() time.Time
}

// TaskRunner exécute la consigne d'une tâche planifiée et retourne la
// réponse à délivrer. Implémenté par agent.TaskRunner ; l'interface est
// déclarée ici, côté consommateur, pour que la livraison des rappels ne
// dépende pas du paquet agent.
type TaskRunner interface {
	RunTask(ctx context.Context, task persistence.Reminder) (string, error)
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

// WithTaskRunner câble l'exécuteur des tâches planifiées. Sans lui, une
// tâche échue est classée failed plutôt que délivrée à vide : mieux vaut une
// trace d'échec qu'un message vide présenté comme le travail demandé.
func (d *Dispatcher) WithTaskRunner(runner TaskRunner) *Dispatcher {
	d.runner = runner
	return d
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

	// Occurrence périmée d'une entrée récurrente : le déclenchement suivant
	// est déjà passé, la livraison de rattrapage n'a plus de valeur (elle
	// arriverait après — ou à la place — de l'occurrence courante). La
	// série continue sur la prochaine occurrence future, sans livraison.
	if rem.Recurrence != "" {
		if deadline, ok := d.occurrenceDeadline(rem); ok && !d.now().Before(deadline) {
			d.logger.InfoContext(ctx, "reminder: occurrence manquée périmée, sautée",
				append(logCtx, "deadline", deadline.UTC().Format(time.RFC3339), "attempts", rem.Attempts)...)

			if next, ok := d.nextOccurrence(ctx, rem, logCtx); ok {
				d.reschedule(ctx, rem, next, logCtx)
			} else {
				d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
			}
			return
		}
	}

	provider, ok := d.senders[rem.Provider]
	if !ok {
		// Erreur de configuration, pas une panne transitoire : retenter ne
		// changerait rien tant que la configuration n'a pas changé.
		d.logger.ErrorContext(ctx, "reminder: fournisseur courier introuvable, rappel classé failed", logCtx...)
		d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
		return
	}

	// Même statut qu'un fournisseur introuvable : un exécuteur absent est un
	// défaut de câblage, attendre ne le fera pas apparaître. body garde son
	// propre garde-fou nil pour ne jamais paniquer.
	if rem.Kind == persistence.ReminderKindTask && d.runner == nil {
		d.logger.ErrorContext(ctx, "reminder: tâche planifiée sans exécuteur câblé, classée failed", logCtx...)
		d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
		return
	}

	body, ok := d.body(ctx, rem, logCtx)
	if !ok {
		d.retryOrGiveUp(ctx, rem, logCtx)
		return
	}

	outgoing := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(rem.ChannelID)),
		courier.NewUser(courier.UserID(rem.PrincipalID), string(rem.PrincipalID)),
		courier.WithMessageMainPart(body),
	)

	if err := d.send(ctx, provider, outgoing); err != nil {
		d.metrics.IncDeliveryError()
		d.logger.ErrorContext(ctx, "reminder: échec de l'envoi", append(logCtx, "error", err)...)
		d.retryOrGiveUp(ctx, rem, logCtx)
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

// body construit le texte à envoyer. Un rappel délivre son message tel
// quel ; une tâche fait travailler son agent et délivre sa réponse.
//
// ok vaut false quand la tâche n'a pas pu être exécutée : l'entrée part
// alors en reprogrammation de tentative (voir retryOrGiveUp) — une panne de
// réseau au mauvais moment ne doit plus tuer une série récurrente, comme le
// faisait l'ancien classement failed immédiat.
func (d *Dispatcher) body(ctx context.Context, rem persistence.Reminder, logCtx []any) (string, bool) {
	if rem.Kind != persistence.ReminderKindTask {
		return "⏰ Rappel : " + rem.Message, true
	}

	if d.runner == nil {
		d.logger.ErrorContext(ctx, "reminder: tâche planifiée sans exécuteur câblé", logCtx...)
		return "", false
	}

	reply, err := d.runner.RunTask(ctx, rem)
	if err != nil {
		d.logger.ErrorContext(ctx, "reminder: échec de l'exécution de la tâche planifiée", append(logCtx, "error", err)...)
		return "", false
	}

	reply = strings.TrimSpace(reply)
	if reply == "" {
		d.logger.ErrorContext(ctx, "reminder: tâche planifiée sans réponse", logCtx...)
		return "", false
	}

	d.metrics.IncScheduledTaskRun()

	return reply, true
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

// occurrenceDeadline retourne la fin de la fenêtre de rattrapage de
// l'occurrence courante d'une entrée récurrente : le déclenchement SUIVANT
// son échéance stockée. ok vaut false si l'expression stockée est
// inexploitable — l'appelant retombe alors sur le traitement normal, qui la
// journalisera via nextOccurrence.
func (d *Dispatcher) occurrenceDeadline(rem persistence.Reminder) (time.Time, bool) {
	schedule, err := cron.ParseStandard(rem.Recurrence)
	if err != nil {
		return time.Time{}, false
	}

	fireAt, err := time.Parse(time.RFC3339, rem.FireAt)
	if err != nil {
		return time.Time{}, false
	}

	loc := time.Local
	if rem.Timezone != "" {
		if l, err := time.LoadLocation(rem.Timezone); err == nil {
			loc = l
		}
	}

	next := schedule.Next(fireAt.In(loc))
	if next.IsZero() {
		return time.Time{}, false
	}

	return next, true
}

// retryOrGiveUp reprogramme une tentative après un échec de livraison, ou
// renonce quand la fenêtre de rattrapage se ferme.
//
// Récurrent : tentatives espacées d'un délai doublé à chaque échec, jusqu'au
// déclenchement suivant ; l'occurrence est alors abandonnée et la série
// réarmée — elle ne meurt plus sur un échec, contrairement à l'ancien
// comportement. Déclenchement unique : mêmes délais, borné à
// oneShotMaxAttempts tentatives, puis failed (un échec définitif doit
// rester visible).
func (d *Dispatcher) retryOrGiveUp(ctx context.Context, rem persistence.Reminder, logCtx []any) {
	attempts := rem.Attempts + 1

	delay := retryBaseDelay << (attempts - 1)
	if delay > retryMaxDelay || delay <= 0 {
		delay = retryMaxDelay
	}

	nextTry := d.now().Add(delay)

	if rem.Recurrence != "" {
		if deadline, ok := d.occurrenceDeadline(rem); ok && !nextTry.Before(deadline) {
			d.logger.WarnContext(ctx, "reminder: occurrence abandonnée, série réarmée",
				append(logCtx, "attempts", attempts, "deadline", deadline.UTC().Format(time.RFC3339))...)

			if next, ok := d.nextOccurrence(ctx, rem, logCtx); ok {
				d.reschedule(ctx, rem, next, logCtx)
			} else {
				d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
			}
			return
		}
	} else if attempts >= oneShotMaxAttempts {
		d.logger.ErrorContext(ctx, "reminder: tentatives épuisées, rappel classé failed",
			append(logCtx, "attempts", attempts)...)
		d.markFinal(ctx, rem, persistence.ReminderStatusFailed, nil, logCtx)
		return
	}

	d.logger.WarnContext(ctx, "reminder: tentative reprogrammée",
		append(logCtx, "attempts", attempts, "next_try", nextTry.UTC().Format(time.RFC3339))...)

	err := d.db.WithTx(ctx, func(tx *sql.Tx) error {
		ok, err := d.repo.RetryLater(ctx, tx, rem.ID, nextTry.UTC().Format(time.RFC3339), attempts)
		if err != nil {
			return err
		}
		if !ok {
			d.logger.WarnContext(ctx, "reminder: reprogrammation ignorée (rappel plus en pending)", logCtx...)
		}
		return nil
	})
	if err != nil {
		d.logger.ErrorContext(ctx, "reminder: échec de la reprogrammation de tentative", append(logCtx, "error", err)...)
	}
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
