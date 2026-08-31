// Package alerting porte les alertes d'exploitation jusqu'à l'exploitant,
// dans sa conversation privée.
//
// C'est le choix qui distingue ce système d'un collecteur de métriques :
// /metrics et /healthz existent déjà, mais il faut aller les regarder. Une
// session de messagerie perdue un vendredi soir ne se remarque autrement
// qu'au premier message sans réponse, le lundi. L'alerte arrive donc là où
// l'exploitant regarde déjà — sa messagerie.
//
// Ce choix a une limite, assumée et documentée : quand la panne EST la
// messagerie, l'alerte ne peut pas partir. Elle est alors journalisée en
// ERROR, conservée non remise, et rejouée dès que le canal revient
// (Flush). Un repli par courriel couvrirait ce cas ; il n'est pas en place.
package alerting

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/weblink"
)

// Natures d'alerte. Elles servent de clé de déduplication avec le sujet, et
// se retrouvent telles quelles dans l'écran d'administration.
const (
	// KindPlatformDown : un compte de messagerie a cessé de répondre.
	KindPlatformDown = "platform_down"
	// KindPlatformMute : un compte de messagerie se déclare en marche mais
	// ne répond plus — le cas le plus traître, puisque rien d'autre ne le
	// signale.
	KindPlatformMute = "platform_mute"
	// KindPluginFailed : un plugin ne démarre plus.
	KindPluginFailed = "plugin_failed"
	// KindModelRoleMissing : un rôle du catalogue n'a aucun modèle, et la
	// fonctionnalité correspondante est donc muette.
	KindModelRoleMissing = "model_role_missing"
	// KindBackupFailed : une sauvegarde a échoué.
	KindBackupFailed = "backup_failed"
)

// repeatAfter est le délai avant qu'une alerte identique reparte. Sans lui,
// un plugin qui redémarre en boucle inonderait la conversation de
// l'exploitant — et une conversation inondée ne se lit plus, ce qui revient
// à n'avoir aucune alerte.
const repeatAfter = time.Hour

// retention borne le journal des alertes : c'est un pense-bête
// d'exploitation, pas un historique d'audit.
const retention = 30 * 24 * time.Hour

// maxPendingFlush borne une reprise : au retour d'une longue panne, mieux
// vaut quelques alertes récentes qu'un déversement de trois jours.
const maxPendingFlush = 5

// Sender remet un message à un membre, dans sa conversation privée.
// Implémenté par le notifieur du registre (internal/registry/notify.go), qui
// sait résoudre le membre et son compte de messagerie.
type Sender interface {
	NotifyOperator(ctx context.Context, memberID, message string) error
}

// Notifier émet les alertes.
type Notifier struct {
	db       *persistence.DB
	alerts   *persistence.AlertRepository
	settings *persistence.InstanceSettingRepository
	sender   Sender
	logger   *slog.Logger
	now      func() time.Time
}

// New construit le notifieur. sender peut être nil : les alertes sont alors
// enregistrées et journalisées, mais jamais remises — l'état d'une instance
// sans messagerie configurée.
func New(db *persistence.DB, sender Sender, logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}

	return &Notifier{
		db:       db,
		alerts:   persistence.NewAlertRepository(),
		settings: persistence.NewInstanceSettingRepository(),
		sender:   sender,
		logger:   logger,
		now:      time.Now,
	}
}

// WithClock remplace l'horloge (tests).
func (n *Notifier) WithClock(now func() time.Time) *Notifier {
	n.now = now
	return n
}

// Notify signale un problème. kind et subject forment la clé de
// déduplication ; message est le texte remis à l'exploitant, rédigé par
// l'application — jamais par le modèle : une alerte doit être exacte, et
// elle part sans que personne l'ait relue.
//
// L'erreur retournée ne concerne que l'enregistrement. Un échec de REMISE
// n'en est pas une : l'alerte reste en attente et sera rejouée. Alerter est
// une courtoisie du système envers son exploitant, jamais une raison de
// faire échouer ce qui l'a déclenchée.
func (n *Notifier) Notify(ctx context.Context, kind, subject, message string) error {
	now := n.now().UTC()

	var (
		alert   persistence.Alert
		skipped bool
	)
	err := n.db.WithTx(ctx, func(tx *sql.Tx) error {
		last, err := n.alerts.LastOf(ctx, tx, kind, subject)
		if err != nil {
			return err
		}
		if !last.IsZero() && now.Sub(last) < repeatAfter {
			skipped = true
			return nil
		}

		id, err := weblink.RandomCrockford(16)
		if err != nil {
			return err
		}

		alert = persistence.Alert{
			ID: id, Kind: kind, Subject: subject, Message: message, CreatedAt: now,
		}
		return n.alerts.Insert(ctx, tx, alert)
	})
	if err != nil {
		return fmt.Errorf("alerting: enregistrement de l'alerte: %w", err)
	}
	if skipped {
		return nil
	}

	// Le journal porte l'alerte même si la remise échoue : c'est la trace
	// de dernier recours, celle qu'on retrouve dans dokku logs.
	n.logger.WarnContext(ctx, "alerting: alerte d'exploitation",
		"kind", kind, "subject", subject, "message", message)

	n.deliver(ctx, alert)

	return nil
}

// deliver tente la remise et note l'issue. Une remise impossible n'est pas
// une erreur : l'alerte attend son tour.
func (n *Notifier) deliver(ctx context.Context, alert persistence.Alert) {
	if n.sender == nil {
		return
	}

	operator, err := n.operator(ctx)
	if err != nil || operator == "" {
		if err != nil {
			n.logger.WarnContext(ctx, "alerting: exploitant introuvable", "error", err)
		}
		return
	}

	if err := n.sender.NotifyOperator(ctx, operator, alert.Message); err != nil {
		n.logger.ErrorContext(ctx, "alerting: alerte non remise, elle sera rejouée",
			"kind", alert.Kind, "subject", alert.Subject, "error", err)
		return
	}

	if err := n.db.WithTx(ctx, func(tx *sql.Tx) error {
		return n.alerts.MarkDelivered(ctx, tx, alert.ID, n.now().UTC())
	}); err != nil {
		// Sans conséquence pour l'exploitant, qui a reçu son message : au
		// pire l'alerte sera remise une seconde fois.
		n.logger.WarnContext(ctx, "alerting: remise non enregistrée", "error", err)
	}
}

// Flush rejoue les alertes restées en attente. À appeler quand un canal
// revient : c'est ce qui rattrape les alertes émises pendant que la
// messagerie était en panne — celles, précisément, qui comptaient le plus.
func (n *Notifier) Flush(ctx context.Context) {
	if n.sender == nil {
		return
	}

	var pending []persistence.Alert
	if err := n.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		pending, err = n.alerts.ListPending(ctx, tx, maxPendingFlush)
		return err
	}); err != nil {
		n.logger.WarnContext(ctx, "alerting: alertes en attente illisibles", "error", err)
		return
	}

	for _, alert := range pending {
		n.deliver(ctx, alert)
	}
}

// Purge efface les alertes trop anciennes.
func (n *Notifier) Purge(ctx context.Context) {
	cutoff := n.now().UTC().Add(-retention)

	if err := n.db.WithTx(ctx, func(tx *sql.Tx) error {
		deleted, err := n.alerts.DeleteBefore(ctx, tx, cutoff)
		if err != nil {
			return err
		}
		if deleted > 0 {
			n.logger.InfoContext(ctx, "alerting: journal des alertes purgé", "deleted", deleted)
		}
		return nil
	}); err != nil {
		n.logger.WarnContext(ctx, "alerting: purge des alertes en échec", "error", err)
	}
}

// operator retourne le membre désigné pour recevoir les alertes.
func (n *Notifier) operator(ctx context.Context) (string, error) {
	var memberID string
	err := n.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		memberID, err = n.settings.Get(ctx, tx, persistence.SettingOperatorMemberID)
		return err
	})

	return memberID, err
}
