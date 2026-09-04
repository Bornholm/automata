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
	// CommandConfirm correspond à "confirmer" / "confirmer 42", et à ses
	// équivalents anglais et espagnol.
	CommandConfirm CommandKind = iota + 1
	// CommandCancel correspond à "annuler" / "annuler 42", et à ses
	// équivalents anglais et espagnol.
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

// commandWords associe chaque mot littéral accepté à sa commande.
//
// Les trois langues servies par l'instance sont reconnues EN PERMANENCE,
// quelle que soit la langue du membre. Ce n'est pas une commodité, c'est ce
// qui empêche une panne silencieuse : le modèle répond dans la langue du
// message reçu, et quelqu'un qui écrit en espagnol à qui l'on répond
// « responde "confirmar" » tape confirmar. Si ce mot n'était pas reconnu,
// son message repartirait vers le modèle comme un message ordinaire,
// l'action ne serait jamais exécutée — et le modèle, voyant une
// confirmation dans l'historique, aurait toutes les chances d'annoncer
// qu'elle l'a été.
//
// La liste reste FERMÉE et littérale. Aucun modèle ne décide qu'un message
// vaut confirmation (voir internal/conversation.Handler) : c'est le point
// de contrôle de toutes les écritures sensibles, et il ne doit dépendre
// d'aucune inférence.
var commandWords = map[string]CommandKind{
	"confirmer": CommandConfirm,
	"confirm":   CommandConfirm,
	"confirmar": CommandConfirm,
	"annuler":   CommandCancel,
	"cancel":    CommandCancel,
	"cancelar":  CommandCancel,
}

// commandPattern reconnaît un mot de commande, éventuellement suivi d'un
// numéro de plan, espaces superflus tolérés en tête/fin (TrimSpace côté
// appelant) et entre les deux mots (\s+).
var commandPattern = regexp.MustCompile(`^([a-z]+)(?:\s+(\d+))?$`)

// ParseCommand reconnaît un texte de message comme une commande
// conversationnelle littérale de confirmation ou d'annulation (plan de
// conception, §10.4), dans l'une des langues servies.
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

	kind, ok := commandWords[m[1]]
	if !ok {
		return Command{}, false
	}

	cmd := Command{Kind: kind}

	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err == nil {
			cmd.PlanNumber = n
		}
	}

	return cmd, true
}
