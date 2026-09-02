package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/google/uuid"
	cron "github.com/robfig/cron/v3"

	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
)

// reminderMaxMessageLen borne le texte d'un rappel : au-delà, ce n'est plus
// un rappel mais un document, et le message est délivré tel quel sur le
// canal.
const reminderMaxMessageLen = 2000

// reminderMaxHorizon borne l'échéance d'un rappel : au-delà d'un an, une
// faute de frappe dans l'année ("2062") est plus probable qu'une intention
// réelle.
const reminderMaxHorizon = 366 * 24 * time.Hour

// ReminderTools regroupe les dépendances nécessaires pour exposer les
// outils de rappels ponctuels (create_reminder, list_reminders,
// cancel_reminder) à un OrchestratorAgent. La valeur zéro (DB nil) n'expose
// aucun outil : voir WithReminderTools.
//
// Contrairement à forget_memory, ces outils s'exécutent directement, sans
// plan de confirmation : un rappel est une écriture interne, réversible
// (cancel_reminder recrée l'état antérieur), limitée à la conversation
// courante — rien de la liste de plan de conception, §10.1 (écritures externes,
// suppressions de données, écritures org). La destination de livraison
// n'est jamais un paramètre du modèle : elle est figée à la conversation
// courante au moment de la création.
type ReminderTools struct {
	DB         *persistence.DB
	Repo       *persistence.ReminderRepository
	Authorizer *authorization.Authorizer

	// Tasks expose en plus les outils de tâches planifiées (schedule_task,
	// list_scheduled_tasks, cancel_scheduled_task). Contrairement à un
	// rappel, qui délivre un texte figé, une tâche fait TRAVAILLER un agent
	// à l'échéance : elle n'est proposée que si un exécuteur est réellement
	// câblé côté dispatcher (voir internal/registry), sinon l'assistant
	// promettrait un travail que personne ne ferait.
	Tasks bool
	// AgentName est l'agent figé sur les tâches créées ici : celui qui les
	// exécutera. Voir migration 0007.
	AgentName string

	// Missions, non nil, ajoute les outils de missions (start_mission,
	// list_missions, abandon_mission) : les dossiers au long cours, avec
	// journal de bord et réveils espacés — voir mission_tools.go et
	// internal/mission. Elles exigent Tasks pour la même raison que
	// schedule_task : sans exécuteur câblé, on promettrait un travail que
	// personne ne ferait.
	Missions *persistence.MissionRepository

	// Events, non nil, permet à un plugin actif de tenir le magasin des
	// rappels d'un membre à la place de la table reminders — un agenda
	// CalDAV, par exemple. La résolution se fait membre par membre, à
	// chaque appel : voir event_store.go.
	//
	// Les tâches planifiées n'y vont JAMAIS. Une tâche n'est pas un
	// événement d'agenda : c'est une consigne donnée à un agent, dont le
	// texte est une instruction et non quelque chose qu'on lit dans son
	// calendrier.
	Events EventStoreResolver

	// Now permet aux tests de fixer l'horloge ; nil vaut time.Now.
	Now func() time.Time

	// Metrics observe les créations de rappels. Peut être nil.
	Metrics *observability.Metrics
}

func (t ReminderTools) enabled() bool {
	return t.DB != nil && t.Repo != nil && t.Authorizer != nil
}

func (t ReminderTools) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// buildReminderTools construit les outils de rappels autorisés pour
// identity. Comme pour la mémoire (buildMemoryTools), la portée n'est
// jamais décidée par le modèle : elle est celle de la conversation courante
// (writeScope), et chaque appel repasse par l'Authorizer.
func (t ReminderTools) buildReminderTools(identity model.ExecutionIdentity) []llm.Tool {
	if !t.enabled() {
		return nil
	}

	tools := []llm.Tool{
		t.newCreateReminderTool(identity),
		t.newListRemindersTool(identity),
		t.newCancelReminderTool(identity),
		// Introspection : ce qui a réellement été délivré ou exécuté.
		// Elle accompagne les rappels parce qu'elle en relit le journal,
		// et qu'un assistant sans rappels n'a rien à introspecter.
		t.newRecentActivityTool(identity),
	}

	if t.Tasks {
		tools = append(tools,
			t.newScheduleTaskTool(identity),
			t.newListScheduledTasksTool(identity),
			t.newCancelScheduledTaskTool(identity),
		)
	}

	tools = append(tools, t.buildMissionTools(identity)...)

	return tools
}

// authorize vérifie la permission <domain>.<scope>.<action> pour la portée
// de la conversation courante et retourne cette portée. domain vaut
// "reminder" ou "task" : programmer un travail de l'assistant est un
// pouvoir distinct de poser un pense-bête, et se donne séparément.
func (t ReminderTools) authorize(ctx context.Context, identity model.ExecutionIdentity, domain, action string) (model.Scope, error) {
	scope, ok := writeScope(identity)
	if !ok {
		return "", fmt.Errorf("reminders and scheduled tasks are not available in this execution context")
	}

	err := t.Authorizer.Authorize(ctx, authorization.AuthorizationRequest{
		Identity:      identity,
		Permission:    fmt.Sprintf("%s.%s.%s", domain, scope, action),
		TargetOrgID:   identity.OrgID,
		TargetScope:   scope,
		TargetScopeID: scopeID(scope, identity),
	})
	if err != nil {
		return "", err
	}

	return scope, nil
}

func (t ReminderTools) newCreateReminderTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		Property("fire_at", "RFC 3339 date-time with offset for a ONE-OFF reminder (e.g. 2026-08-18T09:00:00+02:00), derived from the current date and time given in the execution context. Ignored when 'recurrence' is set.", "string").
		Property("recurrence", "Standard 5-field cron expression for a REPEATING reminder (e.g. '0 20 * * 2' for every Tuesday at 8pm). The application computes the first occurrence itself.", "string").
		Property("timezone", "IANA timezone used to evaluate 'recurrence' (e.g. 'Europe/Paris'). Defaults to the server timezone shown in the execution context.", "string").
		RequiredProperty("message", "The reminder text, sent to the conversation as is at each due time.", "string")

	return llm.NewFuncTool(
		"create_reminder",
		"Schedule a plain reminder in the current conversation: one-off with 'fire_at', repeating with 'recurrence' (cron). Its text is delivered AS IS, no work is done at the due time. Use it for \"remind me to ...\", \"ping us every ...\". Use schedule_task instead when the due time requires actual work (a weather report, a summary).",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			message := strings.TrimSpace(stringParam(params, "message"))
			if message == "" {
				return llm.NewToolResult("error: 'message' is required and cannot be empty."), nil
			}
			if len(message) > reminderMaxMessageLen {
				return llm.NewToolResult(fmt.Sprintf("error: message too long (%d characters, maximum %d).", len(message), reminderMaxMessageLen)), nil
			}

			now := t.now()

			fireAt, recurrence, timezone, problem := parseSchedule(params, now)
			if problem != "" {
				return llm.NewToolResult(problem), nil
			}

			if _, err := t.authorize(ctx, identity, "reminder", "write"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("reminder creation refused: %v", err)), nil
			}

			// Magasin délégué : le rappel part chez le plugin, et rien
			// n'est écrit en base. Pas de repli en cas de panne — un
			// rappel silencieusement éparpillé entre deux magasins est
			// pire qu'un refus que la personne voit passer.
			if store := t.eventStore(ctx, string(identity.OrgID), string(identity.PrincipalID)); store != nil {
				return t.createInEventStore(ctx, store, message, fireAt, recurrence, timezone)
			}

			rem := persistence.Reminder{
				ID:             persistence.ReminderID(uuid.NewString()),
				OrgID:          identity.OrgID,
				PrincipalID:    identity.PrincipalID,
				ConversationID: identity.ConversationID,
				Provider:       identity.Provider,
				ChannelID:      identity.ChannelID,
				Message:        message,
				FireAt:         fireAt.UTC().Format(time.RFC3339),
				Status:         persistence.ReminderStatusPending,
				CreatedAt:      now.UTC().Format(time.RFC3339),
				Recurrence:     recurrence,
				Timezone:       timezone,
				Kind:           persistence.ReminderKindMessage,
			}

			// Dédoublonnage : un rappel identique déjà en attente est
			// rendu tel quel plutôt que doublé. L'assistant qui
			// reformule sa réponse au tour suivant (« et je te rappelle
			// ce soir à 20h ») rappelle en réalité l'outil, et
			// l'utilisateur recevait deux fois le même rappel.
			var existing persistence.Reminder
			duplicate := false

			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				var err error
				existing, duplicate, err = t.Repo.FindPendingDuplicate(ctx, tx,
					string(identity.ConversationID), persistence.ReminderKindMessage, rem.FireAt, message)
				if err != nil {
					return err
				}
				if duplicate {
					return nil
				}
				return t.Repo.Insert(ctx, tx, rem)
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not create the reminder: %v", err)), nil
			}

			if duplicate {
				return llm.NewToolResult(fmt.Sprintf(
					"This reminder already exists, nothing was created: %s (id: %s). Tell the user it is already scheduled instead of announcing a new one.",
					existing.FireAt, existing.ID)), nil
			}

			t.Metrics.IncReminderCreated()

			if recurrence != "" {
				return llm.NewToolResult(fmt.Sprintf("Repeating reminder scheduled (%s, timezone %s), next occurrence %s (id: %s).", recurrence, timezone, fireAt.Format(time.RFC3339), rem.ID)), nil
			}

			return llm.NewToolResult(fmt.Sprintf("Reminder scheduled for %s (id: %s).", fireAt.Format(time.RFC3339), rem.ID)), nil
		},
	)
}

func (t ReminderTools) newListRemindersTool(identity model.ExecutionIdentity) llm.Tool {
	return llm.NewFuncTool(
		"list_reminders",
		"List the reminders still ahead in the current conversation, with their id (for cancel_reminder) and due time. Delivered reminders are NOT listed here: use list_recent_activity for those.",
		llm.NewJSONSchema(),
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			if _, err := t.authorize(ctx, identity, "reminder", "read"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("listing refused: %v", err)), nil
			}

			// Le magasin délégué ne remplace pas la liste, il s'y ajoute :
			// les rappels créés en base avant que l'agenda ne soit branché
			// y restent, et vont bel et bien partir. Les cacher les
			// rendrait à la fois invisibles et inannulables.
			store := t.eventStore(ctx, string(identity.OrgID), string(identity.PrincipalID))

			var reminders []persistence.Reminder
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				var err error
				reminders, err = t.Repo.ListPendingByConversation(ctx, tx, string(identity.ConversationID), persistence.ReminderKindMessage)
				return err
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not list the reminders: %v", err)), nil
			}

			var events []ScheduledEvent
			if store != nil {
				now := t.now()
				events, err = store.List(ctx, now, now.Add(reminderMaxHorizon))
				if err != nil {
					return llm.NewToolResult(fmt.Sprintf("could not list the reminders: %v", err)), nil
				}
			}

			if len(reminders) == 0 && len(events) == 0 {
				// Formulation prudente : la liste ne contient que ce qui
				// reste à venir. Un « aucun rappel » sec a déjà conduit
				// l'assistant à affirmer n'avoir jamais rien programmé,
				// alors que l'utilisateur venait de recevoir le rappel.
				return llm.NewToolResult("No reminder is scheduled ahead in this conversation. This says nothing about reminders already delivered — use list_recent_activity to see what was sent."), nil
			}

			var b strings.Builder
			b.WriteString("Pending reminders:\n")
			line := 0
			for _, rem := range reminders {
				line++
				if rem.Recurrence != "" {
					fmt.Fprintf(&b, "%d. [id: %s] repeating (%s, %s), next occurrence %s — %s\n", line, rem.ID, rem.Recurrence, rem.Timezone, rem.FireAt, rem.Message)
					continue
				}
				fmt.Fprintf(&b, "%d. [id: %s] %s — %s\n", line, rem.ID, rem.FireAt, rem.Message)
			}
			for _, ev := range events {
				line++
				if ev.Recurrence != "" {
					fmt.Fprintf(&b, "%d. [id: %s] repeating (%s, %s), next occurrence %s — %s\n", line, ev.ID, ev.Recurrence, ev.Timezone, ev.FireAt.Format(time.RFC3339), ev.Text)
					continue
				}
				fmt.Fprintf(&b, "%d. [id: %s] %s — %s\n", line, ev.ID, ev.FireAt.Format(time.RFC3339), ev.Text)
			}

			return llm.NewToolResult(strings.TrimSpace(b.String())), nil
		},
	)
}

func (t ReminderTools) newCancelReminderTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("id", "Id of the reminder to cancel, as returned by list_reminders or when it was created.", "string")

	return llm.NewFuncTool(
		"cancel_reminder",
		"Cancel a pending reminder of the current conversation.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			id := strings.TrimSpace(stringParam(params, "id"))
			if id == "" {
				return llm.NewToolResult("error: 'id' is required."), nil
			}

			if _, err := t.authorize(ctx, identity, "reminder", "delete"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("reminder cancellation refused: %v", err)), nil
			}

			// L'identifiant décide du magasin, pas l'inverse : on cherche
			// d'abord en base, puis dans le magasin délégué. L'ordre
			// importe — un rappel créé avant le branchement de l'agenda
			// doit rester annulable.
			store := t.eventStore(ctx, string(identity.OrgID), string(identity.PrincipalID))

			var outcome string
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				rem, found, err := t.Repo.FindByID(ctx, tx, persistence.ReminderID(id))
				if err != nil {
					return err
				}

				// Cloisonnement : un rappel n'est visible et annulable que
				// depuis la conversation où il a été créé — jamais depuis un
				// autre canal, même pour le même principal. Le filtre de
				// nature évite en outre qu'une tâche planifiée disparaisse
				// par un cancel_reminder.
				if !found || rem.ConversationID != identity.ConversationID || rem.Kind != persistence.ReminderKindMessage {
					// Inconnu en base : c'est peut-être un événement du
					// magasin délégué. outcome reste vide, la suite s'en
					// charge hors transaction.
					return nil
				}

				ok, err := t.Repo.UpdateStatus(ctx, tx, rem.ID, persistence.ReminderStatusPending, persistence.ReminderStatusCancelled, nil)
				if err != nil {
					return err
				}
				if !ok {
					outcome = fmt.Sprintf("reminder %q is no longer pending (already sent or cancelled).", id)
					return nil
				}

				outcome = fmt.Sprintf("Reminder %q cancelled.", id)
				return nil
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("could not cancel the reminder: %v", err)), nil
			}

			if outcome == "" {
				if store == nil {
					return llm.NewToolResult(fmt.Sprintf("no pending reminder with id %q in this conversation.", id)), nil
				}

				deleted, err := store.Delete(ctx, id)
				if err != nil {
					return llm.NewToolResult(fmt.Sprintf("could not cancel the reminder: %v", err)), nil
				}
				if !deleted {
					return llm.NewToolResult(fmt.Sprintf("no pending reminder with id %q in this conversation.", id)), nil
				}
				return llm.NewToolResult(fmt.Sprintf("Reminder %q cancelled.", id)), nil
			}

			return llm.NewToolResult(outcome), nil
		},
	)
}

// parseSchedule interprète les paramètres d'échéance communs aux rappels et
// aux tâches planifiées : soit 'recurrence' (cron + fuseau), soit 'fire_at'
// (RFC 3339). problem, non vide, est le message d'erreur à rendre au modèle
// — jamais une erreur Go : une échéance mal formulée est une chose que le
// modèle peut corriger lui-même au tour suivant.
func parseSchedule(params map[string]any, now time.Time) (fireAt time.Time, recurrence, timezone string, problem string) {
	recurrence = strings.TrimSpace(stringParam(params, "recurrence"))
	timezone = strings.TrimSpace(stringParam(params, "timezone"))

	if recurrence != "" {
		schedule, err := cron.ParseStandard(recurrence)
		if err != nil {
			return time.Time{}, "", "", fmt.Sprintf("error: invalid 'recurrence' (%v). Expected a standard 5-field cron expression, e.g. '0 20 * * 2'.", err)
		}

		loc := time.Local
		if timezone != "" {
			loc, err = time.LoadLocation(timezone)
			if err != nil {
				return time.Time{}, "", "", fmt.Sprintf("error: unknown 'timezone' (%v). Expected an IANA name, e.g. 'Europe/Paris'.", err)
			}
		} else {
			timezone = loc.String()
		}

		fireAt = schedule.Next(now.In(loc))
		if fireAt.IsZero() {
			return time.Time{}, "", "", "error: this cron expression has no future occurrence."
		}

		return fireAt, recurrence, timezone, ""
	}

	fireAt, err := time.Parse(time.RFC3339, strings.TrimSpace(stringParam(params, "fire_at")))
	if err != nil {
		return time.Time{}, "", "", fmt.Sprintf("error: invalid 'fire_at' (%v). Expected RFC 3339 with offset, e.g. 2026-08-18T09:00:00+02:00 — or set 'recurrence' for a repeating occurrence.", err)
	}
	if !fireAt.After(now) {
		return time.Time{}, "", "", fmt.Sprintf("error: 'fire_at' (%s) is already past (current time: %s).", fireAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if fireAt.Sub(now) > reminderMaxHorizon {
		return time.Time{}, "", "", "error: 'fire_at' is more than a year away; check the date before retrying."
	}

	return fireAt, "", "", ""
}
