package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bornholm/genai/llm"
)

// Magasin d'événements planifiés fourni par un plugin. Quand un membre en
// a un — un agenda CalDAV branché, par exemple — ses rappels y sont rangés
// au lieu de la table reminders, et deviennent visibles et modifiables
// depuis n'importe quel client du même agenda.
//
// Les interfaces sont déclarées ici, côté consommateur, comme
// PluginToolCaller : le paquet agent ne dépend pas d'internal/plugin,
// c'est internal/registry qui les relie.
//
// Le modèle n'en sait rien et ne doit rien en savoir : les outils gardent
// les mêmes noms, les mêmes schémas et les mêmes garde-fous. Où le rappel
// est rangé est une affaire d'infrastructure, et un assistant informé du
// détail finirait par le raconter.

// EventStoreResolver rend le magasin d'événements d'un membre, ou nil
// quand aucun plugin actif n'en tient un pour lui.
type EventStoreResolver interface {
	EventStore(ctx context.Context, orgID, memberID string) EventStore
}

// EventStore est le magasin d'UN membre : la portée est déjà liée par le
// résolveur, aucun appel ne peut donc désigner quelqu'un d'autre.
type EventStore interface {
	// Put crée (ID vide) ou met à jour un événement et retourne son
	// identifiant.
	Put(ctx context.Context, event ScheduledEvent) (string, error)
	// Delete retire un événement. found faux désigne un identifiant
	// inconnu — pas une erreur.
	Delete(ctx context.Context, id string) (found bool, err error)
	// List retourne les événements de la fenêtre [from, to) ; un to nul ne
	// borne pas la fin.
	List(ctx context.Context, from, to time.Time) ([]ScheduledEvent, error)
	// Name est le plugin qui tient le magasin, pour les journaux et les
	// messages d'erreur destinés à la personne — jamais au modèle.
	Name() string
}

// ScheduledEvent est une entrée du magasin. Elle ne porte aucun état de
// livraison : ni statut, ni tentatives, ni canal. Ces informations restent
// dans la base de l'hôte, où elles ont un sens — l'agenda de quelqu'un
// n'est pas un journal d'expédition.
type ScheduledEvent struct {
	ID   string
	Text string
	// FireAt est la prochaine occurrence, en UTC.
	FireAt time.Time
	// Recurrence est une expression cron standard à 5 champs, vide pour un
	// déclenchement unique — le dialecte de l'hôte partout. Un magasin dont
	// le format diffère traduit, et refuse ce qu'il ne sait pas exprimer.
	Recurrence string
	// Timezone est le fuseau IANA dans lequel Recurrence s'évalue.
	Timezone string
}

// eventStore résout le magasin du membre courant. Il est relu à chaque
// appel d'outil, jamais mémorisé : un agenda débranché en cours de
// conversation doit cesser d'être utilisé au message suivant.
func (t ReminderTools) eventStore(ctx context.Context, orgID, memberID string) EventStore {
	if t.Events == nil || orgID == "" || memberID == "" {
		return nil
	}
	return t.Events.EventStore(ctx, orgID, memberID)
}

// duplicateEvent cherche dans le magasin un événement de même échéance et
// de même texte. Même garde-fou que FindPendingDuplicate côté base :
// l'assistant qui reformule sa réponse au tour suivant rappelle l'outil,
// et la personne recevait le rappel deux fois.
//
// La fenêtre est étroite à dessein — une minute autour de l'échéance :
// deux rappels identiques réellement voulus à des heures différentes
// restent deux rappels.
func duplicateEvent(events []ScheduledEvent, fireAt time.Time, text string) (ScheduledEvent, bool) {
	for _, ev := range events {
		delta := ev.FireAt.Sub(fireAt)
		if delta < -time.Minute || delta > time.Minute {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(ev.Text), strings.TrimSpace(text)) {
			return ev, true
		}
	}
	return ScheduledEvent{}, false
}

// createInEventStore range un rappel dans le magasin délégué et rend au
// modèle le même genre de réponse que la voie base de données : mêmes
// formulations, mêmes identifiants réutilisables par cancel_reminder.
func (t ReminderTools) createInEventStore(ctx context.Context, store EventStore, message string, fireAt time.Time, recurrence, timezone string) (llm.ToolResult, error) {
	// Dédoublonnage avant écriture, comme FindPendingDuplicate en base.
	// La fenêtre interrogée entoure l'échéance : inutile de rapatrier
	// l'agenda entier pour comparer une ligne.
	existing, err := store.List(ctx, fireAt.Add(-time.Minute), fireAt.Add(time.Minute))
	if err == nil {
		if dup, found := duplicateEvent(existing, fireAt, message); found {
			return llm.NewToolResult(fmt.Sprintf(
				"This reminder already exists, nothing was created: %s (id: %s). Tell the user it is already scheduled instead of announcing a new one.",
				dup.FireAt.Format(time.RFC3339), dup.ID)), nil
		}
	}
	// Une lecture en échec ne bloque pas la création : au pire un doublon,
	// jamais un rappel perdu.

	id, err := store.Put(ctx, ScheduledEvent{
		Text:       message,
		FireAt:     fireAt.UTC(),
		Recurrence: recurrence,
		Timezone:   timezone,
	})
	if err != nil {
		// Message franc plutôt que repli silencieux sur la base : la
		// personne doit savoir que son agenda n'a pas pris le rappel.
		return llm.NewToolResult(fmt.Sprintf("could not create the reminder: %v", err)), nil
	}

	t.Metrics.IncReminderCreated()

	if recurrence != "" {
		return llm.NewToolResult(fmt.Sprintf("Repeating reminder scheduled (%s, timezone %s), next occurrence %s (id: %s).", recurrence, timezone, fireAt.Format(time.RFC3339), id)), nil
	}
	return llm.NewToolResult(fmt.Sprintf("Reminder scheduled for %s (id: %s).", fireAt.Format(time.RFC3339), id)), nil
}
