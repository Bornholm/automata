package main

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

var buildNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func wrap(cal *ical.Calendar) caldav.CalendarObject {
	return caldav.CalendarObject{Path: "/cal/x.ics", Data: cal}
}

// Aller-retour d'un rappel ponctuel : ce qui entre doit ressortir
// identique, sinon l'hôte relit autre chose que ce qu'il a écrit.
func TestReminderRoundTrip(t *testing.T) {
	fireAt := time.Date(2026, 8, 24, 18, 30, 0, 0, time.UTC)

	cal, uid, err := buildReminder(reminderEvent{
		Text:   "Sortir les poubelles",
		FireAt: fireAt,
	}, buildNow)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	rem, ok := parseReminder(wrap(cal), buildNow)
	if !ok {
		t.Fatal("l'événement construit n'est pas relu comme rappel")
	}
	if rem.UID != uid {
		t.Errorf("uid relu %q, attendu %q", rem.UID, uid)
	}
	if rem.Text != "Sortir les poubelles" {
		t.Errorf("texte relu %q", rem.Text)
	}
	if !rem.FireAt.Equal(fireAt) {
		t.Errorf("échéance relue %s, attendu %s", rem.FireAt, fireAt)
	}
}

// Une série conserve son expression cron d'origine : la RRULE sert les
// autres clients de l'agenda, la propriété d'extension sert l'aller-retour
// exact avec l'hôte. Re-déduire du cron depuis une RRULE serait
// approximatif là où la copie est fidèle.
func TestReminderRoundTrip_KeepsTheCronExpression(t *testing.T) {
	fireAt := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)

	cal, _, err := buildReminder(reminderEvent{
		Text:       "Arroser les plantes",
		FireAt:     fireAt,
		Recurrence: "0 20 * * 2",
		Timezone:   "Europe/Paris",
	}, buildNow)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	rem, ok := parseReminder(wrap(cal), buildNow)
	if !ok {
		t.Fatal("la série n'est pas relue comme rappel")
	}
	if rem.Recurrence != "0 20 * * 2" {
		t.Errorf("récurrence relue %q", rem.Recurrence)
	}
	if rem.Timezone != "Europe/Paris" {
		t.Errorf("fuseau relu %q", rem.Timezone)
	}

	// La RRULE doit être là pour les autres clients, sinon l'événement ne
	// se répéterait que dans Automata.
	if cal.Events()[0].Props.Get(ical.PropRecurrenceRule) == nil {
		t.Error("aucune RRULE : l'agenda ne répéterait pas l'événement")
	}
}

// Pour une série, l'échéance rendue à l'hôte est la PROCHAINE occurrence,
// pas la date de départ — qui peut remonter à des mois.
func TestParseReminder_ReportsTheNextOccurrence(t *testing.T) {
	start := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC) // un mardi
	cal, _, err := buildReminder(reminderEvent{
		Text:       "Arroser les plantes",
		FireAt:     start,
		Recurrence: "0 20 * * 2",
	}, buildNow)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	// Trois semaines plus tard : la date de départ est loin derrière.
	after := start.AddDate(0, 0, 21)
	rem, ok := parseReminder(wrap(cal), after)
	if !ok {
		t.Fatal("série non relue")
	}
	if !rem.FireAt.After(after) {
		t.Errorf("échéance rendue %s, attendue après %s", rem.FireAt, after)
	}
	if rem.FireAt.Weekday() != time.Tuesday {
		t.Errorf("occurrence un %s, attendu mardi", rem.FireAt.Weekday())
	}
}

// LA garantie du magasin : un vrai rendez-vous n'est pas un rappel. Sans
// ce filtre, list_reminders déballerait tout l'agenda et cancel_reminder
// pourrait effacer une réunion — ce qui ne se rattrape pas.
func TestParseReminder_IgnoresEventsWeDidNotCreate(t *testing.T) {
	cal, _, err := buildEvent("Réunion d'équipe", time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC), time.Hour, buildNow)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	if _, ok := parseReminder(wrap(cal), buildNow); ok {
		t.Error("un rendez-vous ordinaire a été pris pour un rappel de l'assistant")
	}

	// Il reste néanmoins visible des outils de lecture : l'agenda entier
	// se consulte, seule sa suppression est cloisonnée.
	line, ok := describeEvent(wrap(cal))
	if !ok || !strings.Contains(line, "Réunion d'équipe") {
		t.Errorf("le rendez-vous devrait rester lisible: %q", line)
	}
	if strings.Contains(line, "created by the assistant") {
		t.Error("un rendez-vous ordinaire est annoncé comme créé par l'assistant")
	}
}

// Une récurrence intraduisible est refusée à la construction : rien n'est
// écrit dans l'agenda, et l'hôte rend le refus à la personne.
func TestBuildReminder_RefusesUntranslatableRecurrence(t *testing.T) {
	_, _, err := buildReminder(reminderEvent{
		Text:       "Trop souvent",
		FireAt:     buildNow.Add(time.Hour),
		Recurrence: "*/5 * * * *",
	}, buildNow)
	if err == nil {
		t.Fatal("une récurrence intraduisible devrait être refusée")
	}
	if !strings.Contains(err.Error(), "cannot be stored") {
		t.Errorf("message de refus peu explicite: %v", err)
	}
}

// Réécrire le même identifiant met à jour au lieu de dupliquer : c'est ce
// qui rend l'écriture rejouable, et une confirmation peut l'être.
func TestBuildReminder_KeepsTheGivenIdentifier(t *testing.T) {
	cal, uid, err := buildReminder(reminderEvent{
		UID:    "automata-connu",
		Text:   "Sortir les poubelles",
		FireAt: buildNow.Add(time.Hour),
	}, buildNow)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	if uid != "automata-connu" {
		t.Errorf("uid %q, attendu celui fourni", uid)
	}

	rem, ok := parseReminder(wrap(cal), buildNow)
	if !ok || rem.UID != "automata-connu" {
		t.Errorf("uid relu %q", rem.UID)
	}
}
