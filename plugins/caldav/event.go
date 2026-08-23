package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/google/uuid"
	"github.com/teambition/rrule-go"
)

// Traduction entre un rappel de l'hôte et un VEVENT d'agenda.
//
// Deux propriétés d'extension marquent nos événements :
//
//   - X-AUTOMATA-REMINDER identifie ce que l'assistant a créé. Le magasin
//     d'événements ne liste et ne supprime QUE cela : sans ce filtre,
//     list_reminders déballerait tout l'agenda de la personne et
//     cancel_reminder pourrait effacer une vraie réunion. Une suppression
//     malencontreuse ne se rattrape pas.
//   - X-AUTOMATA-CRON conserve l'expression de récurrence d'origine. La
//     RRULE sert les autres clients de l'agenda, cette propriété sert
//     l'aller-retour exact avec l'hôte : re-déduire du cron depuis une
//     RRULE serait approximatif là où la copie est fidèle.
const (
	propReminderMarker = "X-AUTOMATA-REMINDER"
	propCronExpression = "X-AUTOMATA-CRON"
	propTimezoneName   = "X-AUTOMATA-TIMEZONE"
)

// reminderDuration est la durée donnée aux événements créés comme
// rappels. Un événement sans durée s'affiche mal dans la plupart des
// clients ; un quart d'heure se lit comme un pense-bête et non comme un
// créneau bloqué.
const reminderDuration = 15 * time.Minute

// reminderEvent est un rappel tel que le plugin le manipule.
type reminderEvent struct {
	UID        string
	Text       string
	FireAt     time.Time
	Recurrence string
	Timezone   string
}

// buildReminder fabrique le VCALENDAR d'un rappel. Un UID vide en fait
// naître un : le chemin de l'objet en dérive, donc réécrire le même UID
// met à jour au lieu de dupliquer.
func buildReminder(ev reminderEvent, now time.Time) (*ical.Calendar, string, error) {
	uid := ev.UID
	if uid == "" {
		uid = "automata-" + uuid.NewString()
	}

	loc := time.UTC
	if ev.Timezone != "" {
		if parsed, err := time.LoadLocation(ev.Timezone); err == nil {
			loc = parsed
		}
	}
	start := ev.FireAt.In(loc)

	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())
	event.Props.SetText(ical.PropSummary, ev.Text)
	event.Props.SetDateTime(ical.PropDateTimeStart, start)
	event.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(reminderDuration))
	event.Props.SetText(propReminderMarker, "1")

	if ev.Recurrence != "" {
		option, err := cronToRRule(ev.Recurrence, start)
		if err != nil {
			// Refus explicite plutôt qu'approximation : le contrat du
			// magasin le prévoit, et une règle « à peu près » se paie en
			// rappels manquants ou en rappels de trop.
			return nil, "", fmt.Errorf("this repetition cannot be stored in a calendar: %w", err)
		}
		event.Props.SetRecurrenceRule(option)
		event.Props.SetText(propCronExpression, ev.Recurrence)
		if ev.Timezone != "" {
			event.Props.SetText(propTimezoneName, ev.Timezone)
		}
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//Automata//CalDAV plugin//FR")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, event.Component)

	return cal, uid, nil
}

// parseReminder relit un objet d'agenda comme rappel. ok faux désigne un
// événement qui n'est pas de nous — une vraie réunion, typiquement — ou
// un objet illisible.
func parseReminder(object caldav.CalendarObject, after time.Time) (reminderEvent, bool) {
	if object.Data == nil {
		return reminderEvent{}, false
	}

	for _, event := range object.Data.Events() {
		if marker, err := event.Props.Text(propReminderMarker); err != nil || marker == "" {
			continue
		}

		uid, err := event.Props.Text(ical.PropUID)
		if err != nil || uid == "" {
			continue
		}
		text, err := event.Props.Text(ical.PropSummary)
		if err != nil {
			continue
		}
		start, err := event.DateTimeStart(time.UTC)
		if err != nil {
			continue
		}

		cron, _ := event.Props.Text(propCronExpression)
		zone, _ := event.Props.Text(propTimezoneName)

		// Pour une série, l'échéance qui intéresse l'hôte est la
		// prochaine occurrence, pas la date de départ — qui peut remonter
		// à des mois.
		fireAt := start
		if cron != "" {
			if next, ok := nextOccurrence(event, start, after); ok {
				fireAt = next
			}
		}

		return reminderEvent{
			UID:        uid,
			Text:       text,
			FireAt:     fireAt.UTC(),
			Recurrence: cron,
			Timezone:   zone,
		}, true
	}

	return reminderEvent{}, false
}

// nextOccurrence donne la première occurrence strictement postérieure à
// after, en s'appuyant sur la RRULE portée par l'événement.
func nextOccurrence(event ical.Event, start, after time.Time) (time.Time, bool) {
	option, err := event.Props.RecurrenceRule()
	if err != nil || option == nil {
		return time.Time{}, false
	}
	option.Dtstart = start

	rule, err := rrule.NewRRule(*option)
	if err != nil {
		return time.Time{}, false
	}

	next := rule.After(after, false)
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// describeEvent rend une ligne lisible par le modèle pour un objet
// quelconque de l'agenda — rappel de l'assistant ou rendez-vous saisi
// ailleurs.
func describeEvent(object caldav.CalendarObject) (string, bool) {
	if object.Data == nil {
		return "", false
	}

	for _, event := range object.Data.Events() {
		uid, err := event.Props.Text(ical.PropUID)
		if err != nil {
			continue
		}
		summary, err := event.Props.Text(ical.PropSummary)
		if err != nil || strings.TrimSpace(summary) == "" {
			summary = "(no title)"
		}
		start, err := event.DateTimeStart(time.UTC)
		if err != nil {
			continue
		}

		line := fmt.Sprintf("id=%s | start=%s | title=%s", uid, start.Format("2006-01-02 15:04"), summary)
		if marker, err := event.Props.Text(propReminderMarker); err == nil && marker != "" {
			line += " | created by the assistant"
		}
		return line, true
	}

	return "", false
}

// buildEvent fabrique un rendez-vous ordinaire — créé par l'assistant sur
// demande explicite, donc SANS le marqueur de rappel : il n'a rien à faire
// dans le magasin des rappels, et cancel_reminder ne doit pas pouvoir
// l'effacer.
func buildEvent(title string, start time.Time, duration time.Duration, now time.Time) (*ical.Calendar, string, error) {
	if duration <= 0 {
		return nil, "", fmt.Errorf("the duration of an event must be positive")
	}
	uid := "automata-" + uuid.NewString()

	event := ical.NewEvent()
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())
	event.Props.SetText(ical.PropSummary, title)
	event.Props.SetDateTime(ical.PropDateTimeStart, start)
	event.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(duration))

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//Automata//CalDAV plugin//FR")
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, event.Component)

	return cal, uid, nil
}
