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
// courante — rien de la liste de PLAN.md §10.1 (écritures externes,
// suppressions de données, écritures org). La destination de livraison
// n'est jamais un paramètre du modèle : elle est figée à la conversation
// courante au moment de la création.
type ReminderTools struct {
	DB         *persistence.DB
	Repo       *persistence.ReminderRepository
	Authorizer *authorization.Authorizer

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

	return []llm.Tool{
		t.newCreateReminderTool(identity),
		t.newListRemindersTool(identity),
		t.newCancelReminderTool(identity),
	}
}

// authorize vérifie la permission reminder.<scope>.<action> pour la portée
// de la conversation courante et retourne cette portée.
func (t ReminderTools) authorize(ctx context.Context, identity model.ExecutionIdentity, action string) (model.Scope, error) {
	scope, ok := writeScope(identity)
	if !ok {
		return "", fmt.Errorf("les rappels ne sont pas disponibles dans ce contexte d'exécution")
	}

	err := t.Authorizer.Authorize(ctx, authorization.AuthorizationRequest{
		Identity:      identity,
		Permission:    fmt.Sprintf("reminder.%s.%s", scope, action),
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
		Property("fire_at", "Date et heure d'un rappel PONCTUEL au format RFC 3339 avec décalage horaire (ex: 2026-08-18T09:00:00+02:00), calculée à partir de la date et l'heure courantes du contexte d'exécution. Ignoré si 'recurrence' est fourni.", "string").
		Property("recurrence", "Expression cron standard à 5 champs pour un rappel RÉCURRENT (ex: '0 20 * * 2' pour chaque mardi à 20h). L'application calcule elle-même la première occurrence.", "string").
		Property("timezone", "Fuseau IANA dans lequel évaluer 'recurrence' (ex: 'Europe/Paris'). Par défaut, le fuseau du serveur, visible dans la date courante du contexte d'exécution.", "string").
		RequiredProperty("message", "Texte du rappel, tel qu'il sera envoyé dans la conversation à chaque échéance.", "string")

	return llm.NewFuncTool(
		"create_reminder",
		"Programme un rappel dans la conversation courante : ponctuel avec 'fire_at', ou récurrent avec 'recurrence' (cron). Utiliser pour « rappelle-moi ... », « préviens-nous chaque ... ». Un rappel récurrent se répète jusqu'à son annulation (cancel_reminder).",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			message := strings.TrimSpace(stringParam(params, "message"))
			if message == "" {
				return llm.NewToolResult("erreur: le paramètre 'message' est requis et ne peut pas être vide."), nil
			}
			if len(message) > reminderMaxMessageLen {
				return llm.NewToolResult(fmt.Sprintf("erreur: message trop long (%d caractères, maximum %d).", len(message), reminderMaxMessageLen)), nil
			}

			now := t.now()
			recurrence := strings.TrimSpace(stringParam(params, "recurrence"))
			timezone := strings.TrimSpace(stringParam(params, "timezone"))

			var fireAt time.Time

			if recurrence != "" {
				schedule, err := cron.ParseStandard(recurrence)
				if err != nil {
					return llm.NewToolResult(fmt.Sprintf("erreur: 'recurrence' invalide (%v). Format attendu: expression cron standard à 5 champs, ex: '0 20 * * 2'.", err)), nil
				}

				loc := time.Local
				if timezone != "" {
					loc, err = time.LoadLocation(timezone)
					if err != nil {
						return llm.NewToolResult(fmt.Sprintf("erreur: 'timezone' inconnu (%v). Format attendu: nom IANA, ex: 'Europe/Paris'.", err)), nil
					}
				} else {
					timezone = loc.String()
				}

				fireAt = schedule.Next(now.In(loc))
				if fireAt.IsZero() {
					return llm.NewToolResult("erreur: cette expression cron n'a aucune occurrence future."), nil
				}
			} else {
				var err error
				fireAt, err = time.Parse(time.RFC3339, strings.TrimSpace(stringParam(params, "fire_at")))
				if err != nil {
					return llm.NewToolResult(fmt.Sprintf("erreur: 'fire_at' invalide (%v). Format attendu: RFC 3339 avec décalage horaire, ex: 2026-08-18T09:00:00+02:00 — ou fournir 'recurrence' pour un rappel récurrent.", err)), nil
				}

				if !fireAt.After(now) {
					return llm.NewToolResult(fmt.Sprintf("erreur: 'fire_at' (%s) est déjà passé (heure courante: %s).", fireAt.Format(time.RFC3339), now.Format(time.RFC3339))), nil
				}
				if fireAt.Sub(now) > reminderMaxHorizon {
					return llm.NewToolResult("erreur: 'fire_at' est à plus d'un an ; vérifier la date avant de réessayer."), nil
				}
			}

			if _, err := t.authorize(ctx, identity, "write"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("création de rappel refusée: %v", err)), nil
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
			}

			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				return t.Repo.Insert(ctx, tx, rem)
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("échec de la création du rappel: %v", err)), nil
			}

			t.Metrics.IncReminderCreated()

			if recurrence != "" {
				return llm.NewToolResult(fmt.Sprintf("Rappel récurrent programmé (%s, fuseau %s), prochaine occurrence %s (id: %s).", recurrence, timezone, fireAt.Format(time.RFC3339), rem.ID)), nil
			}

			return llm.NewToolResult(fmt.Sprintf("Rappel programmé pour %s (id: %s).", fireAt.Format(time.RFC3339), rem.ID)), nil
		},
	)
}

func (t ReminderTools) newListRemindersTool(identity model.ExecutionIdentity) llm.Tool {
	return llm.NewFuncTool(
		"list_reminders",
		"Liste les rappels en attente de la conversation courante, avec leur identifiant (utilisable par cancel_reminder) et leur échéance.",
		llm.NewJSONSchema(),
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			if _, err := t.authorize(ctx, identity, "read"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("liste des rappels refusée: %v", err)), nil
			}

			var reminders []persistence.Reminder
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				var err error
				reminders, err = t.Repo.ListPendingByConversation(ctx, tx, string(identity.ConversationID))
				return err
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("échec de la liste des rappels: %v", err)), nil
			}

			if len(reminders) == 0 {
				return llm.NewToolResult("Aucun rappel en attente dans cette conversation."), nil
			}

			var b strings.Builder
			b.WriteString("Rappels en attente :\n")
			for i, rem := range reminders {
				if rem.Recurrence != "" {
					fmt.Fprintf(&b, "%d. [id: %s] récurrent (%s, %s), prochaine occurrence %s — %s\n", i+1, rem.ID, rem.Recurrence, rem.Timezone, rem.FireAt, rem.Message)
					continue
				}
				fmt.Fprintf(&b, "%d. [id: %s] %s — %s\n", i+1, rem.ID, rem.FireAt, rem.Message)
			}

			return llm.NewToolResult(strings.TrimSpace(b.String())), nil
		},
	)
}

func (t ReminderTools) newCancelReminderTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("id", "Identifiant du rappel à annuler, obtenu via list_reminders ou lors de la création.", "string")

	return llm.NewFuncTool(
		"cancel_reminder",
		"Annule un rappel en attente de la conversation courante.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			id := strings.TrimSpace(stringParam(params, "id"))
			if id == "" {
				return llm.NewToolResult("erreur: le paramètre 'id' est requis."), nil
			}

			if _, err := t.authorize(ctx, identity, "delete"); err != nil {
				return llm.NewToolResult(fmt.Sprintf("annulation de rappel refusée: %v", err)), nil
			}

			var outcome string
			err := t.DB.WithTx(ctx, func(tx *sql.Tx) error {
				rem, found, err := t.Repo.FindByID(ctx, tx, persistence.ReminderID(id))
				if err != nil {
					return err
				}

				// Cloisonnement : un rappel n'est visible et annulable que
				// depuis la conversation où il a été créé — jamais depuis un
				// autre canal, même pour le même principal.
				if !found || rem.ConversationID != identity.ConversationID {
					outcome = fmt.Sprintf("aucun rappel en attente avec l'identifiant %q dans cette conversation.", id)
					return nil
				}

				ok, err := t.Repo.UpdateStatus(ctx, tx, rem.ID, persistence.ReminderStatusPending, persistence.ReminderStatusCancelled, nil)
				if err != nil {
					return err
				}
				if !ok {
					outcome = fmt.Sprintf("le rappel %q n'est plus en attente (déjà envoyé ou annulé).", id)
					return nil
				}

				outcome = fmt.Sprintf("Rappel %q annulé.", id)
				return nil
			})
			if err != nil {
				return llm.NewToolResult(fmt.Sprintf("échec de l'annulation du rappel: %v", err)), nil
			}

			return llm.NewToolResult(outcome), nil
		},
	)
}
