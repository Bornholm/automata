package ingress

import (
	"testing"

	"github.com/bornholm/automata/internal/weblink"
)

func TestFindLinkToken_AcceptsDisplayedForm(t *testing.T) {
	clear, _, display, err := weblink.NewLinkToken()
	if err != nil {
		t.Fatalf("NewLinkToken: %v", err)
	}

	cases := map[string]string{
		"brut":                   clear,
		"forme affichée":         display,
		"dans une phrase":        "bonjour voici mon code " + clear + " merci",
		"minuscules":             lower(clear),
		"avec retour à la ligne": "voici :\n" + display,
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			found, ok := findLinkToken(text)
			if !ok {
				t.Fatalf("jeton non reconnu dans %q", text)
			}
			// Toutes les formes doivent se ramener au jeton haché : c'est le
			// seul moyen de retrouver la ligne en base.
			if weblink.HashToken(found) != weblink.HashToken(clear) {
				t.Errorf("jeton normalisé %q, attendu %q", found, clear)
			}
		})
	}
}

func TestFindLinkToken_IgnoresOrdinaryText(t *testing.T) {
	for _, text := range []string{
		"", "bonjour, comment ça va ?",
		"atm_TROPCOURT",
		"mon code postal est 44000",
	} {
		if _, ok := findLinkToken(text); ok {
			t.Errorf("aucun jeton ne doit être reconnu dans %q", text)
		}
	}
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
