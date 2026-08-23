package agent_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/persistence"
)

// fakeEventStore tient lieu de magasin fourni par un plugin (un agenda
// CalDAV, en production). Il enregistre ce qu'on lui demande pour que les
// tests vérifient où le rappel est réellement parti.
type fakeEventStore struct {
	events  []agent.ScheduledEvent
	nextID  int
	putErr  error
	listErr error
	puts    int
	deletes int
}

func (f *fakeEventStore) Name() string { return "agenda" }

func (f *fakeEventStore) Put(_ context.Context, event agent.ScheduledEvent) (string, error) {
	f.puts++
	if f.putErr != nil {
		return "", f.putErr
	}
	f.nextID++
	event.ID = fmt.Sprintf("ev-%d", f.nextID)
	f.events = append(f.events, event)
	return event.ID, nil
}

func (f *fakeEventStore) Delete(_ context.Context, id string) (bool, error) {
	f.deletes++
	for i, ev := range f.events {
		if ev.ID == id {
			f.events = append(f.events[:i], f.events[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeEventStore) List(_ context.Context, from, to time.Time) ([]agent.ScheduledEvent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var window []agent.ScheduledEvent
	for _, ev := range f.events {
		if ev.FireAt.Before(from) {
			continue
		}
		if !to.IsZero() && !ev.FireAt.Before(to) {
			continue
		}
		window = append(window, ev)
	}
	return window, nil
}

// staticResolver rend le même magasin pour tout membre.
type staticResolver struct{ store agent.EventStore }

func (r staticResolver) EventStore(context.Context, string, string) agent.EventStore {
	return r.store
}

func newReminderToolsWithStore(t *testing.T, store agent.EventStore) (agent.ReminderTools, *persistence.DB) {
	t.Helper()

	db := reminderTestDB(t)

	return agent.ReminderTools{
		DB:         db,
		Repo:       persistence.NewReminderRepository(nil),
		Authorizer: authorization.NewAuthorizer(reminderTestConfig()),
		Now:        func() time.Time { return reminderTestNow },
		Events:     staticResolver{store: store},
	}, db
}

// Un membre dont un plugin tient le magasin ne doit RIEN écrire dans la
// table : deux magasins pour la même personne, c'est un rappel qui part
// deux fois ou pas du tout.
func TestCreateReminder_GoesToTheEventStoreInsteadOfTheTable(t *testing.T) {
	store := &fakeEventStore{}
	tools, db := newReminderToolsWithStore(t, store)
	identity := privateIdentity("alice")

	result := executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"message": "Sortir les poubelles",
		"fire_at": reminderTestNow.Add(2 * time.Hour).Format(time.RFC3339),
	})

	if !strings.Contains(result, "Reminder scheduled") {
		t.Fatalf("création refusée: %q", result)
	}
	if len(store.events) != 1 {
		t.Fatalf("%d événement(s) dans le magasin, attendu 1", len(store.events))
	}
	if store.events[0].Text != "Sortir les poubelles" {
		t.Errorf("texte enregistré %q", store.events[0].Text)
	}
	if rows := listReminderRows(t, db, string(identity.ConversationID)); len(rows) != 0 {
		t.Errorf("%d rappel(s) écrit(s) en base alors qu'un magasin délégué existe", len(rows))
	}

	// L'identifiant rendu au modèle est celui du magasin : c'est lui
	// qu'il redonnera à cancel_reminder.
	if !strings.Contains(result, store.events[0].ID) {
		t.Errorf("l'identifiant du magasin n'est pas rendu au modèle: %q", result)
	}
}

// Une récurrence traverse le contrat en cron, dialecte de l'hôte : c'est
// au magasin de traduire vers le format de son moteur.
func TestCreateReminder_EventStoreReceivesTheCronRecurrence(t *testing.T) {
	store := &fakeEventStore{}
	tools, _ := newReminderToolsWithStore(t, store)

	result := executeReminderTool(t, tools, privateIdentity("alice"), "create_reminder", map[string]any{
		"message":    "Arroser les plantes",
		"recurrence": "0 20 * * 2",
		"timezone":   "Europe/Paris",
	})

	if !strings.Contains(result, "Repeating reminder scheduled") {
		t.Fatalf("création récurrente refusée: %q", result)
	}
	if len(store.events) != 1 {
		t.Fatalf("%d événement(s), attendu 1", len(store.events))
	}
	if got := store.events[0].Recurrence; got != "0 20 * * 2" {
		t.Errorf("récurrence transmise %q", got)
	}
	if got := store.events[0].Timezone; got != "Europe/Paris" {
		t.Errorf("fuseau transmis %q", got)
	}
}

// Un magasin en panne refuse franchement. Le repli silencieux sur la base
// éparpillerait les rappels entre deux magasins sans que personne ne le
// sache : c'est le pire des deux mondes.
func TestCreateReminder_EventStoreFailureNeverFallsBackToTheTable(t *testing.T) {
	store := &fakeEventStore{putErr: fmt.Errorf("agenda injoignable")}
	tools, db := newReminderToolsWithStore(t, store)
	identity := privateIdentity("alice")

	result := executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"message": "Sortir les poubelles",
		"fire_at": reminderTestNow.Add(2 * time.Hour).Format(time.RFC3339),
	})

	if !strings.Contains(result, "could not create") {
		t.Errorf("l'échec n'est pas rendu au modèle: %q", result)
	}
	if rows := listReminderRows(t, db, string(identity.ConversationID)); len(rows) != 0 {
		t.Errorf("repli sur la base malgré l'échec du magasin: %d rappel(s)", len(rows))
	}
}

// Même garde-fou qu'en base : l'assistant qui reformule sa réponse au tour
// suivant rappelle l'outil, et la personne recevait deux fois le rappel.
func TestCreateReminder_EventStoreDeduplicates(t *testing.T) {
	store := &fakeEventStore{}
	tools, _ := newReminderToolsWithStore(t, store)
	identity := privateIdentity("alice")

	args := map[string]any{
		"message": "Sortir les poubelles",
		"fire_at": reminderTestNow.Add(2 * time.Hour).Format(time.RFC3339),
	}

	executeReminderTool(t, tools, identity, "create_reminder", args)
	second := executeReminderTool(t, tools, identity, "create_reminder", args)

	if !strings.Contains(second, "already exists") {
		t.Errorf("doublon non détecté: %q", second)
	}
	if len(store.events) != 1 {
		t.Errorf("%d événement(s) dans le magasin, attendu 1", len(store.events))
	}
}

// Les rappels créés en base AVANT le branchement de l'agenda vont bel et
// bien partir : les cacher les rendrait invisibles et inannulables.
func TestListReminders_ShowsBothStores(t *testing.T) {
	store := &fakeEventStore{}
	tools, _ := newReminderToolsWithStore(t, store)
	identity := privateIdentity("alice")

	// Un rappel « historique », écrit en base sans passer par le magasin.
	legacy := tools
	legacy.Events = nil
	executeReminderTool(t, legacy, identity, "create_reminder", map[string]any{
		"message": "Rappel historique",
		"fire_at": reminderTestNow.Add(time.Hour).Format(time.RFC3339),
	})

	executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"message": "Rappel de l'agenda",
		"fire_at": reminderTestNow.Add(2 * time.Hour).Format(time.RFC3339),
	})

	listing := executeReminderTool(t, tools, identity, "list_reminders", map[string]any{})

	for _, want := range []string{"Rappel historique", "Rappel de l'agenda"} {
		if !strings.Contains(listing, want) {
			t.Errorf("%q absent de la liste: %q", want, listing)
		}
	}
	// Numérotation continue : deux séries « 1. » se suivant dans la même
	// liste laisseraient croire à deux listes.
	if strings.Count(listing, "1. ") != 1 {
		t.Errorf("numérotation dupliquée: %q", listing)
	}
}

// L'identifiant décide du magasin. Un rappel d'avant le branchement reste
// annulable, un événement de l'agenda l'est aussi, et le magasin distant
// n'est même pas interrogé quand la base a répondu.
func TestCancelReminder_RoutesByIdentifier(t *testing.T) {
	store := &fakeEventStore{}
	tools, _ := newReminderToolsWithStore(t, store)
	identity := privateIdentity("alice")

	legacy := tools
	legacy.Events = nil
	executeReminderTool(t, legacy, identity, "create_reminder", map[string]any{
		"message": "Rappel historique",
		"fire_at": reminderTestNow.Add(time.Hour).Format(time.RFC3339),
	})
	rows := listReminderRows(t, tools.DB, string(identity.ConversationID))
	if len(rows) != 1 {
		t.Fatalf("préparation: %d rappel(s) en base", len(rows))
	}

	executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"message": "Rappel de l'agenda",
		"fire_at": reminderTestNow.Add(2 * time.Hour).Format(time.RFC3339),
	})
	if len(store.events) != 1 {
		t.Fatalf("préparation: %d événement(s) dans le magasin", len(store.events))
	}
	eventID := store.events[0].ID

	fromTable := executeReminderTool(t, tools, identity, "cancel_reminder", map[string]any{"id": string(rows[0].ID)})
	if !strings.Contains(fromTable, "cancelled") {
		t.Errorf("annulation en base refusée: %q", fromTable)
	}
	if store.deletes != 0 {
		t.Error("le magasin distant a été interrogé alors que la base avait répondu")
	}

	fromStore := executeReminderTool(t, tools, identity, "cancel_reminder", map[string]any{"id": eventID})
	if !strings.Contains(fromStore, "cancelled") {
		t.Errorf("annulation dans le magasin refusée: %q", fromStore)
	}
	if len(store.events) != 0 {
		t.Errorf("%d événement(s) restant(s) dans le magasin", len(store.events))
	}
}

// Un identifiant inconnu des deux magasins reste un identifiant inconnu :
// pas une erreur technique, une réponse que le modèle peut corriger.
func TestCancelReminder_UnknownIdentifierInBothStores(t *testing.T) {
	store := &fakeEventStore{}
	tools, _ := newReminderToolsWithStore(t, store)

	result := executeReminderTool(t, tools, privateIdentity("alice"), "cancel_reminder", map[string]any{"id": "inexistant"})
	if !strings.Contains(result, "no pending reminder") {
		t.Errorf("réponse inattendue: %q", result)
	}
}

// Une tâche planifiée n'est pas un événement d'agenda : sa consigne est
// une instruction donnée à un agent, pas quelque chose qu'on lit dans son
// calendrier. Elle reste en base même quand un magasin existe.
func TestScheduleTask_NeverGoesToTheEventStore(t *testing.T) {
	store := &fakeEventStore{}
	tools, db := newTaskTools(t)
	tools.Events = staticResolver{store: store}
	identity := privateIdentity("alice")

	executeReminderTool(t, tools, identity, "schedule_task", map[string]any{
		"instruction": "Préparer le bulletin météo",
		"fire_at":     reminderTestNow.Add(3 * time.Hour).Format(time.RFC3339),
	})

	if len(store.events) != 0 {
		t.Errorf("%d tâche(s) rangée(s) dans le magasin d'événements", len(store.events))
	}

	if tasks := listTaskRows(t, db, string(identity.ConversationID)); len(tasks) != 1 {
		t.Errorf("%d tâche(s) en base, attendu 1", len(tasks))
	}
}
