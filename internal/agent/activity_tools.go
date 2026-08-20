package agent

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// Introspection : ce que l'assistant a réellement fait dans cette
// conversation, par opposition à ce qu'il croit avoir fait.
//
// Sans cet outil, un assistant interrogé sur un rappel déjà délivré ne
// voyait que la liste des rappels À VENIR — vide — et en concluait qu'il
// n'avait jamais rien programmé. Il contredisait alors l'utilisateur, qui
// venait précisément de recevoir ce rappel, et s'excusait d'une faute
// imaginaire. Un journal factuel vaut mieux qu'une déduction sur une
// absence.

// activityWindow est la profondeur du journal : au-delà, un rappel
// délivré n'éclaire plus la conversation en cours.
const activityWindow = 7 * 24 * time.Hour

// maxActivityEntries borne la sortie envoyée au modèle.
const maxActivityEntries = 25

// activityEntry est une ligne du journal, déjà rendue.
type activityEntry struct {
	At   time.Time
	Text string
}

func (t ReminderTools) newRecentActivityTool(identity model.ExecutionIdentity) llm.Tool {
	return llm.NewFuncTool(
		"list_recent_activity",
		"List what the assistant actually did in this conversation over the past days: reminders delivered or failed, scheduled tasks run, and action plans confirmed or cancelled. Use it before claiming that something was or was not done — especially when the user reports receiving something.",
		llm.NewJSONSchema(),
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			if _, err := t.authorize(ctx, identity, "reminder", "read"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("listing refused: %v", err)), nil
			}

			now := t.now()
			since := now.Add(-activityWindow).UTC().Format(time.RFC3339)

			var entries []activityEntry

			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				reminders, err := t.Repo.ListRecentByConversation(ctx, tx, string(identity.ConversationID), since, maxActivityEntries)
				if err != nil {
					return err
				}
				entries = append(entries, reminderEntries(reminders, now)...)

				plans, err := persistence.NewActionPlanRepository().ListRecentByConversation(ctx, tx, identity.ConversationID, since, maxActivityEntries)
				if err != nil {
					return err
				}
				actions := persistence.NewActionRepository()
				for _, plan := range plans {
					planActions, err := actions.ListByPlanID(ctx, tx, plan.ID)
					if err != nil {
						return err
					}
					entries = append(entries, planEntry(plan, planActions))
				}

				return nil
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not read the activity log: %v", err)), nil
			}

			if len(entries) == 0 {
				return llm.NewToolResult("Nothing recorded in this conversation over the past days: no reminder delivered, no task run, no action plan."), nil
			}

			sort.Slice(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
			if len(entries) > maxActivityEntries {
				entries = entries[:maxActivityEntries]
			}

			var b strings.Builder
			b.WriteString("Recent activity in this conversation (most recent first):\n")
			for _, entry := range entries {
				fmt.Fprintf(&b, "- %s\n", entry.Text)
			}

			return llm.NewToolResult(strings.TrimSpace(b.String())), nil
		},
	)
}

// reminderEntries rend les rappels et tâches en lignes de journal.
func reminderEntries(reminders []persistence.Reminder, now time.Time) []activityEntry {
	entries := make([]activityEntry, 0, len(reminders))

	for _, rem := range reminders {
		kind := "reminder"
		if rem.Kind == persistence.ReminderKindTask {
			kind = "scheduled task"
		}

		at := parseActivityTime(rem.FireAt)
		var text string

		switch rem.Status {
		case persistence.ReminderStatusSent:
			if rem.SentAt != nil {
				at = parseActivityTime(*rem.SentAt)
			}
			text = fmt.Sprintf("%s DELIVERED at %s — %q (id: %s)", kind, at.Format(time.RFC3339), rem.Message, rem.ID)
		case persistence.ReminderStatusFailed:
			text = fmt.Sprintf("%s FAILED to deliver, due %s — %q (id: %s)", kind, rem.FireAt, rem.Message, rem.ID)
		case persistence.ReminderStatusCancelled:
			text = fmt.Sprintf("%s cancelled, was due %s — %q (id: %s)", kind, rem.FireAt, rem.Message, rem.ID)
		default:
			when := "upcoming"
			if at.Before(now) {
				when = "overdue"
			}
			text = fmt.Sprintf("%s %s at %s — %q (id: %s)", kind, when, rem.FireAt, rem.Message, rem.ID)
		}

		entries = append(entries, activityEntry{At: at, Text: text})
	}

	return entries
}

// planEntry rend un plan d'actions et l'issue de chacune de ses actions.
func planEntry(plan persistence.ActionPlan, actions []persistence.Action) activityEntry {
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		names = append(names, fmt.Sprintf("%s [%s]", action.ToolName, action.Status))
	}

	detail := "no action"
	if len(names) > 0 {
		detail = strings.Join(names, ", ")
	}

	at := parseActivityTime(plan.UpdatedAt)
	if at.IsZero() {
		at = parseActivityTime(plan.CreatedAt)
	}

	return activityEntry{
		At:   at,
		Text: fmt.Sprintf("action plan %s at %s — %s", plan.Status, at.Format(time.RFC3339), detail),
	}
}

func parseActivityTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
