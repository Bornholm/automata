package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// WatchTriggers implements proto.AutomataPluginServer: it streams one
// TriggerEvent per new email. The host deduplicates by event id and
// re-checks activation and membership — this stream only announces.
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

// superviseAccounts refreshes the account list every minute (ListConfigs
// only returns organizations where the plugin is active) and runs one
// poller per configured account.
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
					if err != nil || !cfg.complete() || !cfg.AllowRead {
						continue
					}

					key := accountKey{entry.OrgID, entry.MemberID}
					seen[key] = struct{}{}
					if _, running := cancels[key]; running {
						continue
					}

					accountCtx, cancel := context.WithCancel(ctx)
					cancels[key] = cancel
					go p.pollAccount(accountCtx, entry.OrgID, entry.MemberID, events)
				}

				// Un compte disparu (config retirée, plugin désactivé
				// pour l'organisation) arrête son poller.
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

// pollAccount polls one mailbox and announces UIDs above the cursor. The
// cursor is persisted through SaveConfig after each announcement:
// at-least-once delivery, the host deduplicates.
func (p *Plugin) pollAccount(ctx context.Context, orgID, memberID string, events chan<- *proto.TriggerEvent) {
	for {
		interval := p.pollOnce(ctx, orgID, memberID, events)

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (p *Plugin) pollOnce(ctx context.Context, orgID, memberID string, events chan<- *proto.TriggerEvent) time.Duration {
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
	if err != nil || !cfg.complete() || !cfg.AllowRead {
		return fallback
	}
	interval := time.Duration(cfg.pollInterval()) * time.Second

	cred, err := credential(ctx, host, orgID, memberID, cfg)
	if err != nil {
		return interval
	}

	client, err := dialIMAP(cfg, cred)
	if err != nil {
		return interval
	}
	defer client.Close()

	// Première visite : se placer au bout de la boîte sans annoncer
	// l'historique entier.
	if !cfg.CursorReady {
		uids, err := searchAll(client)
		if err != nil {
			return interval
		}
		var max uint32
		for _, uid := range uids {
			if uid > max {
				max = uid
			}
		}
		cfg.LastUID = max
		cfg.CursorReady = true
		_ = host.SaveConfig(ctx, orgID, memberID, cfg.marshal())
		return interval
	}

	newer, err := searchSince(client, cfg.LastUID)
	if err != nil || len(newer) == 0 {
		return interval
	}

	summaries, err := fetchSummaries(client, newer)
	if err != nil {
		return interval
	}

	for i := len(summaries) - 1; i >= 0; i-- {
		s := summaries[i]
		event := &proto.TriggerEvent{
			Id:       fmt.Sprintf("email:%s:%s:INBOX:%d", orgID, memberID, s.UID),
			OrgId:    orgID,
			MemberId: memberID,
			Kind:     "email.received",
			// English, and no private content: the body travels through
			// email_read during the turn, never through the event.
			AgentInput:     triggerInput(s, cfg.Instructions),
			OccurredAtUnix: s.Date.Unix(),
		}

		select {
		case events <- event:
		case <-ctx.Done():
			return interval
		}

		if s.UID > cfg.LastUID {
			cfg.LastUID = s.UID
		}
	}

	_ = host.SaveConfig(ctx, orgID, memberID, cfg.marshal())

	return interval
}

// triggerInput compose la consigne du sous-agent pour un courriel reçu.
//
// Trois choses y tiennent ensemble, et l'ordre compte : ce qu'il faut faire,
// la permission de ne rien faire, puis les consignes de la personne — qui
// viennent en dernier parce qu'elles priment sur le reste.
//
// La permission de se taire est décisive. Sans elle, le sous-agent croit
// devoir rendre compte de tout et résume consciencieusement les pourriels ;
// une personne dérangée pour rien finit par ignorer aussi ce qui comptait.
//
// Anglais, et aucun contenu privé : ce texte est journalisable, le corps du
// message ne voyage que par email_read pendant le tour.
func triggerInput(s emailSummary, instructions string) string {
	input := fmt.Sprintf(
		"A new email arrived in the user's mailbox. From: %s. Subject: %s. "+
			"Use email_read with id %d to read it, then give the user a short summary in the user's language. "+
			"If the email clearly expects an answer, draft a reply with email_reply — it will require the user's confirmation.\n\n"+
			"Many emails do not deserve to interrupt anyone: advertising, newsletters, automated notices, "+
			"delivery receipts, alerts from services the user did not ask to hear from. "+
			"For those, report nothing at all.",
		s.From, s.Subject, s.UID)

	if trimmed := strings.TrimSpace(instructions); trimmed != "" {
		// Les consignes de la personne arrivent en dernier et l'emportent :
		// elle connaît sa boîte, et une règle générale ne peut pas deviner
		// que telle infolettre compte pour elle.
		input += "\n\nThe user has given you standing orders about their mail. " +
			"They take precedence over the guidance above, and over your own judgement:\n" + trimmed
	}

	return input
}
