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

// Les missions sont des dossiers au long cours : un objectif, un journal de
// bord tenu par l'agent, des réveils espacés de plusieurs jours. Elles
// vivent à côté des tâches planifiées et réutilisent leurs permissions
// (task.*) : une mission est une tâche au long cours, pas une nouvelle
// famille de pouvoirs. Les outils sont portés par ReminderTools pour la
// même raison que schedule_task : ils partagent le gating Tasks — sans
// exécuteur câblé côté registre, proposer une mission serait promettre un
// travail que personne ne ferait.

// missionMaxObjectiveLen borne le mandat d'une mission. Plus long qu'une
// consigne de tâche : un dossier porte du contexte (interlocuteurs,
// références, historique) qu'une tâche cron n'a pas.
const missionMaxObjectiveLen = 3000

// missionMaxTitleLen borne le titre, affiché dans des listes (conversation
// et profil) et jamais chiffré : il doit rester un intitulé, pas un résumé.
const missionMaxTitleLen = 120

func (t ReminderTools) missionsEnabled() bool {
	return t.enabled() && t.Tasks && t.Missions != nil
}

func (t ReminderTools) buildMissionTools(identity model.ExecutionIdentity) []llm.Tool {
	if !t.missionsEnabled() {
		return nil
	}

	return []llm.Tool{
		t.newStartMissionTool(identity),
		t.newListMissionsTool(identity),
		t.newAbandonMissionTool(identity),
	}
}

func (t ReminderTools) newStartMissionTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("title", "Short label for lists (e.g. \"Parking complaint follow-up\"). A name, not a summary.", "string").
		RequiredProperty("objective", "The mission's mandate, written to be understood ON ITS OWN, without this conversation's history: what to pursue, who is involved, what \"done\" looks like. It cannot be changed later.", "string").
		RequiredProperty("first_check_at", "RFC 3339 date-time with offset for the FIRST check-in (e.g. 2026-09-10T09:00:00+02:00), derived from the current date and time given in the execution context.", "string")

	return llm.NewFuncTool(
		"start_mission",
		"Open a long-running MISSION: a matter you will follow over days or weeks, checking in at intervals, keeping a logbook, and proposing next steps as they become relevant (e.g. \"follow my complaint and prepare a reminder if no reply\"). Every check-in reports to this conversation, and any external action still requires explicit confirmation. For a one-off or cron-style job, use schedule_task instead; for a plain memo, create_reminder.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			title := strings.TrimSpace(stringParam(params, "title"))
			objective := strings.TrimSpace(stringParam(params, "objective"))
			if title == "" || objective == "" {
				return llm.NewToolResult("error: 'title' and 'objective' are required and cannot be empty."), nil
			}
			if len(title) > missionMaxTitleLen {
				return llm.NewToolResult(fmt.Sprintf("error: title too long (%d characters, maximum %d).", len(title), missionMaxTitleLen)), nil
			}
			if len(objective) > missionMaxObjectiveLen {
				return llm.NewToolResult(fmt.Sprintf("error: objective too long (%d characters, maximum %d).", len(objective), missionMaxObjectiveLen)), nil
			}

			now := t.now()

			firstCheckAt, err := time.Parse(time.RFC3339, strings.TrimSpace(stringParam(params, "first_check_at")))
			if err != nil {
				return llm.NewToolResult("error: 'first_check_at' must be an RFC 3339 date-time with offset (e.g. 2026-09-10T09:00:00+02:00)."), nil
			}
			if !firstCheckAt.After(now) {
				return llm.NewToolResult(fmt.Sprintf("error: 'first_check_at' is in the past (current time: %s).", now.Format(time.RFC3339))), nil
			}

			if _, err := t.authorize(ctx, identity, "task", "write"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("mission refused: %v", err)), nil
			}

			mission := persistence.Mission{
				ID:             uuid.NewString(),
				OrgID:          string(identity.OrgID),
				PrincipalID:    string(identity.PrincipalID),
				ConversationID: string(identity.ConversationID),
				Provider:       identity.Provider,
				ChannelID:      identity.ChannelID,
				AgentID:        t.AgentName,
				Title:          title,
				Objective:      objective,
				Status:         persistence.MissionStatusActive,
				NextCheckAt:    firstCheckAt.UTC(),
				CreatedAt:      now.UTC(),
				UpdatedAt:      now.UTC(),
			}

			var outcome string
			err = t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				// La borne se vérifie dans la même transaction que
				// l'insertion : chaque mission active coûte des tours de
				// modèle à intervalles réguliers, la limite doit tenir
				// même sous appels concurrents.
				count, err := t.Missions.CountActiveByPrincipal(ctx, tx, mission.OrgID, mission.PrincipalID)
				if err != nil {
					return err
				}
				if count >= persistence.MaxActiveMissionsPerPrincipal {
					outcome = fmt.Sprintf("error: you already follow %d active missions, the maximum. Close or abandon one first (list_missions, abandon_mission).", count)
					return nil
				}

				return t.Missions.Insert(ctx, tx, mission)
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not start the mission: %v", err)), nil
			}
			if outcome != "" {
				return llm.NewToolResult(outcome), nil
			}

			return llm.NewToolResult(fmt.Sprintf("Mission %q started (id: %s). First check-in %s; each check-in reports here.", title, mission.ID, firstCheckAt.Format(time.RFC3339))), nil
		},
	)
}

func (t ReminderTools) newListMissionsTool(identity model.ExecutionIdentity) llm.Tool {
	return llm.NewFuncTool(
		"list_missions",
		"List the missions of the current conversation, with their id (for abandon_mission), status, next check-in and latest logbook note. Use it to answer \"where do my ongoing matters stand?\".",
		llm.NewJSONSchema(),
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			if _, err := t.authorize(ctx, identity, "task", "read"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("listing refused: %v", err)), nil
			}

			var missions []persistence.Mission
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				var err error
				missions, err = t.Missions.ListByConversation(ctx, tx, string(identity.ConversationID), 20)
				return err
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not list the missions: %v", err)), nil
			}

			if len(missions) == 0 {
				return llm.NewToolResult("No mission in this conversation."), nil
			}

			var b strings.Builder
			b.WriteString("Missions:\n")
			for i, mission := range missions {
				fmt.Fprintf(&b, "%d. [id: %s] %s — %s", i+1, mission.ID, mission.Title, mission.Status)
				if mission.Status == persistence.MissionStatusActive && !mission.NextCheckAt.IsZero() {
					fmt.Fprintf(&b, ", next check-in %s", mission.NextCheckAt.Format(time.RFC3339))
				}
				if note := latestJournalLine(mission.Journal); note != "" {
					fmt.Fprintf(&b, "\n   latest note: %s", note)
				}
				b.WriteString("\n")
			}

			return llm.NewToolResult(strings.TrimSpace(b.String())), nil
		},
	)
}

func (t ReminderTools) newAbandonMissionTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("id", "The mission id, as returned by list_missions or start_mission.", "string")

	return llm.NewFuncTool(
		"abandon_mission",
		"Abandon an active mission of the current conversation: no further check-in will happen. Use list_missions first to get the id, and confirm with the person before abandoning a mission they did not just ask to stop.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			id := strings.TrimSpace(stringParam(params, "id"))
			if id == "" {
				return llm.NewToolResult("error: 'id' is required."), nil
			}

			if _, err := t.authorize(ctx, identity, "task", "delete"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("abandon refused: %v", err)), nil
			}

			var outcome string
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				mission, found, err := t.Missions.FindByID(ctx, tx, id)
				if err != nil {
					return err
				}

				// Même cloisonnement que cancel_scheduled_task : visible et
				// abandonnable depuis la seule conversation où la mission a
				// été ouverte.
				if !found || mission.ConversationID != string(identity.ConversationID) {
					outcome = fmt.Sprintf("no mission with id %q in this conversation.", id)
					return nil
				}
				if mission.Status != persistence.MissionStatusActive {
					outcome = fmt.Sprintf("mission %q is already closed (%s).", id, mission.Status)
					return nil
				}

				changed, err := t.Missions.UpdateStatus(ctx, tx, mission.OrgID, mission.PrincipalID, mission.ID, persistence.MissionStatusAbandoned, t.now())
				if err != nil {
					return err
				}
				if !changed {
					outcome = fmt.Sprintf("mission %q could not be abandoned.", id)
					return nil
				}

				outcome = fmt.Sprintf("Mission %q abandoned.", mission.Title)
				return nil
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not abandon the mission: %v", err)), nil
			}

			return llm.NewToolResult(outcome), nil
		},
	)
}

// latestJournalLine retourne la dernière ligne non vide du journal de bord :
// celle qui porte l'état courant du dossier, la seule utile dans une liste.
func latestJournalLine(journal string) string {
	lines := strings.Split(strings.TrimSpace(journal), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
