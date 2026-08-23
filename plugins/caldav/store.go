package main

import (
	"context"
	"time"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Magasin d'événements : les trois RPC par lesquelles l'hôte range les
// rappels d'un membre dans son agenda plutôt que dans sa propre table.
//
// La règle qui gouverne tout ce fichier : ce magasin ne voit QUE les
// événements portant le marqueur de rappel. L'agenda de quelqu'un contient
// des réunions, des anniversaires, des rendez-vous médicaux ; les exposer
// comme des rappels annulables ferait de cancel_reminder une porte ouverte
// sur la suppression d'une vraie réunion. Une suppression ne se rattrape
// pas — le filtre n'est pas une commodité, c'est la garantie.

// PutEvent implémente proto.AutomataPluginServer.
func (p *Plugin) PutEvent(ctx context.Context, in *proto.PutEventRequest) (*proto.PutEventResponse, error) {
	if in.Ctx == nil || in.Event == nil {
		return &proto.PutEventResponse{IsError: true, ErrorText: "incomplete request"}, nil
	}

	_, sess, errText := p.session(ctx, in.Ctx)
	if errText != "" {
		return &proto.PutEventResponse{IsError: true, ErrorText: errText}, nil
	}

	cal, uid, err := buildReminder(reminderEvent{
		UID:        in.Event.Id,
		Text:       in.Event.Text,
		FireAt:     time.Unix(in.Event.FireAtUnix, 0).UTC(),
		Recurrence: in.Event.Recurrence,
		Timezone:   in.Event.Timezone,
	}, p.now())
	if err != nil {
		return &proto.PutEventResponse{IsError: true, ErrorText: err.Error()}, nil
	}

	if err := sess.put(ctx, uid, cal); err != nil {
		return &proto.PutEventResponse{IsError: true, ErrorText: err.Error()}, nil
	}

	return &proto.PutEventResponse{Id: uid}, nil
}

// DeleteEvent implémente proto.AutomataPluginServer. Un identifiant qui ne
// désigne pas un rappel de l'assistant est traité comme inconnu, jamais
// supprimé : c'est le filet qui empêche l'annulation d'une vraie réunion.
func (p *Plugin) DeleteEvent(ctx context.Context, in *proto.DeleteEventRequest) (*proto.DeleteEventResponse, error) {
	if in.Ctx == nil || in.Id == "" {
		return &proto.DeleteEventResponse{IsError: true, ErrorText: "incomplete request"}, nil
	}

	_, sess, errText := p.session(ctx, in.Ctx)
	if errText != "" {
		return &proto.DeleteEventResponse{IsError: true, ErrorText: errText}, nil
	}

	if !p.isReminder(ctx, sess, in.Id) {
		return &proto.DeleteEventResponse{Found: false}, nil
	}

	found, err := sess.remove(ctx, in.Id)
	if err != nil {
		return &proto.DeleteEventResponse{IsError: true, ErrorText: err.Error()}, nil
	}
	return &proto.DeleteEventResponse{Found: found}, nil
}

// ListEvents implémente proto.AutomataPluginServer.
func (p *Plugin) ListEvents(ctx context.Context, in *proto.ListEventsRequest) (*proto.ListEventsResponse, error) {
	if in.Ctx == nil {
		return &proto.ListEventsResponse{IsError: true, ErrorText: "incomplete request"}, nil
	}

	_, sess, errText := p.session(ctx, in.Ctx)
	if errText != "" {
		return &proto.ListEventsResponse{IsError: true, ErrorText: errText}, nil
	}

	from := time.Unix(in.FromUnix, 0).UTC()
	var to time.Time
	if in.ToUnix > 0 {
		to = time.Unix(in.ToUnix, 0).UTC()
	}

	reminders, err := p.reminders(ctx, sess, from, to)
	if err != nil {
		return &proto.ListEventsResponse{IsError: true, ErrorText: err.Error()}, nil
	}

	events := make([]*proto.ScheduledEvent, 0, len(reminders))
	for _, rem := range reminders {
		events = append(events, &proto.ScheduledEvent{
			Id:         rem.UID,
			Text:       rem.Text,
			FireAtUnix: rem.FireAt.Unix(),
			Recurrence: rem.Recurrence,
			Timezone:   rem.Timezone,
		})
	}

	return &proto.ListEventsResponse{Events: events}, nil
}

// reminders liste les rappels de l'assistant dans la fenêtre demandée.
func (p *Plugin) reminders(ctx context.Context, sess *session, from, to time.Time) ([]reminderEvent, error) {
	objects, err := sess.query(ctx, from, to)
	if err != nil {
		return nil, err
	}

	var reminders []reminderEvent
	for _, object := range objects {
		if rem, ok := parseReminder(object, from); ok {
			reminders = append(reminders, rem)
		}
	}
	return reminders, nil
}

// isReminder dit si l'identifiant désigne bien un rappel de l'assistant.
// Une lecture en échec répond non : mieux vaut refuser une annulation
// légitime que supprimer un rendez-vous qui ne nous appartient pas.
func (p *Plugin) isReminder(ctx context.Context, sess *session, uid string) bool {
	object, err := sess.client.GetCalendarObject(ctx, sess.objectPath(uid))
	if err != nil || object == nil {
		return false
	}
	_, ok := parseReminder(*object, p.now().UTC())
	return ok
}
