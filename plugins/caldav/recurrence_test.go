package main

import (
	"testing"
	"time"

	"github.com/teambition/rrule-go"
)

var testStart = time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)

// Les formes couramment produites par l'assistant doivent traverser sans
// perte : c'est là-dessus que se joue l'utilité du magasin délégué.
func TestCronToRRule_TranslatesCommonForms(t *testing.T) {
	cases := map[string]struct {
		expr string
		freq rrule.Frequency
		days []rrule.Weekday
		mday []int
		mons []int
	}{
		"tous les jours à 20h00":     {"0 20 * * *", rrule.DAILY, nil, nil, nil},
		"chaque mardi à 20h00":       {"0 20 * * 2", rrule.WEEKLY, []rrule.Weekday{rrule.TU}, nil, nil},
		"lundi et jeudi":             {"30 8 * * 1,4", rrule.WEEKLY, []rrule.Weekday{rrule.MO, rrule.TH}, nil, nil},
		"le 1er de chaque mois":      {"0 9 1 * *", rrule.MONTHLY, nil, []int{1}, nil},
		"le 15 janvier de chaque an": {"0 9 15 1 *", rrule.YEARLY, nil, []int{15}, []int{1}},
		"le dimanche écrit 7":        {"0 10 * * 7", rrule.WEEKLY, []rrule.Weekday{rrule.SU}, nil, nil},
	}

	for name, tc := range cases {
		opt, err := cronToRRule(tc.expr, testStart)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if opt.Freq != tc.freq {
			t.Errorf("%s: fréquence %v, attendu %v", name, opt.Freq, tc.freq)
		}
		if len(opt.Byweekday) != len(tc.days) {
			t.Errorf("%s: %d jour(s) de semaine, attendu %d", name, len(opt.Byweekday), len(tc.days))
		}
		if tc.mday != nil && len(opt.Bymonthday) != len(tc.mday) {
			t.Errorf("%s: jours du mois %v, attendu %v", name, opt.Bymonthday, tc.mday)
		}
		if tc.mons != nil && len(opt.Bymonth) != len(tc.mons) {
			t.Errorf("%s: mois %v, attendu %v", name, opt.Bymonth, tc.mons)
		}
	}
}

// L'heure de la première occurrence est celle que la règle reproduit :
// une traduction qui décalerait l'heure serait invisible à la lecture et
// visible à la première sonnerie.
func TestCronToRRule_KeepsTheTimeOfDay(t *testing.T) {
	opt, err := cronToRRule("30 8 * * 1", testStart)
	if err != nil {
		t.Fatalf("traduction: %v", err)
	}

	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		t.Fatalf("règle invalide: %v", err)
	}

	next := rule.After(testStart, false)
	if next.IsZero() {
		t.Fatal("la règle n'a aucune occurrence future")
	}
	if next.Hour() != 8 || next.Minute() != 30 {
		t.Errorf("prochaine occurrence à %02d:%02d, attendu 08:30", next.Hour(), next.Minute())
	}
	if next.Weekday() != time.Monday {
		t.Errorf("prochaine occurrence un %s, attendu lundi", next.Weekday())
	}
}

// Ce qui n'est pas exprimable est refusé, jamais approché : un rappel
// rangé sous une règle plus large ou plus étroite que demandée est un
// mensonge que la personne découvre en recevant les mauvais rappels.
func TestCronToRRule_RefusesWhatItCannotExpress(t *testing.T) {
	refused := map[string]string{
		// cron déclenche sur l'un OU l'autre, iCalendar croise les deux :
		// traduire donnerait des rappels manquants.
		"jour du mois et de semaine": "0 9 1 * 1",
		// Un intervalle n'est pas une entrée d'agenda.
		"toutes les minutes":       "* * * * *",
		"toutes les cinq minutes":  "*/5 * * * *",
		"toutes les heures":        "0 * * * *",
		"plage d'heures":           "0 9-17 * * *",
		"pas assez de champs":      "0 20 * *",
		"champ non numérique":      "0 20 * * lundi",
		"valeur hors bornes":       "0 25 * * *",
		"jour de semaine invalide": "0 20 * * 9",
	}

	for name, expr := range refused {
		if _, err := cronToRRule(expr, testStart); err == nil {
			t.Errorf("%s (%q) devrait être refusé", name, expr)
		}
	}
}
