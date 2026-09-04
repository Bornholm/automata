package action_test

import (
	"strings"
	"testing"
	"unicode"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/i18n"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		text   string
		wantOK bool
		kind   action.CommandKind
		number int
	}{
		{"confirmer", true, action.CommandConfirm, 0},
		{"Confirmer", true, action.CommandConfirm, 0},
		{"  confirmer  ", true, action.CommandConfirm, 0},
		{"confirmer 42", true, action.CommandConfirm, 42},
		{"CONFIRMER   42", true, action.CommandConfirm, 42},
		{"annuler", true, action.CommandCancel, 0},
		{"annuler 7", true, action.CommandCancel, 7},
		{"bonjour", false, 0, 0},
		{"confirmer plan 42", false, 0, 0},
		{"", false, 0, 0},
		{"confirmez", false, 0, 0},
	}

	for _, c := range cases {
		got, ok := action.ParseCommand(c.text)
		if ok != c.wantOK {
			t.Errorf("action.ParseCommand(%q): ok=%v, voulu %v", c.text, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Kind != c.kind {
			t.Errorf("action.ParseCommand(%q): kind=%v, voulu %v", c.text, got.Kind, c.kind)
		}
		if got.PlanNumber != c.number {
			t.Errorf("action.ParseCommand(%q): number=%d, voulu %d", c.text, got.PlanNumber, c.number)
		}
	}
}

// Le mot de confirmation est le point d'entrée de TOUTE écriture sensible,
// et le modèle répond dans la langue du message reçu. Un mot non reconnu ne
// produit pas d'erreur : le message repart vers le modèle comme un message
// ordinaire, l'action n'est jamais exécutée, et le modèle — qui voit une
// confirmation dans l'historique — annonce volontiers qu'elle l'a été.
// C'est cette panne muette que les six mots empêchent.
func TestParseCommand_AcceptsTheThreeLanguages(t *testing.T) {
	confirms := []string{"confirmer", "confirm", "confirmar", "CONFIRMAR", "  Confirm  "}
	cancels := []string{"annuler", "cancel", "cancelar", "Annuler"}

	for _, text := range confirms {
		cmd, ok := action.ParseCommand(text)
		if !ok || cmd.Kind != action.CommandConfirm {
			t.Errorf("action.ParseCommand(%q) = %+v, %v — confirmation attendue", text, cmd, ok)
		}
	}
	for _, text := range cancels {
		cmd, ok := action.ParseCommand(text)
		if !ok || cmd.Kind != action.CommandCancel {
			t.Errorf("action.ParseCommand(%q) = %+v, %v — annulation attendue", text, cmd, ok)
		}
	}

	// Le numéro de plan suit le mot dans toutes les langues.
	if cmd, ok := action.ParseCommand("confirmar 2"); !ok || cmd.Kind != action.CommandConfirm || cmd.PlanNumber != 2 {
		t.Errorf("action.ParseCommand(\"confirmar 2\") = %+v, %v", cmd, ok)
	}
}

// La liste reste FERMÉE : ce qui n'y est pas doit repartir au modèle comme
// un message ordinaire, jamais exécuter quoi que ce soit. Un « oui » vaut
// accord dans la conversation, pas confirmation d'une écriture.
func TestParseCommand_StaysClosed(t *testing.T) {
	for _, text := range []string{
		"oui", "yes", "sí", "vale", "ok", "d'accord",
		"confirme", "confirmé", "confirms", "confirmame",
		"je confirme", "confirmer le rendez-vous", "cancelling",
		"", "  ", "confirmer 2 fois",
	} {
		if cmd, ok := action.ParseCommand(text); ok {
			t.Errorf("action.ParseCommand(%q) = %+v, reconnu à tort comme commande", text, cmd)
		}
	}
}

// Le lien entre le catalogue et l'analyseur, qu'aucun des deux ne garantit
// seul : le texte qui annonce la proposition DOIT nommer un mot que
// ParseCommand reconnaît. Traduire la phrase sans traduire — ou en
// traduisant mal — le mot produirait la panne muette décrite plus haut,
// cette fois par notre faute et non par celle du modèle.
func TestProposalFooter_NamesWordsTheParserAccepts(t *testing.T) {
	for _, locale := range i18n.Supported {
		footer := i18n.T(locale, "action.proposal.footer", "5m")

		var confirm, cancel bool
		for _, word := range strings.FieldsFunc(footer, func(r rune) bool {
			return !unicode.IsLetter(r)
		}) {
			cmd, ok := action.ParseCommand(word)
			if !ok {
				continue
			}
			confirm = confirm || cmd.Kind == action.CommandConfirm
			cancel = cancel || cmd.Kind == action.CommandCancel
		}

		if !confirm {
			t.Errorf("%s: la proposition ne nomme aucun mot de confirmation reconnu: %q", locale, footer)
		}
		if !cancel {
			t.Errorf("%s: la proposition ne nomme aucun mot d'annulation reconnu: %q", locale, footer)
		}
	}
}
