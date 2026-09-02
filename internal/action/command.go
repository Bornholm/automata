// Package action implémente le système centralisé et persisté de plans
// d'actions (plan de conception, §10, Phase 15) : proposition, confirmation et
// exécution d'actions sensibles (écritures non idempotentes, suppressions),
// en remplacement des mécanismes ad-hoc en mémoire des phases précédentes
// (forget_memory §8.5, agenda §10/Phase 13, todo Phase 14).
package action

import (
	"regexp"
	"strconv"
	"strings"
)

// CommandKind énumère les commandes conversationnelles littérales
// supportées (plan de conception, §10.4).
type CommandKind int

const (
	// CommandConfirm correspond à "confirmer" / "confirmer 42".
	CommandConfirm CommandKind = iota + 1
	// CommandCancel correspond à "annuler" / "annuler 42".
	CommandCancel
)

// Command décrit une commande conversationnelle reconnue par ParseCommand.
type Command struct {
	Kind CommandKind
	// PlanNumber est le numéro d'ordre du plan visé (1-based, parmi les
	// plans actifs de la conversation courante, voir Engine.HandleCommand),
	// ou 0 si non précisé par l'utilisateur.
	PlanNumber int
}

// commandPattern reconnaît "confirmer"/"annuler", éventuellement suivi d'un
// numéro de plan, espaces superflus tolérés en tête/fin (TrimSpace côté
// appelant) et entre les deux mots (\s+).
var commandPattern = regexp.MustCompile(`^(confirmer|annuler)(?:\s+(\d+))?$`)

// ParseCommand reconnaît un texte de message comme une commande
// conversationnelle littérale "confirmer"/"annuler" (plan de conception, §10.4).
// Insensible à la casse, tolère les espaces superflus. ok vaut false si
// text n'est pas une de ces commandes : dans ce cas, l'appelant doit
// continuer le traitement normal (jamais invoquer le LLM pour décider si
// c'est une commande, voir internal/conversation.Handler).
func ParseCommand(text string) (Command, bool) {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.Join(strings.Fields(normalized), " ")

	m := commandPattern.FindStringSubmatch(normalized)
	if m == nil {
		return Command{}, false
	}

	cmd := Command{}
	switch m[1] {
	case "confirmer":
		cmd.Kind = CommandConfirm
	case "annuler":
		cmd.Kind = CommandCancel
	}

	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err == nil {
			cmd.PlanNumber = n
		}
	}

	return cmd, true
}
