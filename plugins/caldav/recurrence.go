package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// Traduction entre le dialecte de récurrence de l'hôte — une expression
// cron standard à 5 champs — et la RRULE d'iCalendar.
//
// Les deux langages ne se recouvrent pas. Le contrat du magasin d'événements
// le prévoit : ce qui n'est pas exprimable est REFUSÉ, jamais approché. Un
// rappel hebdomadaire rangé comme quotidien serait un mensonge tranquille,
// découvert par la personne le jour où elle recevrait six rappels de trop.
//
// L'expression cron d'origine est aussi conservée telle quelle dans
// l'événement (X-AUTOMATA-CRON) : la RRULE sert les autres clients de
// l'agenda, la propriété sert l'aller-retour exact avec l'hôte.

// weekdays fait correspondre les jours cron (0 = dimanche, 7 aussi) aux
// jours d'iCalendar.
var weekdays = map[int]rrule.Weekday{
	0: rrule.SU, 1: rrule.MO, 2: rrule.TU, 3: rrule.WE,
	4: rrule.TH, 5: rrule.FR, 6: rrule.SA, 7: rrule.SU,
}

// cronToRRule traduit une expression cron en règle de récurrence ancrée
// sur start, qui est la première occurrence — l'hôte l'a déjà calculée.
func cronToRRule(expr string, start time.Time) (*rrule.ROption, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("expression cron à %d champs, 5 attendus", len(fields))
	}

	minute, err := singleValue(fields[0], 0, 59, "minutes")
	if err != nil {
		return nil, err
	}
	hour, err := singleValue(fields[1], 0, 23, "heures")
	if err != nil {
		return nil, err
	}

	monthDays, err := valueList(fields[2], 1, 31, "jours du mois")
	if err != nil {
		return nil, err
	}
	months, err := valueList(fields[3], 1, 12, "mois")
	if err != nil {
		return nil, err
	}
	weekDays, err := valueList(fields[4], 0, 7, "jours de la semaine")
	if err != nil {
		return nil, err
	}

	// Piège sémantique réel : quand cron restreint À LA FOIS le jour du
	// mois et le jour de la semaine, il déclenche sur l'UN OU L'AUTRE ;
	// iCalendar, lui, croise les deux. Traduire donnerait une règle plus
	// étroite que demandée, donc des rappels manquants — refuser est la
	// seule réponse honnête.
	if len(monthDays) > 0 && len(weekDays) > 0 {
		return nil, fmt.Errorf("jour du mois et jour de la semaine ne peuvent pas être fixés ensemble : cron déclenche sur l'un ou l'autre, un agenda sur les deux à la fois")
	}

	opt := &rrule.ROption{
		Dtstart:  start,
		Byhour:   []int{hour},
		Byminute: []int{minute},
		Bysecond: []int{0},
	}

	switch {
	case len(weekDays) > 0:
		opt.Freq = rrule.WEEKLY
		for _, day := range weekDays {
			opt.Byweekday = append(opt.Byweekday, weekdays[day])
		}
		if len(months) > 0 {
			opt.Bymonth = months
		}
	case len(monthDays) > 0:
		opt.Bymonthday = monthDays
		if len(months) > 0 {
			opt.Freq = rrule.YEARLY
			opt.Bymonth = months
		} else {
			opt.Freq = rrule.MONTHLY
		}
	case len(months) > 0:
		opt.Freq = rrule.YEARLY
		opt.Bymonth = months
		opt.Bymonthday = []int{start.Day()}
	default:
		opt.Freq = rrule.DAILY
	}

	return opt, nil
}

// singleValue lit un champ qui doit désigner UNE valeur. Un rappel se pose
// à une heure, pas dans un intervalle : « toutes les minutes » n'est pas
// une entrée d'agenda, et l'accepter remplirait le calendrier de la
// personne.
func singleValue(field string, min, max int, label string) (int, error) {
	if field == "*" || strings.ContainsAny(field, ",-/") {
		return 0, fmt.Errorf("le champ des %s doit désigner une valeur unique (%q)", label, field)
	}
	return parseBounded(field, min, max, label)
}

// valueList lit un champ « * » (aucune restriction, liste vide) ou une
// liste de valeurs séparées par des virgules.
func valueList(field string, min, max int, label string) ([]int, error) {
	if field == "*" {
		return nil, nil
	}
	if strings.ContainsAny(field, "-/") {
		return nil, fmt.Errorf("le champ des %s n'accepte que des valeurs séparées par des virgules (%q)", label, field)
	}

	var values []int
	for _, part := range strings.Split(field, ",") {
		value, err := parseBounded(part, min, max, label)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func parseBounded(raw string, min, max int, label string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("le champ des %s n'est pas un nombre (%q)", label, raw)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("le champ des %s est hors bornes (%d, attendu entre %d et %d)", label, value, min, max)
	}
	return value, nil
}
