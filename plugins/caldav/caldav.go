package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// Accès au serveur CalDAV : ouverture d'une session, résolution de
// l'agenda du membre, et les quatre opérations dont le reste du plugin a
// besoin — lister une fenêtre, lire, écrire, supprimer.

// dialTimeout borne un aller-retour vers le serveur d'agenda. Un serveur
// lent ne doit pas suspendre le tour de conversation qui l'interroge.
const dialTimeout = 20 * time.Second

// session est un client lié à un agenda précis.
type session struct {
	client   *caldav.Client
	calendar string
}

// dial ouvre une session sur l'agenda du membre. Le chemin de collection
// est celui qu'il a choisi ; sans choix, le premier agenda publié par le
// serveur sert par défaut — ce qui est le bon comportement pour la grande
// majorité des comptes, qui n'en ont qu'un.
func dial(ctx context.Context, cfg memberConfig, password string) (*session, error) {
	base := &http.Client{
		Timeout:   dialTimeout,
		Transport: newTransport(cfg),
	}
	httpClient := webdav.HTTPClientWithBasicAuth(base, cfg.Username, password)

	client, err := caldav.NewClient(httpClient, cfg.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("the calendar server address is not usable: %w", err)
	}

	if cfg.CalendarPath != "" {
		return &session{client: client, calendar: cfg.CalendarPath}, nil
	}

	calendars, err := discoverCalendars(ctx, client)
	if err != nil {
		return nil, err
	}
	if len(calendars) == 0 {
		return nil, fmt.Errorf("no calendar found on this account")
	}

	return &session{client: client, calendar: calendars[0].Path}, nil
}

// probeServer éprouve réellement la connexion : une session, puis un
// aller-retour qui exige que le serveur réponde et que l'authentification
// passe. Réservé au bouton « Tester la connexion », qui doit constater et
// non supposer — les opérations courantes, elles, n'ont pas à payer cet
// aller-retour à chaque appel.
func probeServer(ctx context.Context, cfg memberConfig, password string) error {
	sess, err := dial(ctx, cfg, password)
	if err != nil {
		return err
	}

	// La découverte du principal est le plus petit échange qui prouve les
	// trois choses que la personne veut savoir : le serveur répond, le
	// certificat est accepté, les identifiants passent.
	if _, err := sess.client.FindCurrentUserPrincipal(ctx); err != nil {
		return fmt.Errorf("the server refused the connection (check the address and the credentials): %w", err)
	}

	return nil
}

// discoverCalendars remonte du principal à l'ensemble des agendas du
// compte. C'est la séquence prescrite par CalDAV, et la seule qui marche
// sur les serveurs où l'URL saisie n'est pas déjà celle d'une collection.
func discoverCalendars(ctx context.Context, client *caldav.Client) ([]caldav.Calendar, error) {
	principal, err := client.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("the server refused the connection (check the address and the credentials): %w", err)
	}

	homeSet, err := client.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("could not locate the calendars of this account: %w", err)
	}

	calendars, err := client.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("could not list the calendars of this account: %w", err)
	}

	// Un agenda qui n'accepte pas les VEVENT ne nous sert à rien : les
	// listes de tâches et les carnets d'anniversaires vivent souvent dans
	// le même compte.
	var usable []caldav.Calendar
	for _, cal := range calendars {
		if len(cal.SupportedComponentSet) == 0 || containsComponent(cal.SupportedComponentSet, ical.CompEvent) {
			usable = append(usable, cal)
		}
	}
	return usable, nil
}

func containsComponent(set []string, name string) bool {
	for _, component := range set {
		if strings.EqualFold(component, name) {
			return true
		}
	}
	return false
}

// query retourne les objets de l'agenda dont une occurrence tombe dans la
// fenêtre [from, to). Un to nul demande tout ce qui suit from.
//
// Le filtre est posé côté serveur : rapatrier l'agenda entier pour le
// trier ici serait lent, et sur un agenda de plusieurs années, coûteux
// pour le serveur de quelqu'un d'autre.
func (s *session) query(ctx context.Context, from, to time.Time) ([]caldav.CalendarObject, error) {
	if to.IsZero() {
		to = from.AddDate(2, 0, 0)
	}

	objects, err := s.client.QueryCalendar(ctx, s.calendar, &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			AllComps: true,
		},
		CompFilter: caldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []caldav.CompFilter{{
				Name:  ical.CompEvent,
				Start: from.UTC(),
				End:   to.UTC(),
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("the calendar could not be read: %w", err)
	}
	return objects, nil
}

// put écrit un objet à un chemin dérivé de son UID. Le chemin est stable :
// réécrire le même UID met à jour l'événement au lieu d'en créer un
// second, ce qui rend l'opération rejouable — la confirmation d'une
// écriture peut l'être.
func (s *session) put(ctx context.Context, uid string, cal *ical.Calendar) error {
	if _, err := s.client.PutCalendarObject(ctx, s.objectPath(uid), cal); err != nil {
		return fmt.Errorf("the event could not be written to the calendar: %w", err)
	}
	return nil
}

// remove supprime un objet. found faux désigne un événement absent — pas
// une erreur : l'annuler deux fois donne le même résultat.
func (s *session) remove(ctx context.Context, uid string) (bool, error) {
	path := s.objectPath(uid)

	if _, err := s.client.GetCalendarObject(ctx, path); err != nil {
		return false, nil
	}
	if err := s.client.RemoveAll(ctx, path); err != nil {
		return false, fmt.Errorf("the event could not be removed from the calendar: %w", err)
	}
	return true, nil
}

func (s *session) objectPath(uid string) string {
	return strings.TrimSuffix(s.calendar, "/") + "/" + uid + ".ics"
}
