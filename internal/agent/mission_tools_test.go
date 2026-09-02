package agent_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

func newMissionTools(t *testing.T) (agent.ReminderTools, *persistence.DB) {
	t.Helper()

	tools, db := newTaskTools(t)
	tools.Missions = persistence.NewMissionRepository(nil)

	return tools, db
}

func insertMission(t *testing.T, db *persistence.DB, mission persistence.Mission) {
	t.Helper()

	repo := persistence.NewMissionRepository(nil)
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return repo.Insert(context.Background(), tx, mission)
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func findMission(t *testing.T, db *persistence.DB, id string) persistence.Mission {
	t.Helper()

	repo := persistence.NewMissionRepository(nil)
	var mission persistence.Mission
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var found bool
		var err error
		mission, found, err = repo.FindByID(context.Background(), tx, id)
		if err == nil && !found {
			t.Fatalf("mission %q introuvable", id)
		}
		return err
	}); err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	return mission
}

func baseMission(id string, identity model.ExecutionIdentity) persistence.Mission {
	return persistence.Mission{
		ID: id, OrgID: string(identity.OrgID), PrincipalID: string(identity.PrincipalID),
		ConversationID: string(identity.ConversationID), Provider: identity.Provider,
		ChannelID: identity.ChannelID, AgentID: "main",
		Title: "Dossier " + id, Objective: "Suivre le dossier " + id,
		Status:      persistence.MissionStatusActive,
		NextCheckAt: reminderTestNow.AddDate(0, 0, 7),
		CreatedAt:   reminderTestNow, UpdatedAt: reminderTestNow,
	}
}

// Une mission créée porte l'identité complète du tour courant : c'est elle
// qui décide où les réveils livrent leurs nouvelles et où les plans
// attendent leur « confirmer ».
func TestStartMission_PersistsConversationAndAgent(t *testing.T) {
	tools, db := newMissionTools(t)
	identity := privateIdentity("alice")

	result := executeReminderTool(t, tools, identity, "start_mission", map[string]any{
		"title":          "Réclamation parking",
		"objective":      "Follow the parking complaint, chase weekly without a reply.",
		"first_check_at": "2026-08-24T09:00:00+02:00",
	})

	if !strings.Contains(result, "Mission") || !strings.Contains(result, "started") {
		t.Fatalf("résultat = %q, attendu une confirmation d'ouverture", result)
	}

	id := result[strings.Index(result, "(id: ")+5 : strings.Index(result, ")")]
	mission := findMission(t, db, id)
	if mission.ConversationID != string(identity.ConversationID) || mission.PrincipalID != string(identity.PrincipalID) {
		t.Errorf("identité = (%q, %q), attendu celle du tour courant", mission.ConversationID, mission.PrincipalID)
	}
	if mission.AgentID != "main" {
		t.Errorf("agent = %q, attendu l'agent créateur figé", mission.AgentID)
	}
	if mission.Status != persistence.MissionStatusActive {
		t.Errorf("statut = %q", mission.Status)
	}
	// 9h à +02:00 = 07:00 UTC : l'échéance est normalisée en UTC.
	if got := mission.NextCheckAt.UTC().Format("2006-01-02T15:04:05Z"); got != "2026-08-24T07:00:00Z" {
		t.Errorf("premier réveil = %q, attendu 2026-08-24T07:00:00Z", got)
	}
}

// La permission reminder.* ne suffit pas : une mission fait TRAVAILLER
// l'agent, c'est le pouvoir task.*.
func TestStartMission_DeniedWithoutTaskPermission(t *testing.T) {
	tools, db := newMissionTools(t)
	identity := privateIdentity("leo") // rôle read-only : aucune permission task.*

	result := executeReminderTool(t, tools, identity, "start_mission", map[string]any{
		"title":          "Dossier",
		"objective":      "Follow something.",
		"first_check_at": "2026-08-24T09:00:00+02:00",
	})

	if !strings.Contains(result, "refused") {
		t.Fatalf("résultat = %q, attendu un refus", result)
	}

	repo := persistence.NewMissionRepository(nil)
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		count, err := repo.CountActiveByPrincipal(context.Background(), tx, string(identity.OrgID), string(identity.PrincipalID))
		if count != 0 {
			t.Errorf("missions persistées = %d malgré le refus", count)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Chaque mission active coûte un tour de modèle par réveil : la borne par
// personne refuse la création au-delà, avec la marche à suivre.
func TestStartMission_RefusesBeyondActiveLimit(t *testing.T) {
	tools, db := newMissionTools(t)
	identity := privateIdentity("alice")

	for i := 0; i < persistence.MaxActiveMissionsPerPrincipal; i++ {
		insertMission(t, db, baseMission(string(rune('a'+i)), identity))
	}

	result := executeReminderTool(t, tools, identity, "start_mission", map[string]any{
		"title":          "Une de trop",
		"objective":      "One mission too many.",
		"first_check_at": "2026-08-24T09:00:00+02:00",
	})

	if !strings.Contains(result, "maximum") {
		t.Fatalf("résultat = %q, attendu le refus de la borne", result)
	}
}

// Le cloisonnement des missions est la conversation, comme pour les tâches
// planifiées : une mission d'une autre conversation n'est ni listée ni
// abandonnable, même avec son identifiant exact.
func TestMissionTools_ScopedToConversation(t *testing.T) {
	tools, db := newMissionTools(t)
	alice := privateIdentity("alice")

	elsewhere := baseMission("mis-autre", alice)
	elsewhere.ConversationID = "whatsapp:autre-canal"
	insertMission(t, db, elsewhere)
	insertMission(t, db, baseMission("mis-ici", alice))

	listed := executeReminderTool(t, tools, alice, "list_missions", map[string]any{})
	if strings.Contains(listed, "mis-autre") {
		t.Errorf("une mission d'une autre conversation est listée : %q", listed)
	}
	if !strings.Contains(listed, "mis-ici") {
		t.Errorf("la mission de la conversation manque : %q", listed)
	}

	result := executeReminderTool(t, tools, alice, "abandon_mission", map[string]any{"id": "mis-autre"})
	if !strings.Contains(result, "no mission") {
		t.Fatalf("résultat = %q, attendu un refus de cloisonnement", result)
	}
	if got := findMission(t, db, "mis-autre"); got.Status != persistence.MissionStatusActive {
		t.Errorf("statut = %q, la mission d'ailleurs a été touchée", got.Status)
	}

	result = executeReminderTool(t, tools, alice, "abandon_mission", map[string]any{"id": "mis-ici"})
	if !strings.Contains(result, "abandoned") {
		t.Fatalf("résultat = %q, attendu l'abandon", result)
	}
	if got := findMission(t, db, "mis-ici"); got.Status != persistence.MissionStatusAbandoned {
		t.Errorf("statut = %q, attendu abandoned", got.Status)
	}
}

// Sans exécuteur de tâches câblé, aucun outil de mission n'est offert : on
// ne promet pas un travail que personne ne fera.
func TestMissionTools_AbsentWhenTasksDisabled(t *testing.T) {
	tools, _ := newMissionTools(t)
	tools.Tasks = false

	result := executeReminderTool(t, tools, privateIdentity("alice"), "start_mission", map[string]any{
		"title":          "Dossier",
		"objective":      "Follow something.",
		"first_check_at": "2026-08-24T09:00:00+02:00",
	})

	// L'outil n'existe pas dans ce tour : le modèle reçoit une erreur
	// d'outil inconnu, pas un refus de permission.
	if strings.Contains(result, "started") {
		t.Fatalf("résultat = %q, l'outil ne devait pas exister", result)
	}
}
