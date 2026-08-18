package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// taskMaxInstructionLen borne la consigne d'une tâche planifiée. Elle est
// rejouée à chaque occurrence, sans l'historique de la conversation : une
// consigne doit tenir seule, pas contenir un dossier.
const taskMaxInstructionLen = 2000

// newScheduleTaskTool expose schedule_task : programmer un TRAVAIL de
// l'assistant, par opposition à create_reminder qui pose un pense-bête.
//
// Ce que le modèle ne choisit jamais : l'agent exécutant (celui qui crée la
// tâche), l'identité d'exécution (le principal courant), le canal de
// livraison (la conversation courante) et la politique d'actions (lecture
// seule, appliquée par le dispatcher). Il ne décide que du QUAND et du QUOI.
func (t ReminderTools) newScheduleTaskTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		Property("fire_at", "RFC 3339 date-time with offset for a ONE-OFF run (e.g. 2026-08-19T08:00:00+02:00), derived from the current date and time given in the execution context. Ignored when 'recurrence' is set.", "string").
		Property("recurrence", "Standard 5-field cron expression for a REPEATING run (e.g. '0 8 * * *' for every day at 8am). The application computes the first occurrence itself.", "string").
		Property("timezone", "IANA timezone used to evaluate 'recurrence' (e.g. 'Europe/Paris'). Defaults to the server timezone shown in the execution context.", "string").
		RequiredProperty("instruction", "What to do at each run, written to be understood ON ITS OWN, without this conversation's history (e.g. \"Prepare a short weather report for Aignay-le-Duc (21510) for the day\"). Include every detail needed: place, expected format, length.", "string")

	return llm.NewFuncTool(
		"schedule_task",
		"Schedule a TASK you will carry out yourself at the due time, using your tools, and whose result is sent to this conversation: one-off with 'fire_at', repeating with 'recurrence' (cron). Use it for \"every morning give me a weather report\", \"every Monday summarise my tasks\" — anything requiring WORK at the due time. For a plain memo whose text is enough, use create_reminder instead.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			instruction := strings.TrimSpace(stringParam(params, "instruction"))
			if instruction == "" {
				return llm.NewToolResult("error: 'instruction' is required and cannot be empty."), nil
			}
			if len(instruction) > taskMaxInstructionLen {
				return llm.NewToolResult(fmt.Sprintf("error: instruction too long (%d characters, maximum %d).", len(instruction), taskMaxInstructionLen)), nil
			}

			now := t.now()

			fireAt, recurrence, timezone, problem := parseSchedule(params, now)
			if problem != "" {
				return llm.NewToolResult(problem), nil
			}

			if _, err := t.authorize(ctx, identity, "task", "write"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("scheduling refused: %v", err)), nil
			}

			task := persistence.Reminder{
				ID:             persistence.ReminderID(uuid.NewString()),
				OrgID:          identity.OrgID,
				PrincipalID:    identity.PrincipalID,
				ConversationID: identity.ConversationID,
				Provider:       identity.Provider,
				ChannelID:      identity.ChannelID,
				Message:        instruction,
				FireAt:         fireAt.UTC().Format(time.RFC3339),
				Status:         persistence.ReminderStatusPending,
				CreatedAt:      now.UTC().Format(time.RFC3339),
				Recurrence:     recurrence,
				Timezone:       timezone,
				Kind:           persistence.ReminderKindTask,
				AgentID:        t.AgentName,
			}

			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				return t.Repo.Insert(ctx, tx, task)
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not schedule the task: %v", err)), nil
			}

			t.Metrics.IncScheduledTaskCreated()

			if recurrence != "" {
				return llm.NewToolResult(fmt.Sprintf("Task scheduled (%s, timezone %s), first run %s (id: %s).", recurrence, timezone, fireAt.Format(time.RFC3339), task.ID)), nil
			}

			return llm.NewToolResult(fmt.Sprintf("Task scheduled for %s (id: %s).", fireAt.Format(time.RFC3339), task.ID)), nil
		},
	)
}

func (t ReminderTools) newListScheduledTasksTool(identity model.ExecutionIdentity) llm.Tool {
	return llm.NewFuncTool(
		"list_scheduled_tasks",
		"List the scheduled tasks of the current conversation, with their id (for cancel_scheduled_task), due time and instruction. Use it to answer \"what do you have scheduled?\".",
		llm.NewJSONSchema(),
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			if _, err := t.authorize(ctx, identity, "task", "read"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("listing refused: %v", err)), nil
			}

			var tasks []persistence.Reminder
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				var err error
				tasks, err = t.Repo.ListPendingByConversation(ctx, tx, string(identity.ConversationID), persistence.ReminderKindTask)
				return err
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not list the tasks: %v", err)), nil
			}

			if len(tasks) == 0 {
				return llm.NewToolResult("No scheduled task in this conversation."), nil
			}

			var b strings.Builder
			b.WriteString("Scheduled tasks:\n")
			for i, task := range tasks {
				if task.Recurrence != "" {
					fmt.Fprintf(&b, "%d. [id: %s] repeating (%s, %s), next run %s — %s\n", i+1, task.ID, task.Recurrence, task.Timezone, task.FireAt, task.Message)
					continue
				}
				fmt.Fprintf(&b, "%d. [id: %s] %s — %s\n", i+1, task.ID, task.FireAt, task.Message)
			}

			return llm.NewToolResult(strings.TrimSpace(b.String())), nil
		},
	)
}

func (t ReminderTools) newCancelScheduledTaskTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("id", "Id of the task to cancel, as returned by list_scheduled_tasks or when it was scheduled.", "string")

	return llm.NewFuncTool(
		"cancel_scheduled_task",
		"Cancel a scheduled task of the current conversation.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			id := strings.TrimSpace(stringParam(params, "id"))
			if id == "" {
				return llm.NewToolResult("error: 'id' is required."), nil
			}

			if _, err := t.authorize(ctx, identity, "task", "delete"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("cancellation refused: %v", err)), nil
			}

			var outcome string
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				task, found, err := t.Repo.FindByID(ctx, tx, persistence.ReminderID(id))
				if err != nil {
					return err
				}

				// Même cloisonnement que les rappels : visible et annulable
				// depuis la seule conversation où la tâche a été créée, et
				// jamais confondue avec un rappel du même identifiant.
				if !found || task.ConversationID != identity.ConversationID || task.Kind != persistence.ReminderKindTask {
					outcome = fmt.Sprintf("no scheduled task with id %q in this conversation.", id)
					return nil
				}

				ok, err := t.Repo.UpdateStatus(ctx, tx, task.ID, persistence.ReminderStatusPending, persistence.ReminderStatusCancelled, nil)
				if err != nil {
					return err
				}
				if !ok {
					outcome = fmt.Sprintf("task %q is no longer scheduled (already run or cancelled).", id)
					return nil
				}

				outcome = fmt.Sprintf("Task %q cancelled.", id)
				return nil
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not cancel the task: %v", err)), nil
			}

			return llm.NewToolResult(outcome), nil
		},
	)
}
