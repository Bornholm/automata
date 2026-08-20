package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Plugin is the email assistant plugin: per-member mailbox over IMAP/SMTP,
// reading tools executed live, sending tools ALWAYS behind the host's
// human confirmation, and a watcher announcing incoming mail.
type Plugin struct {
	proto.UnimplementedAutomataPluginServer

	mu   sync.Mutex
	host pluginsdk.HostClient
	// sentKeys remembers idempotency keys already submitted: a replayed
	// confirmation must not send the email twice.
	sentKeys map[string]struct{}
	sentList []string
}

const sentKeysCap = 512

func newPlugin() *Plugin {
	return &Plugin{sentKeys: map[string]struct{}{}}
}

// SetHostClient implements pluginsdk.HostClientSetter.
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

// Describe implements proto.AutomataPluginServer.
func (p *Plugin) Describe(context.Context, *proto.DescribeRequest) (*proto.PluginDescriptor, error) {
	return &proto.PluginDescriptor{
		Name:             "email",
		Version:          "0.1.0",
		Description:      "Boîte mail personnelle : lecture, recherche, et réponses toujours soumises à confirmation.",
		PermissionDomain: "email",
		HasTriggers:      true,
		SubAgent: &proto.SubAgentDescriptor{
			SystemPrompt: "You are the user's email assistant. Use the email tools to read, search and summarize the mailbox. " +
				"Always cite emails by the id returned by the tools. Never invent an email address or an email id. " +
				"When drafting or replying, keep the user's tone, be concise, and remember that sending always requires " +
				"the user's explicit confirmation — never claim an email was sent.",
			Description: "reads, searches and summarizes the user's personal mailbox, and can draft or reply to emails " +
				"(sending always requires the user's confirmation)",
			MaxSequentialToolCalls: 6,
		},
	}, nil
}

// ListTools implements proto.AutomataPluginServer. The tool set follows
// the MEMBER's own switches: reading tools only if allow_read, sending
// tools only if allow_write. What the agent cannot see, it cannot ask
// for. Without a configured mailbox: no tools at all.
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
				Name:            "email_list_recent",
				Description:     "List the most recent emails of the inbox (id, from, subject, date).",
				InputSchemaJson: `{"type":"object","properties":{"count":{"type":"integer","description":"How many emails to list, at most 20.","default":5}}}`,
				ReadOnly:        true,
			},
			&proto.ToolDescriptor{
				Name:            "email_read",
				Description:     "Read one email in full, by the id returned by email_list_recent or email_search.",
				InputSchemaJson: `{"type":"object","properties":{"id":{"type":"string","description":"The email id."}},"required":["id"]}`,
				ReadOnly:        true,
			},
			&proto.ToolDescriptor{
				Name:            "email_search",
				Description:     "Search the inbox for emails matching the query (subject and body).",
				InputSchemaJson: `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`,
				ReadOnly:        true,
			})
	}
	if cfg.AllowWrite && cfg.sendComplete() {
		tools = append(tools,
			&proto.ToolDescriptor{
				Name:            "email_send",
				Description:     "Send a new email from the user's mailbox.",
				InputSchemaJson: `{"type":"object","properties":{"to":{"type":"string","description":"Recipient address."},"subject":{"type":"string"},"body":{"type":"string"}},"required":["to","subject","body"]}`,
			},
			&proto.ToolDescriptor{
				Name:            "email_reply",
				Description:     "Reply to an existing email, by its id. Threading headers are resolved automatically.",
				InputSchemaJson: `{"type":"object","properties":{"id":{"type":"string","description":"The email id to reply to."},"body":{"type":"string"}},"required":["id","body"]}`,
			})
	}

	return &proto.ListToolsOutput{Tools: tools}, nil
}

// CallTool implements proto.AutomataPluginServer.
func (p *Plugin) CallTool(ctx context.Context, in *proto.CallToolInput) (*proto.CallToolOutput, error) {
	host := p.hostClient()
	if host == nil || in.Ctx == nil {
		return toolError("plugin not initialized"), nil
	}

	cfg, password, errText := p.account(ctx, in.Ctx)
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
	case "email_list_recent":
		if !cfg.AllowRead {
			return toolError("reading is disabled for this mailbox"), nil
		}
		return p.listRecent(cfg, password, args)
	case "email_read":
		if !cfg.AllowRead {
			return toolError("reading is disabled for this mailbox"), nil
		}
		return p.read(cfg, password, args)
	case "email_search":
		if !cfg.AllowRead {
			return toolError("reading is disabled for this mailbox"), nil
		}
		return p.search(cfg, password, args)
	case "email_send":
		if !cfg.AllowWrite {
			return toolError("sending is disabled for this mailbox"), nil
		}
		return p.send(cfg, password, args, in.Ctx.IdempotencyKey, "")
	case "email_reply":
		if !cfg.AllowWrite {
			return toolError("sending is disabled for this mailbox"), nil
		}
		return p.reply(cfg, password, args, in.Ctx.IdempotencyKey)
	default:
		return toolError("unknown tool"), nil
	}
}

// account loads the member's configuration and password.
func (p *Plugin) account(ctx context.Context, cc *proto.CallContext) (memberConfig, string, string) {
	host := p.hostClient()

	raw, found, err := host.GetConfig(ctx, cc.OrgId, cc.MemberId)
	if err != nil || !found {
		return memberConfig{}, "", "no mailbox configured for this user"
	}
	cfg, err := parseConfig(raw)
	if err != nil || !cfg.complete() {
		return memberConfig{}, "", "the mailbox configuration is incomplete"
	}

	cred, err := credential(ctx, host, cc.OrgId, cc.MemberId, cfg)
	if err != nil {
		return memberConfig{}, "", err.Error()
	}

	return cfg, cred, ""
}

func (p *Plugin) listRecent(cfg memberConfig, password string, args map[string]any) (*proto.CallToolOutput, error) {
	count := 5
	if raw, ok := args["count"].(float64); ok && raw > 0 {
		count = int(raw)
	}
	if count > 20 {
		count = 20
	}

	client, err := dialIMAP(cfg, password)
	if err != nil {
		return toolError(err.Error()), nil
	}
	defer client.Close()

	summaries, err := listRecent(client, count)
	if err != nil {
		return toolError(err.Error()), nil
	}
	if len(summaries) == 0 {
		return &proto.CallToolOutput{ResultText: "The inbox is empty."}, nil
	}

	var b strings.Builder
	for _, s := range summaries {
		fmt.Fprintf(&b, "id=%d | from=%s | subject=%s | date=%s\n", s.UID, s.From, s.Subject, s.Date.Format("2006-01-02 15:04"))
	}
	return &proto.CallToolOutput{ResultText: b.String()}, nil
}

func (p *Plugin) read(cfg memberConfig, password string, args map[string]any) (*proto.CallToolOutput, error) {
	uid, ok := parseUID(args["id"])
	if !ok {
		return toolError("the 'id' argument must be an email id"), nil
	}

	client, err := dialIMAP(cfg, password)
	if err != nil {
		return toolError(err.Error()), nil
	}
	defer client.Close()

	content, err := readEmail(client, uid)
	if err != nil {
		return toolError(err.Error()), nil
	}

	text := fmt.Sprintf("id=%d\nFrom: %s\nTo: %s\nSubject: %s\nDate: %s\n\n%s",
		content.UID, content.From, strings.Join(content.To, ", "), content.Subject,
		content.Date.Format("2006-01-02 15:04"), content.Body)
	return &proto.CallToolOutput{ResultText: text}, nil
}

func (p *Plugin) search(cfg memberConfig, password string, args map[string]any) (*proto.CallToolOutput, error) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return toolError("the 'query' argument is required"), nil
	}

	client, err := dialIMAP(cfg, password)
	if err != nil {
		return toolError(err.Error()), nil
	}
	defer client.Close()

	summaries, err := searchText(client, query)
	if err != nil {
		return toolError(err.Error()), nil
	}
	if len(summaries) == 0 {
		return &proto.CallToolOutput{ResultText: "No email matches the query."}, nil
	}

	var b strings.Builder
	for _, s := range summaries {
		fmt.Fprintf(&b, "id=%d | from=%s | subject=%s | date=%s\n", s.UID, s.From, s.Subject, s.Date.Format("2006-01-02 15:04"))
	}
	return &proto.CallToolOutput{ResultText: b.String()}, nil
}

func (p *Plugin) send(cfg memberConfig, password string, args map[string]any, idempotencyKey, inReplyTo string) (*proto.CallToolOutput, error) {
	to, _ := args["to"].(string)
	subject, _ := args["subject"].(string)
	body, _ := args["body"].(string)
	if to == "" || body == "" {
		return toolError("the 'to' and 'body' arguments are required"), nil
	}

	if idempotencyKey != "" && !p.firstSubmission(idempotencyKey) {
		return &proto.CallToolOutput{ResultText: "This email was already sent (replayed confirmation)."}, nil
	}

	if err := sendEmail(cfg, password, to, subject, body, inReplyTo); err != nil {
		return toolError(err.Error()), nil
	}

	return &proto.CallToolOutput{ResultText: fmt.Sprintf("Email sent to %s.", to)}, nil
}

func (p *Plugin) reply(cfg memberConfig, password string, args map[string]any, idempotencyKey string) (*proto.CallToolOutput, error) {
	uid, ok := parseUID(args["id"])
	if !ok {
		return toolError("the 'id' argument must be an email id"), nil
	}
	body, _ := args["body"].(string)
	if body == "" {
		return toolError("the 'body' argument is required"), nil
	}

	// Threading resolved from the mailbox, never from the model.
	client, err := dialIMAP(cfg, password)
	if err != nil {
		return toolError(err.Error()), nil
	}
	original, err := readEmail(client, uid)
	_ = client.Close()
	if err != nil {
		return toolError(err.Error()), nil
	}

	subject := original.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}

	return p.send(cfg, password, map[string]any{
		"to":      original.From,
		"subject": subject,
		"body":    body,
	}, idempotencyKey, original.MessageID)
}

// firstSubmission records the idempotency key; false if already seen.
func (p *Plugin) firstSubmission(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, seen := p.sentKeys[key]; seen {
		return false
	}
	if len(p.sentList) >= sentKeysCap {
		oldest := p.sentList[0]
		p.sentList = p.sentList[1:]
		delete(p.sentKeys, oldest)
	}
	p.sentKeys[key] = struct{}{}
	p.sentList = append(p.sentList, key)
	return true
}

func parseUID(raw any) (uint32, bool) {
	switch v := raw.(type) {
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(n), true
	case float64:
		if v <= 0 {
			return 0, false
		}
		return uint32(v), true
	}
	return 0, false
}

func toolError(text string) *proto.CallToolOutput {
	return &proto.CallToolOutput{ResultText: text, IsError: true}
}

var _ = time.Now
