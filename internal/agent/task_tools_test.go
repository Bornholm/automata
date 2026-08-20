package agent_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
)

// taskTestConfig ajoute les permissions task.* à la configuration des tests
// de rappels : programmer un travail de l'assistant est un pouvoir distinct
// de poser un pense-bête, et se donne séparément.
func taskTestConfig() *config.Config {
	cfg := reminderTestConfig()
	adult := cfg.Identities.Roles["adult"]
	adult.Permissions = append(adult.Permissions,
		"task.personal.read", "task.personal.write", "task.personal.delete",
		"task.group.read", "task.group.write", "task.group.delete",
	)
	cfg.Identities.Roles["adult"] = adult
	return cfg
}

func newTaskTools(t *testing.T) (agent.ReminderTools, *persistence.DB) {
	t.Helper()

	db := reminderTestDB(t)

	return agent.ReminderTools{
		DB:         db,
		Repo:       persistence.NewReminderRepository(nil),
		Authorizer: authorization.NewAuthorizer(taskTestConfig()),
		Now:        func() time.Time { return reminderTestNow },
		Tasks:      true,
		AgentName:  "main",
	}, db
}

func listTaskRows(t *testing.T, db *persistence.DB, conversationID string) []persistence.Reminder {
	t.Helper()

	repo := persistence.NewReminderRepository(nil)
	var rows []persistence.Reminder
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		rows, err = repo.ListPendingByConversation(context.Background(), tx, conversationID, persistence.ReminderKindTask)
		return err
	})
	if err != nil {
		t.Fatalf("ListPendingByConversation: %v", err)
	}

	return rows
}

func TestScheduleTask_PersistsRecurringTaskForCurrentConversation(t *testing.T) {
	tools, db := newTaskTools(t)
	identity := privateIdentity("alice")

	result := executeReminderTool(t, tools, identity, "schedule_task", map[string]any{
		"recurrence":  "0 8 * * *",
		"timezone":    "Europe/Paris",
		"instruction": "Prepare a short weather report for Aignay-le-Duc",
	})

	if !strings.Contains(result, "Task scheduled") {
		t.Fatalf("résultat = %q, attendu une confirmation de programmation", result)
	}

	rows := listTaskRows(t, db, string(identity.ConversationID))
	if len(rows) != 1 {
		t.Fatalf("tâches persistées = %d, attendu 1", len(rows))
	}

	task := rows[0]
	if task.Kind != persistence.ReminderKindTask {
		t.Errorf("nature = %q, attendu task", task.Kind)
	}
	if task.AgentID != "main" {
		t.Errorf("agent = %q, attendu l'agent créateur figé", task.AgentID)
	}
	if task.PrincipalID != identity.PrincipalID || task.ConversationID != identity.ConversationID {
		t.Errorf("identité = (%q, %q), attendu celle du tour courant", task.PrincipalID, task.ConversationID)
	}
	if task.Recurrence != "0 8 * * *" || task.Timezone != "Europe/Paris" {
		t.Errorf("récurrence = (%q, %q)", task.Recurrence, task.Timezone)
	}

	// 8h à Paris le lendemain, soit 06:00 UTC : l'échéance est bien calculée
	// dans le fuseau demandé, pas dans celui du serveur.
	if task.FireAt != "2026-08-18T06:00:00Z" {
		t.Errorf("première occurrence = %q, attendu 2026-08-18T06:00:00Z", task.FireAt)
	}

	// Une tâche n'est pas un rappel : elle ne doit pas apparaître dans la
	// liste des rappels.
	if got := listReminderRows(t, db, string(identity.ConversationID)); len(got) != 0 {
		t.Errorf("rappels = %d, attendu 0 : une tâche ne doit pas se faire passer pour un rappel", len(got))
	}
}

func TestScheduleTask_DeniedWithoutTaskPermission(t *testing.T) {
	tools, db := newTaskTools(t)
	identity := privateIdentity("leo") // rôle read-only : aucune permission task.*

	result := executeReminderTool(t, tools, identity, "schedule_task", map[string]any{
		"recurrence":  "0 8 * * *",
		"instruction": "Prepare a weather report",
	})

	if !strings.Contains(result, "refused") {
		t.Errorf("résultat = %q, attendu un refus", result)
	}
	if got := listTaskRows(t, db, string(identity.ConversationID)); len(got) != 0 {
		t.Errorf("tâches persistées = %d, attendu 0", len(got))
	}
}

// Une permission reminder.* ne donne pas le droit de programmer un travail :
// les deux domaines sont distincts, sinon le pouvoir le plus fort serait
// accordé par mégarde avec le plus faible.
func TestScheduleTask_ReminderPermissionIsNotEnough(t *testing.T) {
	db := reminderTestDB(t)
	tools := agent.ReminderTools{
		DB:         db,
		Repo:       persistence.NewReminderRepository(nil),
		Authorizer: authorization.NewAuthorizer(reminderTestConfig()), // reminder.* uniquement
		Now:        func() time.Time { return reminderTestNow },
		Tasks:      true,
		AgentName:  "main",
	}

	identity := privateIdentity("alice")
	result := executeReminderTool(t, tools, identity, "schedule_task", map[string]any{
		"recurrence":  "0 8 * * *",
		"instruction": "Prepare a weather report",
	})

	if !strings.Contains(result, "refused") {
		t.Errorf("résultat = %q, attendu un refus (task.personal.write absent)", result)
	}
}

func TestScheduledTaskTools_AbsentWhenTasksDisabled(t *testing.T) {
	tools, _ := newTaskTools(t)
	tools.Tasks = false

	result := executeReminderTool(t, tools, privateIdentity("alice"), "schedule_task", map[string]any{
		"recurrence":  "0 8 * * *",
		"instruction": "Prepare a weather report",
	})

	// L'outil n'existe pas : la boucle d'appel d'outils le signale au modèle
	// au lieu de programmer quoi que ce soit.
	if strings.Contains(result, "Task scheduled") {
		t.Errorf("résultat = %q, aucune tâche ne doit être programmée sans exécuteur", result)
	}
}

// Un rappel et une tâche ne s'annulent pas l'un l'autre, même en connaissant
// l'identifiant : chaque outil ne voit que sa propre nature.
func TestCancelScheduledTask_DoesNotCancelReminders(t *testing.T) {
	tools, db := newTaskTools(t)
	identity := privateIdentity("alice")

	executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"fire_at": "2026-08-18T09:00:00+02:00",
		"message": "sortir les poubelles",
	})

	reminders := listReminderRows(t, db, string(identity.ConversationID))
	if len(reminders) != 1 {
		t.Fatalf("rappels = %d, attendu 1", len(reminders))
	}

	result := executeReminderTool(t, tools, identity, "cancel_scheduled_task", map[string]any{
		"id": string(reminders[0].ID),
	})
	if !strings.Contains(result, "no scheduled task") {
		t.Errorf("résultat = %q, attendu un refus de trouver une tâche", result)
	}

	if got := listReminderRows(t, db, string(identity.ConversationID)); len(got) != 1 {
		t.Errorf("rappels restants = %d, le rappel a été annulé par l'outil des tâches", len(got))
	}
}

func TestListScheduledTasks_ShowsPendingWithIDs(t *testing.T) {
	tools, _ := newTaskTools(t)
	identity := privateIdentity("alice")

	executeReminderTool(t, tools, identity, "schedule_task", map[string]any{
		"recurrence":  "0 8 * * *",
		"timezone":    "Europe/Paris",
		"instruction": "Prepare a short weather report",
	})

	result := executeReminderTool(t, tools, identity, "list_scheduled_tasks", map[string]any{})

	if !strings.Contains(result, "Scheduled tasks") || !strings.Contains(result, "weather report") {
		t.Errorf("résultat = %q, attendu la liste des tâches", result)
	}
	if !strings.Contains(result, "0 8 * * *") {
		t.Errorf("résultat = %q, attendu la récurrence", result)
	}
}
