package agent_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// reminderTestNow est l'horloge fixe des tests d'outils de rappels.
var reminderTestNow = time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)

func reminderTestConfig() *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: []string{
					"reminder.personal.read", "reminder.personal.write", "reminder.personal.delete",
					"reminder.group.read", "reminder.group.write", "reminder.group.delete",
				}},
				"read-only": {Permissions: []string{"reminder.personal.read"}},
			},
			Principals: []config.Principal{
				{ID: "alice", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
				{ID: "leo", Kind: config.PrincipalKindHuman, DisplayName: "Léo", Roles: []string{"read-only"}},
			},
		},
	}
}

func reminderTestDB(t *testing.T) *persistence.DB {
	t.Helper()

	cfg := config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}

	db, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func newReminderTools(t *testing.T) (agent.ReminderTools, *persistence.DB) {
	t.Helper()

	db := reminderTestDB(t)

	return agent.ReminderTools{
		DB:         db,
		Repo:       persistence.NewReminderRepository(),
		Authorizer: authorization.NewAuthorizer(reminderTestConfig()),
		Now:        func() time.Time { return reminderTestNow },
	}, db
}

// executeReminderTool exécute un unique tool-call (name, args) sur un
// OrchestratorAgent doté de tools, et retourne le texte du résultat d'outil
// observé par le modèle au tour suivant.
func executeReminderTool(t *testing.T, tools agent.ReminderTools, identity model.ExecutionIdentity, name string, args map[string]any) string {
	t.Helper()

	var toolResultText string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", name, toolCallArgs(t, args))), nil
			}

			for _, m := range opts.Messages {
				if m.Role() == llm.RoleTool {
					toolResultText = m.Content()
				}
			}

			return scriptedFinalResponse("ok"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", "Test Org", map[string]delegation.Specialist{}, 5).WithReminderTools(tools)

	if _, err := a.Execute(context.Background(), agent.Request{Identity: identity, Input: "test"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	return toolResultText
}

func listReminderRows(t *testing.T, db *persistence.DB, conversationID string) []persistence.Reminder {
	t.Helper()

	repo := persistence.NewReminderRepository()
	var rows []persistence.Reminder
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		rows, err = repo.ListPendingByConversation(context.Background(), tx, conversationID)
		return err
	})
	if err != nil {
		t.Fatalf("ListPendingByConversation: %v", err)
	}

	return rows
}

func TestCreateReminder_PersistsForCurrentConversation(t *testing.T) {
	tools, db := newReminderTools(t)
	identity := privateIdentity("alice")

	result := executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"fire_at": "2026-08-18T09:00:00+02:00",
		"message": "sortir les poubelles",
	})

	if !strings.Contains(result, "Rappel programmé") {
		t.Fatalf("résultat = %q, attendu une confirmation de programmation", result)
	}

	rows := listReminderRows(t, db, string(identity.ConversationID))
	if len(rows) != 1 {
		t.Fatalf("rappels persistés = %d, attendu 1", len(rows))
	}

	rem := rows[0]
	if rem.Provider != identity.Provider || rem.ChannelID != identity.ChannelID {
		t.Errorf("destination = (%s, %s), attendu celle de la conversation (%s, %s) — jamais choisie par le modèle",
			rem.Provider, rem.ChannelID, identity.Provider, identity.ChannelID)
	}
	if rem.FireAt != "2026-08-18T07:00:00Z" {
		t.Errorf("fire_at = %q, attendu 2026-08-18T07:00:00Z (normalisé UTC)", rem.FireAt)
	}
}

func TestCreateReminder_RejectsPastAndFarFuture(t *testing.T) {
	tools, db := newReminderTools(t)
	identity := privateIdentity("alice")

	past := executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"fire_at": "2026-08-17T09:00:00Z",
		"message": "trop tard",
	})
	if !strings.Contains(past, "déjà passé") {
		t.Errorf("résultat pour une date passée = %q, attendu un refus 'déjà passé'", past)
	}

	far := executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"fire_at": "2028-01-01T09:00:00Z",
		"message": "trop loin",
	})
	if !strings.Contains(far, "plus d'un an") {
		t.Errorf("résultat pour une date trop lointaine = %q, attendu un refus 'plus d'un an'", far)
	}

	if rows := listReminderRows(t, db, string(identity.ConversationID)); len(rows) != 0 {
		t.Errorf("rappels persistés = %d, attendu 0", len(rows))
	}
}

func TestCreateReminder_DeniedWithoutWritePermission(t *testing.T) {
	tools, db := newReminderTools(t)
	identity := privateIdentity("leo") // rôle read-only : pas de reminder.personal.write

	result := executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"fire_at": "2026-08-18T09:00:00Z",
		"message": "interdit",
	})

	if !strings.Contains(result, "refusée") {
		t.Errorf("résultat = %q, attendu un refus d'autorisation", result)
	}

	if rows := listReminderRows(t, db, string(identity.ConversationID)); len(rows) != 0 {
		t.Errorf("rappels persistés = %d, attendu 0", len(rows))
	}
}

func TestCancelReminder_IsolatedByConversation(t *testing.T) {
	tools, db := newReminderTools(t)
	alice := privateIdentity("alice")

	created := executeReminderTool(t, tools, alice, "create_reminder", map[string]any{
		"fire_at": "2026-08-18T09:00:00Z",
		"message": "secret d'alice",
	})

	rows := listReminderRows(t, db, string(alice.ConversationID))
	if len(rows) != 1 {
		t.Fatalf("rappels persistés = %d, attendu 1 (résultat: %q)", len(rows), created)
	}
	id := string(rows[0].ID)

	// Depuis une autre conversation (groupe), le rappel d'alice est
	// invisible et inannulable, même pour alice.
	group := groupIdentity("alice", "famille")
	fromGroup := executeReminderTool(t, tools, group, "cancel_reminder", map[string]any{"id": id})
	if !strings.Contains(fromGroup, "aucun rappel") {
		t.Errorf("annulation depuis une autre conversation = %q, attendu 'aucun rappel'", fromGroup)
	}

	// Depuis la conversation d'origine, l'annulation réussit.
	fromOwn := executeReminderTool(t, tools, alice, "cancel_reminder", map[string]any{"id": id})
	if !strings.Contains(fromOwn, "annulé") {
		t.Errorf("annulation depuis la conversation d'origine = %q, attendu 'annulé'", fromOwn)
	}

	if remaining := listReminderRows(t, db, string(alice.ConversationID)); len(remaining) != 0 {
		t.Errorf("rappels restants = %d, attendu 0", len(remaining))
	}
}

func TestListReminders_ShowsPendingWithIDs(t *testing.T) {
	tools, _ := newReminderTools(t)
	identity := privateIdentity("alice")

	executeReminderTool(t, tools, identity, "create_reminder", map[string]any{
		"fire_at": "2026-08-18T09:00:00Z",
		"message": "sortir les poubelles",
	})

	result := executeReminderTool(t, tools, identity, "list_reminders", map[string]any{})

	if !strings.Contains(result, "sortir les poubelles") || !strings.Contains(result, "[id: ") {
		t.Errorf("liste = %q, attendu le message et un identifiant", result)
	}
}
