package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Magasin d'événements planifiés fourni par un plugin. Un membre dont un
// plugin actif tient le magasin voit ses rappels rangés là — dans un
// agenda CalDAV, par exemple — plutôt que dans la table reminders.
//
// Le partage est délibéré : le plugin détient l'horaire et le texte, ce
// qu'un agenda sait représenter et que la personne veut voir sur son
// téléphone ; l'hôte garde la livraison, que le calendrier de personne
// n'a à porter. C'est aussi ce qui préserve la garantie de non-doublon,
// qui repose côté hôte sur un dédoublonnage local et non sur un
// aller-retour vers un serveur distant qui peut être en panne au pire
// moment.

// eventStoreConfigKey est la clé réservée par laquelle un plugin annonce,
// dans la configuration d'un membre, qu'il tient le magasin pour lui.
//
// Une clé de configuration plutôt qu'un appel gRPC : la reprise est un
// réglage du membre, pas une propriété du plugin — un même plugin CalDAV
// sert des membres qui ont branché un agenda et d'autres non. L'hôte lit
// ce seul champ et reste ignorant de tout le reste du document.
const eventStoreConfigKey = "automata_event_store"

// eventStoreTimeout borne un appel au magasin : il est sur le chemin d'un
// outil, donc d'un tour de conversation.
const eventStoreTimeout = 30 * time.Second

// ScheduledEvent est une entrée du magasin : le texte à délivrer et son
// échéance. Aucun état de livraison — il reste côté hôte.
type ScheduledEvent struct {
	// ID est l'identifiant du plugin, opaque pour l'hôte. Vide à la
	// création.
	ID   string
	Text string
	// FireAt est la prochaine occurrence, en UTC.
	FireAt time.Time
	// Recurrence est une expression cron standard à 5 champs ; vide pour
	// un événement à déclenchement unique.
	Recurrence string
	// Timezone est le fuseau IANA dans lequel Recurrence s'évalue.
	Timezone string
}

// EventStoreFor retourne le nom du plugin qui tient le magasin
// d'événements de ce membre, s'il y en a un.
//
// Deux plugins revendiquant le même membre est une erreur de
// configuration : le premier dans l'ordre d'activation l'emporte et la
// collision est journalisée. Refuser tout serait pire — la personne
// perdrait ses rappels le temps de comprendre.
func (m *Manager) EventStoreFor(ctx context.Context, db dbTx, orgID, memberID string) (string, bool) {
	if orgID == "" || memberID == "" {
		return "", false
	}

	var claimants []string
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		enabled, err := m.hostService.activations.EnabledPlugins(ctx, tx, orgID)
		if err != nil {
			return err
		}

		for _, name := range enabled {
			if !m.providesEventStore(name) {
				continue
			}

			cfg, found, err := m.hostService.configs.Get(ctx, tx, name, orgID, memberID)
			if err != nil {
				return err
			}
			if !found {
				continue
			}

			// La configuration est scellée au repos comme celle de tout
			// plugin : la lire ici passe par la même boîte que le
			// service hôte.
			clear, err := m.hostService.box.OpenText(cfg.Config)
			if err != nil {
				slog.WarnContext(ctx, "plugin: configuration illisible, magasin d'événements ignoré",
					"plugin", name, "org_id", orgID)
				continue
			}
			if eventStoreClaimed(clear) {
				claimants = append(claimants, name)
			}
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "plugin: résolution du magasin d'événements échouée",
			"org_id", orgID, "error", err)
		return "", false
	}

	if len(claimants) == 0 {
		return "", false
	}
	if len(claimants) > 1 {
		slog.WarnContext(ctx, "plugin: plusieurs magasins d'événements revendiqués, le premier l'emporte",
			"org_id", orgID, "plugins", claimants)
	}
	return claimants[0], true
}

// providesEventStore lit le descripteur déjà en mémoire : aucun appel
// gRPC, la résolution est sur le chemin de chaque outil de rappel.
func (m *Manager) providesEventStore(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, entry := m.findEntry(name)
	return entry != nil && entry.Descriptor != nil && entry.Descriptor.ProvidesEventStore
}

// eventStoreClaimed cherche la clé réservée dans la configuration du
// membre. Un document illisible ne revendique rien : l'hôte retombe sur
// sa propre table, ce qui est le comportement sûr.
func eventStoreClaimed(configJSON string) bool {
	if configJSON == "" {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(configJSON), &probe); err != nil {
		return false
	}
	raw, ok := probe[eventStoreConfigKey]
	if !ok {
		return false
	}
	var claimed bool
	return json.Unmarshal(raw, &claimed) == nil && claimed
}

// PutEvent crée (ID vide) ou met à jour un événement et retourne son
// identifiant côté plugin.
func (m *Manager) PutEvent(ctx context.Context, pluginName string, callCtx CallContext, event ScheduledEvent) (string, error) {
	client, err := m.eventStoreClient(ctx, pluginName)
	if err != nil {
		return "", err
	}

	callTimeout, cancel := context.WithTimeout(ctx, eventStoreTimeout)
	defer cancel()

	out, err := client.PutEvent(callTimeout, &proto.PutEventRequest{
		Ctx:   toProtoContext(callCtx),
		Event: toProtoEvent(event),
	})
	if err != nil {
		return "", fmt.Errorf("enregistrement de l'événement dans %q: %w", pluginName, err)
	}
	if out.IsError {
		return "", fmt.Errorf("%s", out.ErrorText)
	}

	// Identifiants seulement : jamais le texte de l'événement.
	slog.InfoContext(ctx, "plugin: événement enregistré dans le magasin du plugin",
		"plugin", pluginName, "org_id", callCtx.OrgID, "recurring", event.Recurrence != "")

	return out.Id, nil
}

// DeleteEvent retire un événement. found faux désigne un identifiant
// inconnu — pas une erreur.
func (m *Manager) DeleteEvent(ctx context.Context, pluginName string, callCtx CallContext, id string) (bool, error) {
	client, err := m.eventStoreClient(ctx, pluginName)
	if err != nil {
		return false, err
	}

	callTimeout, cancel := context.WithTimeout(ctx, eventStoreTimeout)
	defer cancel()

	out, err := client.DeleteEvent(callTimeout, &proto.DeleteEventRequest{
		Ctx: toProtoContext(callCtx),
		Id:  id,
	})
	if err != nil {
		return false, fmt.Errorf("suppression de l'événement dans %q: %w", pluginName, err)
	}
	if out.IsError {
		return false, fmt.Errorf("%s", out.ErrorText)
	}

	return out.Found, nil
}

// ListEvents liste les événements du membre dans la fenêtre [from, to).
// Un to nul ne borne pas la fin.
func (m *Manager) ListEvents(ctx context.Context, pluginName string, callCtx CallContext, from, to time.Time) ([]ScheduledEvent, error) {
	client, err := m.eventStoreClient(ctx, pluginName)
	if err != nil {
		return nil, err
	}

	callTimeout, cancel := context.WithTimeout(ctx, eventStoreTimeout)
	defer cancel()

	req := &proto.ListEventsRequest{
		Ctx:      toProtoContext(callCtx),
		FromUnix: from.UTC().Unix(),
	}
	if !to.IsZero() {
		req.ToUnix = to.UTC().Unix()
	}

	out, err := client.ListEvents(callTimeout, req)
	if err != nil {
		return nil, fmt.Errorf("liste des événements de %q: %w", pluginName, err)
	}
	if out.IsError {
		return nil, fmt.Errorf("%s", out.ErrorText)
	}

	events := make([]ScheduledEvent, 0, len(out.Events))
	for _, ev := range out.Events {
		events = append(events, ScheduledEvent{
			ID:         ev.Id,
			Text:       ev.Text,
			FireAt:     time.Unix(ev.FireAtUnix, 0).UTC(),
			Recurrence: ev.Recurrence,
			Timezone:   ev.Timezone,
		})
	}
	return events, nil
}

// eventStoreClient rend le client d'un plugin dont le descripteur déclare
// bien un magasin : sans ce contrôle, l'hôte appellerait des RPC qu'un
// plugin ordinaire n'implémente pas.
func (m *Manager) eventStoreClient(ctx context.Context, pluginName string) (proto.AutomataPluginClient, error) {
	client, desc, ok := m.GetOrRestart(ctx, pluginName)
	if !ok {
		return nil, fmt.Errorf("plugin %q indisponible", pluginName)
	}
	if desc == nil || !desc.ProvidesEventStore {
		return nil, fmt.Errorf("le plugin %q ne fournit pas de magasin d'événements", pluginName)
	}
	return client, nil
}

func toProtoEvent(ev ScheduledEvent) *proto.ScheduledEvent {
	return &proto.ScheduledEvent{
		Id:         ev.ID,
		Text:       ev.Text,
		FireAtUnix: ev.FireAt.UTC().Unix(),
		Recurrence: ev.Recurrence,
		Timezone:   ev.Timezone,
	}
}
