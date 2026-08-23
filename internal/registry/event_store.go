package registry

import (
	"context"
	"time"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/plugin"
)

// Adaptation du gestionnaire de plugins au contrat
// agent.EventStoreResolver : quand un plugin actif tient le magasin
// d'événements d'un membre, ses rappels y sont rangés à la place de la
// table reminders. Même motif que pluginSpecialistProvider — le paquet
// agent ne connaît que l'interface, le registre fait la jonction.

type pluginEventStoreResolver struct {
	manager *plugin.Manager
	db      *persistence.DB
}

// newPluginEventStoreResolver construit le résolveur ; nil quand le
// système de plugins est désactivé, ce qui laisse les rappels dans la
// base — le comportement historique.
func newPluginEventStoreResolver(manager *plugin.Manager, db *persistence.DB) agent.EventStoreResolver {
	if manager == nil {
		return nil
	}
	return &pluginEventStoreResolver{manager: manager, db: db}
}

// EventStore implémente agent.EventStoreResolver.
func (r *pluginEventStoreResolver) EventStore(ctx context.Context, orgID, memberID string) agent.EventStore {
	name, ok := r.manager.EventStoreFor(ctx, r.db, orgID, memberID)
	if !ok {
		return nil
	}
	return &pluginEventStore{
		manager: r.manager,
		name:    name,
		callCtx: plugin.CallContext{OrgID: orgID, MemberID: memberID},
	}
}

// pluginEventStore est le magasin d'UN membre : la portée est figée à la
// résolution, donc aucun appel ne peut désigner quelqu'un d'autre, quoi
// que fasse l'appelant.
type pluginEventStore struct {
	manager *plugin.Manager
	name    string
	callCtx plugin.CallContext
}

func (s *pluginEventStore) Name() string { return s.name }

func (s *pluginEventStore) Put(ctx context.Context, event agent.ScheduledEvent) (string, error) {
	return s.manager.PutEvent(ctx, s.name, s.callCtx, plugin.ScheduledEvent{
		ID:         event.ID,
		Text:       event.Text,
		FireAt:     event.FireAt,
		Recurrence: event.Recurrence,
		Timezone:   event.Timezone,
	})
}

func (s *pluginEventStore) Delete(ctx context.Context, id string) (bool, error) {
	return s.manager.DeleteEvent(ctx, s.name, s.callCtx, id)
}

func (s *pluginEventStore) List(ctx context.Context, from, to time.Time) ([]agent.ScheduledEvent, error) {
	events, err := s.manager.ListEvents(ctx, s.name, s.callCtx, from, to)
	if err != nil {
		return nil, err
	}

	converted := make([]agent.ScheduledEvent, 0, len(events))
	for _, ev := range events {
		converted = append(converted, agent.ScheduledEvent{
			ID:         ev.ID,
			Text:       ev.Text,
			FireAt:     ev.FireAt,
			Recurrence: ev.Recurrence,
			Timezone:   ev.Timezone,
		})
	}
	return converted, nil
}
