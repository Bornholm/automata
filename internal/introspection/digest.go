package introspection

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// La synthèse mensuelle : ce que l'exploitant apprend des frictions de son
// instance, sans jamais rien apprendre de ses membres.
//
// L'anonymat est structurel, pas déclaratif : la synthèse est composée
// uniquement d'agrégats SQL (COUNT … GROUP BY) sur des colonnes de
// métadonnées — aucun nom, aucun identifiant de membre, aucun titre de
// suggestion n'entre dans le texte. C'est la boucle produit de
// l'exploitant : savoir QUOI construire ou réparer, jamais POUR QUI.

// defaultDigestCron est l'échéance de la synthèse : le premier du mois.
const defaultDigestCron = "50 5 1 * *"

// digestTask est l'ancrage maintenance_runs de la synthèse, global.
const digestTask = "introspection_digest"

// digestWindow est la fenêtre des agrégats.
const digestWindow = 31 * 24 * time.Hour

// DigestSender remet la synthèse à l'exploitant. Implémenté par
// alerting.Notifier : déduplication et rejeu si la messagerie était en
// panne, et visibilité dans l'écran Alertes.
type DigestSender interface {
	Notify(ctx context.Context, kind, subject, message string) error
}

// WithDigest active la synthèse mensuelle.
func (i *Introspector) WithDigest(sender DigestSender, cronSpec string) (*Introspector, error) {
	spec := cronSpec
	if spec == "" {
		spec = defaultDigestCron
	}
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("introspection: expression cron de synthèse %q invalide: %w", spec, err)
	}

	i.digestSender = sender
	i.digestSchedule = schedule
	return i, nil
}

// tickDigest vérifie l'échéance de la synthèse. Même ancrage que la passe :
// premier passage sans exécution.
func (i *Introspector) tickDigest(ctx context.Context, now time.Time) error {
	if i.digestSender == nil {
		return nil
	}

	var (
		lastRun time.Time
		found   bool
	)
	if err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		lastRun, found, err = i.runs.GetLastRun(ctx, tx, digestTask)
		return err
	}); err != nil {
		return err
	}

	if !found {
		return i.recordRun(ctx, digestTask, now)
	}
	if i.digestSchedule.Next(lastRun).After(now) {
		return nil
	}

	message, err := i.composeDigest(ctx, now)
	if err != nil {
		return err
	}

	if err := i.digestSender.Notify(ctx, "introspection_digest", "monthly", message); err != nil {
		// La remise est du ressort d'alerting (rejeu inclus) : une erreur
		// ici est un échec d'enregistrement, pas d'envoi.
		return err
	}

	return i.recordRun(ctx, digestTask, now)
}

// composeDigest produit le texte : uniquement des agrégats.
func (i *Introspector) composeDigest(ctx context.Context, now time.Time) (string, error) {
	since := now.Add(-digestWindow)
	sinceStr := since.Format(time.RFC3339)

	var (
		plansByPermission map[string]int
		remindersFailed   int
		tasksFailed       int
		suggestionCounts  map[string]int
	)

	err := i.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error

		// Les plans jamais aboutis, par domaine de permission : c'est le
		// domaine qui dit à l'exploitant OÙ se situe la friction (courriel,
		// agenda…), sans rien dire de qui ni de quoi.
		plansByPermission = map[string]int{}
		rows, err := tx.QueryContext(ctx, `
			SELECT a.required_permission, COUNT(*)
			FROM action_plans p
			JOIN actions a ON a.plan_id = p.id AND a.position = 0
			WHERE p.created_at >= ? AND p.status IN ('expired', 'failed')
			GROUP BY a.required_permission`, sinceStr)
		if err != nil {
			return fmt.Errorf("agrégat des plans: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var permission string
			var count int
			if err := rows.Scan(&permission, &count); err != nil {
				return fmt.Errorf("lecture d'un agrégat de plans: %w", err)
			}
			plansByPermission[permission] = count
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("parcours des plans: %w", err)
		}

		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reminders
			WHERE created_at >= ? AND status = 'failed' AND kind = 'message'`, sinceStr).
			Scan(&remindersFailed); err != nil {
			return fmt.Errorf("agrégat des rappels: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reminders
			WHERE created_at >= ? AND status = 'failed' AND kind = 'task'`, sinceStr).
			Scan(&tasksFailed); err != nil {
			return fmt.Errorf("agrégat des tâches: %w", err)
		}

		suggestionCounts, err = i.suggestions.CountByStatusSince(ctx, tx, since)
		return err
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("📊 Synthèse mensuelle d'Automata — frictions et suggestions des 31 derniers jours.\n")

	if len(plansByPermission) == 0 && remindersFailed == 0 && tasksFailed == 0 {
		b.WriteString("\nAucune friction relevée. ")
	} else {
		b.WriteString("\nFrictions :\n")
		// Ordre stable pour un texte comparable de mois en mois.
		permissions := make([]string, 0, len(plansByPermission))
		for permission := range plansByPermission {
			permissions = append(permissions, permission)
		}
		sort.Strings(permissions)
		for _, permission := range permissions {
			fmt.Fprintf(&b, "- %d action(s) proposée(s) jamais confirmée(s) : %s\n", plansByPermission[permission], permission)
		}
		if remindersFailed > 0 {
			fmt.Fprintf(&b, "- %d rappel(s) en échec de remise\n", remindersFailed)
		}
		if tasksFailed > 0 {
			fmt.Fprintf(&b, "- %d tâche(s) planifiée(s) en échec\n", tasksFailed)
		}
	}

	emitted := 0
	for _, count := range suggestionCounts {
		emitted += count
	}
	if emitted > 0 {
		fmt.Fprintf(&b, "\nSuggestions : %d émise(s), %d suivie(s), %d écartée(s).\n",
			emitted,
			suggestionCounts["accepted"],
			suggestionCounts["dismissed"])
	} else {
		b.WriteString("Aucune suggestion émise ce mois-ci.\n")
	}

	b.WriteString("\nCes chiffres sont des agrégats anonymes : aucun contenu ni aucun membre n'y figure.")

	return b.String(), nil
}
