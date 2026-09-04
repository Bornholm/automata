package action

import (
	"fmt"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/persistence"
)

// formatPlanProposal formate le texte renvoyé à l'utilisateur juste après
// la création d'un plan (plan de conception, §8.5, "afficher une liste numérotée",
// généralisé ici à toute action).
func formatPlanProposal(locale i18n.Locale, actions []persistence.Action, ttl time.Duration) string {
	var b strings.Builder

	b.WriteString(i18n.T(locale, "action.proposal.header"))
	for i, a := range actions {
		// Le résumé de l'action vient du modèle, qui l'a écrit dans la
		// langue du message : il n'y a rien à traduire ici.
		fmt.Fprintf(&b, "%d. %s\n", i+1, a.Summary)
	}
	b.WriteString(i18n.T(locale, "action.proposal.footer", ttl.Round(time.Second)))

	return b.String()
}

// formatAmbiguousPlans formate le texte demandant explicitement lequel des
// plans actifs confirmer (plan de conception, §10.4).
func formatAmbiguousPlans(locale i18n.Locale, plans []persistence.ActionPlan) string {
	var b strings.Builder

	b.WriteString(i18n.T(locale, "action.ambiguous.header"))
	for i, p := range plans {
		b.WriteString(i18n.T(locale, "action.ambiguous.item", i+1, p.CreatedAt, p.Status))
	}

	return b.String()
}

// finalPlanStatus détermine le statut final d'un plan à partir des
// résultats de ses actions (plan de conception, §10.5 point 9) : succeeded si toutes
// ont réussi, failed si toutes ont échoué, partially_succeeded sinon.
func finalPlanStatus(outcomes []actionOutcome) string {
	if len(outcomes) == 0 {
		return StatusFailed
	}

	succeeded := 0
	for _, o := range outcomes {
		if o.ok {
			succeeded++
		}
	}

	switch {
	case succeeded == len(outcomes):
		return StatusSucceeded
	case succeeded == 0:
		return StatusFailed
	default:
		return StatusPartiallySucceeded
	}
}

// formatExecutionReport formate le rapport final envoyé à l'utilisateur
// après exécution d'un plan confirmé (plan de conception, §10.5 point 9, "enregistrer
// le résultat" — le texte restitué doit rendre explicites les succès et les
// échecs partiels).
func formatExecutionReport(locale i18n.Locale, finalStatus string, outcomes []actionOutcome) string {
	var b strings.Builder

	switch finalStatus {
	case StatusSucceeded:
		b.WriteString(i18n.T(locale, "action.report.all_succeeded"))
	case StatusPartiallySucceeded:
		b.WriteString(i18n.T(locale, "action.report.partial"))
	default:
		b.WriteString(i18n.T(locale, "action.report.none"))
	}

	for i, o := range outcomes {
		status := i18n.T(locale, "action.report.status_failed")
		if o.ok {
			status = i18n.T(locale, "action.report.status_ok")
		}
		fmt.Fprintf(&b, "%d. [%s] %s — %s\n", i+1, status, o.action.Summary, o.message)
	}

	return b.String()
}
