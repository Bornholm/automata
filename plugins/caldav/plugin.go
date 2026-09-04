package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Plugin est l'assistant d'agenda : lecture et recherche d'un agenda
// CalDAV personnel, création d'événements derrière la confirmation
// humaine de l'hôte, et — quand le membre le demande — magasin des
// rappels de l'hôte à la place de sa table.
type Plugin struct {
	proto.UnimplementedAutomataPluginServer

	mu   sync.Mutex
	host pluginsdk.HostClient
	// writtenKeys retient les clés d'idempotence déjà honorées : une
	// confirmation rejouée ne doit pas créer l'événement deux fois.
	writtenKeys map[string]struct{}
	writtenList []string
	now         func() time.Time
}

const writtenKeysCap = 512

func newPlugin() *Plugin {
	return &Plugin{writtenKeys: map[string]struct{}{}, now: time.Now}
}

// SetHostClient implémente pluginsdk.HostClientSetter.
func (p *Plugin) SetHostClient(client pluginsdk.HostClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.host = client
}

func (p *Plugin) hostClient() pluginsdk.HostClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host
}

// Describe implémente proto.AutomataPluginServer.
func (p *Plugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:             "caldav",
		Version:          "0.1.0",
		Description:      "Agenda personnel CalDAV : lecture, recherche, et création d'événements soumise à confirmation. Peut aussi accueillir les rappels de l'assistant.",
		PermissionDomain: "calendar",
		HasTriggers:      true,
		// Le membre qui le demande voit ses rappels rangés dans son
		// agenda plutôt que dans la table de l'hôte.
		ProvidesEventStore: true,
		SubAgent: &proto.SubAgentDescriptor{
			SystemPrompt: "You are the user's calendar assistant. Use the calendar tools to look up, search and summarize what is on the agenda. " +
				"Always cite events by the id returned by the tools. Never invent an event, a date or an id. " +
				"When the user asks what they have on a given day, read the calendar rather than relying on anything said earlier in the conversation — " +
				"an agenda changes from other devices. Creating or removing an event always requires the user's explicit confirmation: " +
				"never claim an event was created or cancelled.",
			Description: "reads, searches and summarizes the user's personal calendar, and can prepare new events or cancel existing ones " +
				"(any change requires the user's confirmation)",
			MaxSequentialToolCalls: 6,
		},
	}, nil
}

// ListTools implémente proto.AutomataPluginServer : les outils suivent les
// interrupteurs du MEMBRE. Ce que l'agent ne voit pas, il ne peut pas le
// demander. Sans agenda configuré : aucun outil.
func (p *Plugin) ListTools(ctx context.Context, in *proto.ListToolsInput) (*proto.ListToolsOutput, error) {
	host := p.hostClient()
	if host == nil || in.Ctx == nil || in.Ctx.MemberId == "" {
		return &proto.ListToolsOutput{}, nil
	}

	raw, found, err := host.GetConfig(ctx, in.Ctx.OrgId, in.Ctx.MemberId)
	if err != nil || !found {
		return &proto.ListToolsOutput{}, nil
	}
	cfg, err := parseConfig(raw)
	if err != nil || !cfg.complete() {
		return &proto.ListToolsOutput{}, nil
	}

	var tools []*proto.ToolDescriptor
	if cfg.AllowRead {
		tools = append(tools,
			&proto.ToolDescriptor{
				Name:            "calendar_list_events",
				Description:     "List the events of the user's calendar between two dates (id, start, title). To answer a question about the PAST (\"what did I have this week?\"), pass an explicit 'from' in the past: the default window starts now and would show nothing.",
				InputSchemaJson: `{"type":"object","properties":{"from":{"type":"string","description":"Start of the window, RFC 3339 date-time with offset. Defaults to now, which leaves out everything earlier today and before."},"to":{"type":"string","description":"End of the window, RFC 3339 date-time with offset. Defaults to seven days after 'from'."}}}`,
				ReadOnly:        true,
			},
			&proto.ToolDescriptor{
				Name:            "calendar_search_events",
				Description:     "Search the user's calendar for events whose title matches the query, over the past year and the coming year.",
				InputSchemaJson: `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`,
				ReadOnly:        true,
			})
	}
	if cfg.AllowWrite {
		tools = append(tools,
			&proto.ToolDescriptor{
				Name:            "calendar_create_event",
				Description:     "Create an event in the user's calendar.",
				InputSchemaJson: `{"type":"object","properties":{"title":{"type":"string"},"start":{"type":"string","description":"RFC 3339 date-time with offset."},"duration_minutes":{"type":"integer","description":"Length of the event in minutes; defaults to 60."}},"required":["title","start"]}`,
			},
			&proto.ToolDescriptor{
				Name:            "calendar_cancel_event",
				Description:     "Remove an event from the user's calendar, by the id returned by calendar_list_events or calendar_search_events.",
				InputSchemaJson: `{"type":"object","properties":{"id":{"type":"string","description":"The event id."}},"required":["id"]}`,
			})
	}

	return &proto.ListToolsOutput{Tools: tools}, nil
}

// CallTool implémente proto.AutomataPluginServer.
func (p *Plugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	if in.Ctx == nil {
		return toolError("plugin not initialized"), nil
	}

	cfg, sess, errText := p.session(ctx, in.Ctx)
	if errText != "" {
		return toolError(errText), nil
	}

	var args map[string]any
	if in.ArgumentsJson != "" {
		if err := json.Unmarshal([]byte(in.ArgumentsJson), &args); err != nil {
			return toolError("unreadable arguments"), nil
		}
	}

	switch in.Name {
	case "calendar_list_events":
		if !cfg.AllowRead {
			return toolError("reading is disabled for this calendar"), nil
		}
		return p.listEvents(ctx, sess, args)
	case "calendar_search_events":
		if !cfg.AllowRead {
			return toolError("reading is disabled for this calendar"), nil
		}
		return p.searchEvents(ctx, sess, args)
	case "calendar_create_event":
		if !cfg.AllowWrite {
			return toolError("writing is disabled for this calendar"), nil
		}
		return p.createEvent(ctx, sess, args, in.Ctx.IdempotencyKey)
	case "calendar_cancel_event":
		if !cfg.AllowWrite {
			return toolError("writing is disabled for this calendar"), nil
		}
		return p.cancelEvent(ctx, sess, args)
	default:
		return toolError("unknown tool"), nil
	}
}

// session charge la configuration du membre et ouvre une session sur son
// agenda. Le texte d'erreur, non vide, est rendu au modèle tel quel.
func (p *Plugin) session(ctx context.Context, cc *proto.CallContext) (memberConfig, *session, string) {
	host := p.hostClient()
	if host == nil {
		return memberConfig{}, nil, "plugin not initialized"
	}

	raw, found, err := host.GetConfig(ctx, cc.OrgId, cc.MemberId)
	if err != nil || !found {
		return memberConfig{}, nil, "no calendar configured for this user"
	}
	cfg, err := parseConfig(raw)
	if err != nil || !cfg.complete() {
		return memberConfig{}, nil, "the calendar configuration is incomplete"
	}

	password, found, err := host.GetSecret(ctx, cc.OrgId, cc.MemberId, secretKeyPassword)
	if err != nil || !found {
		return memberConfig{}, nil, "the calendar password is not set"
	}

	sess, err := dial(ctx, cfg, password)
	if err != nil {
		// La cause part au journal de l'exploitant, pas seulement au
		// modèle : un échec de connexion sans trace est indiagnosticable
		// à distance, et c'est exactement ce qui manquait.
		logDialFailure(ctx, cfg, err)
		return memberConfig{}, nil, err.Error()
	}

	return cfg, sess, ""
}

// logDialFailure journalise un échec de connexion avec de quoi le
// comprendre : l'adresse du serveur, et la cause telle qu'elle remonte.
// Jamais l'identifiant ni le mot de passe.
func logDialFailure(ctx context.Context, cfg memberConfig, err error) {
	attrs := []any{
		"server", tlsServerName(cfg.ServerURL),
		"has_tls_exception", cfg.TLSFingerprint != "",
		"error", err.Error(),
	}
	// Un échec de certificat a sa propre issue — poser une exception — et
	// mérite d'être nommé comme tel plutôt que noyé dans « connexion
	// impossible ».
	if isCertificateError(err) {
		attrs = append(attrs, "cause", "certificat refusé")
	}

	slog.WarnContext(ctx, "caldav: connexion au serveur impossible", attrs...)
}

func (p *Plugin) listEvents(ctx context.Context, sess *session, args map[string]any) (*proto.CallToolOutput, error) {
	from := p.now().UTC()
	if raw, ok := args["from"].(string); ok && strings.TrimSpace(raw) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
		if err != nil {
			return toolError("the 'from' argument must be an RFC 3339 date-time with offset"), nil
		}
		from = parsed
	}

	to := from.AddDate(0, 0, 7)
	if raw, ok := args["to"].(string); ok && strings.TrimSpace(raw) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
		if err != nil {
			return toolError("the 'to' argument must be an RFC 3339 date-time with offset"), nil
		}
		to = parsed
	}

	objects, err := sess.query(ctx, from, to)
	if err != nil {
		return toolError(err.Error()), nil
	}

	var b strings.Builder
	for _, object := range objects {
		if line, ok := describeEvent(object); ok {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return &proto.CallToolOutput{ResultText: "No event in this window."}, nil
	}
	return &proto.CallToolOutput{ResultText: b.String()}, nil
}

// searchWindow borne la recherche par titre. Elle s'étend des deux côtés
// de l'instant présent : « quelles étaient mes réunions cette semaine ? »
// est une question aussi ordinaire que « qu'ai-je la semaine prochaine ? »,
// et une fenêtre qui commence maintenant y répond invariablement « aucune »
// — un vide indiscernable d'un agenda vraiment vide (signalé le
// 2026-09-04).
//
// Un an de part et d'autre : au-delà, la recherche ramènerait surtout des
// séries récurrentes anciennes, et le filtre textuel s'applique après le
// rapatriement.
func searchWindow(now time.Time) (from, to time.Time) {
	now = now.UTC()
	return now.AddDate(-1, 0, 0), now.AddDate(1, 0, 0)
}

func (p *Plugin) searchEvents(ctx context.Context, sess *session, args map[string]any) (*proto.CallToolOutput, error) {
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return toolError("the 'query' argument is required"), nil
	}

	from, to := searchWindow(p.now())
	objects, err := sess.query(ctx, from, to)
	if err != nil {
		return toolError(err.Error()), nil
	}

	// Le filtre textuel est appliqué ici : CalDAV sait filtrer sur le
	// texte, mais les serveurs le font de façon inégale, et la fenêtre est
	// déjà bornée à un an.
	var b strings.Builder
	for _, object := range objects {
		line, ok := describeEvent(object)
		if ok && strings.Contains(strings.ToLower(line), strings.ToLower(query)) {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return &proto.CallToolOutput{ResultText: "No event matches the query."}, nil
	}
	return &proto.CallToolOutput{ResultText: b.String()}, nil
}

func (p *Plugin) createEvent(ctx context.Context, sess *session, args map[string]any, idempotencyKey string) (*proto.CallToolOutput, error) {
	title, _ := args["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		return toolError("the 'title' argument is required"), nil
	}

	raw, _ := args["start"].(string)
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return toolError("the 'start' argument must be an RFC 3339 date-time with offset"), nil
	}

	duration := 60 * time.Minute
	if minutes, ok := args["duration_minutes"].(float64); ok && minutes > 0 {
		duration = time.Duration(minutes) * time.Minute
	}

	if idempotencyKey != "" && !p.firstSubmission(idempotencyKey) {
		return &proto.CallToolOutput{ResultText: "This event was already created (replayed confirmation)."}, nil
	}

	cal, uid, err := buildEvent(title, start, duration, p.now())
	if err != nil {
		return toolError(err.Error()), nil
	}
	if err := sess.put(ctx, uid, cal); err != nil {
		return toolError(err.Error()), nil
	}

	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Event created on %s (id: %s).", start.Format(time.RFC3339), uid)}, nil
}

func (p *Plugin) cancelEvent(ctx context.Context, sess *session, args map[string]any) (*proto.CallToolOutput, error) {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return toolError("the 'id' argument is required"), nil
	}

	removed, err := sess.remove(ctx, id)
	if err != nil {
		return toolError(err.Error()), nil
	}
	if !removed {
		return &proto.CallToolOutput{ResultText: fmt.Sprintf("No event with id %q in this calendar.", id)}, nil
	}
	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Event %q removed from the calendar.", id)}, nil
}

// firstSubmission enregistre la clé d'idempotence ; faux si déjà vue.
func (p *Plugin) firstSubmission(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, seen := p.writtenKeys[key]; seen {
		return false
	}
	if len(p.writtenList) >= writtenKeysCap {
		oldest := p.writtenList[0]
		p.writtenList = p.writtenList[1:]
		delete(p.writtenKeys, oldest)
	}
	p.writtenKeys[key] = struct{}{}
	p.writtenList = append(p.writtenList, key)
	return true
}

func toolError(text string) *proto.CallToolOutput {
	return &proto.CallToolOutput{ResultText: text, IsError: true}
}
