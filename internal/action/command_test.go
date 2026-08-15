package action_test

import (
	"testing"

	"github.com/bornholm/automata/internal/action"
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
			t.Errorf("ParseCommand(%q): ok=%v, voulu %v", c.text, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Kind != c.kind {
			t.Errorf("ParseCommand(%q): kind=%v, voulu %v", c.text, got.Kind, c.kind)
		}
		if got.PlanNumber != c.number {
			t.Errorf("ParseCommand(%q): number=%d, voulu %d", c.text, got.PlanNumber, c.number)
		}
	}
}
