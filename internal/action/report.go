package action

import (
	"fmt"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

// formatPlanProposal formate le texte renvoyé à l'utilisateur juste après
// la création d'un plan (PLAN.md §8.5, "afficher une liste numérotée",
// généralisé ici à toute action).
func formatPlanProposal(actions []persistence.Action, ttl time.Duration) string {
	var b strings.Builder

	b.WriteString("Actions proposées, en attente de confirmation :\n")
	for i, a := range actions {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a.Summary)
	}
	fmt.Fprintf(&b, "\nRépondez « confirmer » pour exécuter, ou « annuler » pour abandonner. Cette proposition expire dans %s.", ttl.Round(time.Second))

	return b.String()
}

// formatAmbiguousPlans formate le texte demandant explicitement lequel des
// plans actifs confirmer (PLAN.md §10.4).
func formatAmbiguousPlans(plans []persistence.ActionPlan) string {
	var b strings.Builder

	b.WriteString("Plusieurs plans d'actions sont en attente de confirmation dans cette conversation. Précisez lequel, par exemple « confirmer 1 » :\n")
	for i, p := range plans {
		fmt.Fprintf(&b, "%d. plan proposé le %s (statut : %s)\n", i+1, p.CreatedAt, p.Status)
	}

	return b.String()
}

// finalPlanStatus détermine le statut final d'un plan à partir des
// résultats de ses actions (PLAN.md §10.5 point 9) : succeeded si toutes
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
// après exécution d'un plan confirmé (PLAN.md §10.5 point 9, "enregistrer
// le résultat" — le texte restitué doit rendre explicites les succès et les
// échecs partiels).
func formatExecutionReport(finalStatus string, outcomes []actionOutcome) string {
	var b strings.Builder

	switch finalStatus {
	case StatusSucceeded:
		b.WriteString("Toutes les actions du plan ont été exécutées avec succès :\n")
	case StatusPartiallySucceeded:
		b.WriteString("Le plan a été exécuté partiellement :\n")
	default:
		b.WriteString("Aucune action du plan n'a pu être exécutée :\n")
	}

	for i, o := range outcomes {
		status := "échec"
		if o.ok {
			status = "réussi"
		}
		fmt.Fprintf(&b, "%d. [%s] %s — %s\n", i+1, status, o.action.Summary, o.message)
	}

	return b.String()
}
