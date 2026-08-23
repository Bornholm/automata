package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Surveillance des échéances : le plugin détient l'horaire, c'est donc à
// lui d'annoncer qu'une occurrence est arrivée. L'hôte ne balaie plus
// rien pour ces membres — sans quoi il interrogerait des serveurs CalDAV
// distants toutes les trente secondes, pour tous les comptes.
//
// L'annonce porte le texte du rappel dans DeliverText : l'hôte le livre
// mot pour mot, sans tour de modèle. Un pense-bête que la personne a
// écrit elle-même n'a pas à être reformulé, ni payé au prix d'un appel de
// LLM.

// WatchTriggers implémente proto.AutomataPluginServer.
func (p *Plugin) WatchTriggers(_ *proto.WatchTriggersRequest, stream proto.AutomataPlugin_WatchTriggersServer) error {
	ctx := stream.Context()

	events := make(chan *proto.TriggerEvent, 16)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.superviseAccounts(ctx, events)
	}()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case event := <-events:
			if err := stream.Send(event); err != nil {
				wg.Wait()
				return err
			}
		}
	}
}

// superviseAccounts rafraîchit la liste des comptes chaque minute
// (ListConfigs ne rend que les organisations où le plugin est actif) et
// tient un balayeur par compte ayant confié ses rappels à son agenda.
func (p *Plugin) superviseAccounts(ctx context.Context, events chan<- *proto.TriggerEvent) {
	type accountKey struct{ org, member string }
	cancels := map[accountKey]context.CancelFunc{}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		host := p.hostClient()
		if host != nil {
			entries, err := host.ListConfigs(ctx)
			if err == nil {
				seen := map[accountKey]struct{}{}
				for _, entry := range entries {
					if entry.MemberID == "" {
						continue
					}
					cfg, err := parseConfig(entry.ConfigJSON)
					// Un compte qui n'a pas confié ses rappels à son
					// agenda n'a rien à annoncer : ses échéances sont
					// tenues par l'hôte, qui les délivre lui-même.
					if err != nil || !cfg.complete() || !cfg.EventStore {
						continue
					}

					key := accountKey{entry.OrgID, entry.MemberID}
					seen[key] = struct{}{}
					if _, running := cancels[key]; running {
						continue
					}

					accountCtx, cancel := context.WithCancel(ctx)
					cancels[key] = cancel
					go p.sweepAccount(accountCtx, entry.OrgID, entry.MemberID, events)
				}

				for key, cancel := range cancels {
					if _, still := seen[key]; !still {
						cancel()
						delete(cancels, key)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Plugin) sweepAccount(ctx context.Context, orgID, memberID string, events chan<- *proto.TriggerEvent) {
	for {
		interval := p.sweepOnce(ctx, orgID, memberID, events)

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// sweepOnce annonce les occurrences échues depuis le curseur, puis avance
// le curseur. L'ordre est délibéré : annoncer d'abord, enregistrer
// ensuite. Un plantage entre les deux rejouera l'annonce — l'hôte
// dédoublonne par identifiant d'événement — là où l'inverse perdrait le
// rappel sans un mot.
func (p *Plugin) sweepOnce(ctx context.Context, orgID, memberID string, events chan<- *proto.TriggerEvent) time.Duration {
	host := p.hostClient()
	fallback := time.Duration(defaultPollSeconds) * time.Second
	if host == nil {
		return fallback
	}

	raw, found, err := host.GetConfig(ctx, orgID, memberID)
	if err != nil || !found {
		return fallback
	}
	cfg, err := parseConfig(raw)
	if err != nil || !cfg.complete() || !cfg.EventStore {
		return fallback
	}
	interval := time.Duration(cfg.pollInterval()) * time.Second

	password, found, err := host.GetSecret(ctx, orgID, memberID, secretKeyPassword)
	if err != nil || !found {
		return interval
	}

	sess, err := dial(ctx, cfg, password)
	if err != nil {
		slog.WarnContext(ctx, "caldav: agenda injoignable", "org_id", orgID, "error", err)
		return interval
	}

	now := p.now().UTC()

	// Premier balayage : le curseur s'ancre au présent sans rien
	// annoncer. Sans cette ancre, brancher un agenda déjà rempli
	// déverserait d'un coup tous les rappels passés de l'année.
	cursor, anchored := parseCursor(cfg.LastSweep)
	if !anchored {
		cfg.LastSweep = now.Format(time.RFC3339)
		_ = host.SaveConfig(ctx, orgID, memberID, cfg.marshal())
		return interval
	}

	// Rattrapage borné : après un long arrêt, on ne remonte pas plus loin
	// qu'un jour. Un rappel du matin délivré l'après-midi a de la valeur ;
	// celui d'avant-hier n'en a plus, il n'a que la nuisance.
	if floor := now.Add(-24 * time.Hour); cursor.Before(floor) {
		cursor = floor
	}

	due, err := p.reminders(ctx, sess, cursor, now)
	if err != nil {
		slog.WarnContext(ctx, "caldav: lecture des échéances échouée", "org_id", orgID, "error", err)
		return interval
	}

	for _, rem := range due {
		if rem.FireAt.Before(cursor) || rem.FireAt.After(now) {
			continue
		}

		select {
		case events <- dueEvent(orgID, memberID, rem):
		case <-ctx.Done():
			return interval
		}
	}

	cfg.LastSweep = now.Format(time.RFC3339)
	if err := host.SaveConfig(ctx, orgID, memberID, cfg.marshal()); err != nil {
		slog.WarnContext(ctx, "caldav: curseur non enregistré", "org_id", orgID, "error", err)
	}

	return interval
}

// dueEvent fabrique l'annonce d'une échéance. L'identifiant mêle l'UID et
// l'occurrence : une série doit pouvoir sonner chaque semaine sans que
// l'hôte prenne la deuxième pour un doublon de la première.
func dueEvent(orgID, memberID string, rem reminderEvent) *proto.TriggerEvent {
	return &proto.TriggerEvent{
		Id:             fmt.Sprintf("%s@%d", rem.UID, rem.FireAt.Unix()),
		OrgId:          orgID,
		MemberId:       memberID,
		Kind:           "calendar.reminder_due",
		OccurredAtUnix: rem.FireAt.Unix(),
		// Le texte part mot pour mot : c'est celui que la personne a
		// écrit. Il ne passe ni par un modèle, ni par un journal.
		DeliverText: rem.Text,
	}
}

func parseCursor(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
