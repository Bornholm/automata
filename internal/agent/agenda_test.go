package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
)

// --- serveur MCP HTTP (SSE) factice simulant google-calendar : un outil de
// lecture (list_events) et un outil d'écriture (create_event), même
// approche que internal/agent/mcp_tool_agent_test.go (Phase 12) et
// internal/mcp/manager_test.go (Phase 11). Aucun vrai serveur MCP Google
// Calendar n'est disponible dans ce dépôt (PLAN.md Phase 13).

type listEventsParams struct {
	CalendarID string `json:"calendar_id"`
	From       string `json:"from"`
	To         string `json:"to"`
}

type createEventParams struct {
	CalendarID string `json:"calendar_id"`
	Title      string `json:"title"`
	Start      string `json:"start"`
	End        string `json:"end"`
}

// calendarSpy enregistre les appels reçus côté serveur fake, notamment le
// calendar_id effectivement reçu (jamais celui prétendu par le modèle) :
// c'est la seule source de vérité fiable pour "ressource forgée refusée".
type calendarSpy struct {
	mu sync.Mutex

	listCalls   int
	createCalls int

	lastListCalendarID   string
	lastCreateCalendarID string
}

func (s *calendarSpy) recordList(calendarID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.lastListCalendarID = calendarID
}

func (s *calendarSpy) recordCreate(calendarID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.lastCreateCalendarID = calendarID
}

func (s *calendarSpy) snapshot() (listCalls, createCalls int, lastListCalendarID, lastCreateCalendarID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.createCalls, s.lastListCalendarID, s.lastCreateCalendarID
}

func newFakeCalendarServer(t *testing.T) (*httptest.Server, *calendarSpy) {
	t.Helper()

	spy := &calendarSpy{}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-calendar", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "list_events", Description: "list calendar events for a given range"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args listEventsParams) (*goMCP.CallToolResult, any, error) {
			spy.recordList(args.CalendarID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Aucun événement trouvé."}}}, nil, nil
		})

	goMCP.AddTool(server, &goMCP.Tool{Name: "create_event", Description: "create a calendar event"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args createEventParams) (*goMCP.CallToolResult, any, error) {
			spy.recordCreate(args.CalendarID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Événement créé avec succès."}}}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, spy
}

// testCalendarConfig construit une configuration avec trois canaux
// déclarant une ressource "calendar" distincte : personnel (alice), groupe
// (main-group) et org (home) — cette dernière portée n'a pas de structure
// dédiée dans le schéma de configuration (voir internal/resource) : c'est
// un canal scope: org qui en tient lieu, comme documenté dans
// ResolveCalendarID.
func testCalendarConfig(t *testing.T, calendarServerURL string) *config.Config {
	t.Helper()

	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		MCPServers: map[string]config.MCPServer{
			"google-calendar": {Transport: "http", URL: calendarServerURL},
		},
		Channels: []config.Channel{
			{
				Provider: "whatsapp", ChannelID: "alice-priv",
				Kind: config.ChannelKindPrivate, Scope: config.ScopePersonal, ScopeID: "alice",
				Resources: map[string]string{"calendar": "alice-personal-calendar"},
			},
			{
				Provider: "whatsapp", ChannelID: "group-chan",
				Kind: config.ChannelKindGroup, Scope: config.ScopeGroup, ScopeID: "main-group",
				Resources: map[string]string{"calendar": "main-group-calendar"},
			},
			{
				Provider: "whatsapp", ChannelID: "org-chan",
				Kind: config.ChannelKindGroup, Scope: config.ScopeOrg, ScopeID: "home",
				Resources: map[string]string{"calendar": "org-calendar"},
			},
		},
	}
}

func newAgendaAgent(t *testing.T, cfg *config.Config, client llm.ChatCompletionClient, store *agent.CalendarProposalStore, maxSequentialToolCalls int) agent.Agent {
	t.Helper()

	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	return agent.NewAgendaToolAgent(client, "system", "agenda", "Maison", m, []string{"google-calendar"}, mcp.Limits{}, maxSequentialToolCalls, cfg, store)
}

func privateConversation(id model.ConversationID) model.Conversation {
	return model.Conversation{ID: id, OrgID: "home", Provider: "whatsapp", ChannelID: "alice-priv", Kind: model.ChannelPrivate, Scope: model.ScopePersonal, ScopeID: "alice"}
}

func groupConversation(id model.ConversationID) model.Conversation {
	return model.Conversation{ID: id, OrgID: "home", Provider: "whatsapp", ChannelID: "group-chan", Kind: model.ChannelGroup, Scope: model.ScopeGroup, ScopeID: "main-group"}
}

func orgConversation(id model.ConversationID) model.Conversation {
	return model.Conversation{ID: id, OrgID: "home", Provider: "whatsapp", ChannelID: "org-chan", Kind: model.ChannelGroup, Scope: model.ScopeOrg, ScopeID: "home"}
}

func TestAgendaToolAgent_ReadPersonalFromPrivate(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"calendar_id":"","from":"2026-09-01","to":"2026-09-30"}`)), nil
			}
			return scriptedFinalResponse("Rien de prévu ce mois-ci."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: privateConversation("conv-1"), Input: "Qu'est-ce que j'ai de prévu ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	listCalls, createCalls, lastListCalendarID, _ := spy.snapshot()
	if listCalls != 1 {
		t.Fatalf("list_events aurait dû être appelé une fois sans confirmation, appelé %d fois", listCalls)
	}
	if createCalls != 0 {
		t.Fatalf("create_event n'aurait jamais dû être appelé, appelé %d fois", createCalls)
	}
	if lastListCalendarID != "alice-personal-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "alice-personal-calendar")
	}
	if !strings.Contains(result.Reply, "prévu") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

func TestAgendaToolAgent_ReadGroupFromGroup(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"from":"2026-09-01","to":"2026-09-30"}`)), nil
			}
			return scriptedFinalResponse("Rien de prévu pour le groupe."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: groupConversation("conv-2"), Input: "Qu'est-ce qu'on a de prévu ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID != "main-group-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "main-group-calendar")
	}
}

func TestAgendaToolAgent_PersonalNeverLeaksFromGroup(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	// Le modèle tente d'obtenir l'agenda personnel d'Alice depuis une
	// conversation de groupe : aucun paramètre du tool ne permet même de le
	// demander (calendar_id est toujours écrasé), donc ce test vérifie que
	// la conversation de groupe ne peut de toute façon jamais aboutir sur le
	// calendrier personnel.
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"calendar_id":"alice-personal-calendar","from":"2026-09-01","to":"2026-09-30"}`)), nil
			}
			return scriptedFinalResponse("Voilà."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: groupConversation("conv-3"), Input: "Donne-moi l'agenda perso d'Alice"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID != "main-group-calendar" {
		t.Fatalf("calendar_id reçu = %q, ne doit jamais être l'agenda personnel", lastListCalendarID)
	}
}

func TestAgendaToolAgent_OrgScopeResolvedFromConfiguredChannel(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"from":"2026-09-01","to":"2026-09-30"}`)), nil
			}
			return scriptedFinalResponse("Rien de prévu pour l'organisation."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: orgConversation("conv-4"), Input: "Agenda de l'organisation ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID != "org-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "org-calendar")
	}
}

func TestAgendaToolAgent_UnconfiguredScopeRejected(t *testing.T) {
	httpServer, _ := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			t.Fatal("le client llm ne devrait jamais être appelé si l'agenda n'est pas configuré pour la portée")
			return llm.ChatCompletionResponse(nil), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	// "bob" n'a aucun canal déclaré dans testCalendarConfig : aucune
	// ressource "calendar" n'est configurée pour cette portée.
	conv := model.Conversation{ID: "conv-5", OrgID: "home", Provider: "whatsapp", ChannelID: "bob-priv", Kind: model.ChannelPrivate, Scope: model.ScopePersonal, ScopeID: "bob"}

	_, err := a.Execute(context.Background(), agent.Request{Conversation: conv, Input: "Mon agenda ?"})
	if err == nil {
		t.Fatal("une erreur claire était attendue pour une portée sans agenda configuré")
	}
}

func TestAgendaToolAgent_ForgedCalendarIDIgnored(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"calendar_id":"forged-id-from-model","from":"2026-09-01","to":"2026-09-30"}`)), nil
			}
			return scriptedFinalResponse("Voilà."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: privateConversation("conv-6"), Input: "Mon agenda ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID == "forged-id-from-model" {
		t.Fatal("le calendar_id forgé par le modèle n'aurait jamais dû atteindre le serveur mcp")
	}
	if lastListCalendarID != "alice-personal-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "alice-personal-calendar")
	}
}

func TestAgendaToolAgent_CreateEventProposedNotExecuted(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "create_event", `{"title":"Dentiste","start":"2026-09-12T14:00:00+02:00","end":"2026-09-12T15:00:00+02:00"}`)), nil
			}
			return scriptedFinalResponse("Je te propose ce rendez-vous, confirme si ça te va."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: privateConversation("conv-7"), Input: "Ajoute un rendez-vous chez le dentiste"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, createCalls, _, _ := spy.snapshot()
	if createCalls != 0 {
		t.Fatalf("create_event n'aurait pas dû être exécuté réellement, appelé %d fois", createCalls)
	}
	if !strings.Contains(result.Reply, "propose") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

var proposalIDPattern = regexp.MustCompile(`ID de proposition: (\S+)`)

func TestAgendaToolAgent_ConfirmedCreateEventExecutesReally(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			switch turn {
			case 0:
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "create_event", `{"title":"Dentiste","start":"2026-09-12T14:00:00+02:00","end":"2026-09-12T15:00:00+02:00"}`)), nil
			case 1:
				proposalID := extractProposalID(t, opts)
				return scriptedToolCallResponse(llm.NewToolCall("call-2", "create_event", `{"proposal_id":"`+proposalID+`","confirm":true}`)), nil
			default:
				return scriptedFinalResponse("Rendez-vous confirmé."), nil
			}
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: privateConversation("conv-8"), Input: "Ajoute puis confirme le rendez-vous chez le dentiste"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, createCalls, _, lastCreateCalendarID := spy.snapshot()
	if createCalls != 1 {
		t.Fatalf("create_event aurait dû être exécuté réellement une fois après confirmation, appelé %d fois", createCalls)
	}
	if lastCreateCalendarID != "alice-personal-calendar" {
		t.Fatalf("calendar_id reçu à l'exécution = %q, attendu %q", lastCreateCalendarID, "alice-personal-calendar")
	}
	if !strings.Contains(result.Reply, "confirmé") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

// extractProposalID retrouve l'identifiant de proposition dans le dernier
// message "tool" des messages déjà échangés (le résultat du premier appel
// de create_event).
func extractProposalID(t *testing.T, opts *llm.ChatCompletionOptions) string {
	t.Helper()

	for i := len(opts.Messages) - 1; i >= 0; i-- {
		msg := opts.Messages[i]
		if msg.Role() != llm.RoleTool {
			continue
		}
		match := proposalIDPattern.FindStringSubmatch(msg.Content())
		if match != nil {
			return match[1]
		}
	}

	t.Fatal("aucun identifiant de proposition trouvé dans les messages")
	return ""
}

func TestAgendaToolAgent_ExpiredProposalRejected(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	current := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	store := agent.NewCalendarProposalStore()
	store.Now = func() time.Time { return current }

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			switch turn {
			case 0:
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "create_event", `{"title":"Dentiste","start":"2026-09-12T14:00:00+02:00","end":"2026-09-12T15:00:00+02:00"}`)), nil
			case 1:
				proposalID := extractProposalID(t, opts)
				// Dépasse largement les 5 minutes d'expiration avant de
				// confirmer.
				current = current.Add(10 * time.Minute)
				return scriptedToolCallResponse(llm.NewToolCall("call-2", "create_event", `{"proposal_id":"`+proposalID+`","confirm":true}`)), nil
			default:
				return scriptedFinalResponse("La proposition a expiré."), nil
			}
		},
	}

	a := newAgendaAgent(t, cfg, client, store, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: privateConversation("conv-9"), Input: "Ajoute le rendez-vous"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, createCalls, _, _ := spy.snapshot()
	if createCalls != 0 {
		t.Fatalf("create_event n'aurait jamais dû être exécuté après expiration, appelé %d fois", createCalls)
	}
	if !strings.Contains(result.Reply, "expiré") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

func TestAgendaToolAgent_AmbiguousDateRejected(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := testCalendarConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				// "start" sans fuseau horaire : ambigu, ne doit jamais être
				// proposé.
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "create_event", `{"title":"Dentiste","start":"2026-09-12T14:00:00","end":"2026-09-12T15:00:00+02:00"}`)), nil
			}
			return scriptedFinalResponse("Il me faut une date précise avec fuseau horaire."), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: privateConversation("conv-10"), Input: "Ajoute un rendez-vous demain à 14h"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, createCalls, _, _ := spy.snapshot()
	if createCalls != 0 {
		t.Fatalf("create_event n'aurait jamais dû être exécuté avec une date ambiguë, appelé %d fois", createCalls)
	}
}

func TestAgendaToolAgent_MCPServerUnreachableReturnsClearError(t *testing.T) {
	cfg := testCalendarConfig(t, "")
	cfg.MCPServers["google-calendar"] = config.MCPServer{Transport: "http", URL: "http://127.0.0.1:1"}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			t.Fatal("le client llm ne devrait jamais être appelé si le serveur mcp est injoignable")
			return llm.ChatCompletionResponse(nil), nil
		},
	}

	a := newAgendaAgent(t, cfg, client, nil, 5)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := a.Execute(ctx, agent.Request{Conversation: privateConversation("conv-11"), Input: "Mon agenda ?"})
	if err == nil {
		t.Fatal("une erreur claire était attendue lorsque le serveur mcp est injoignable")
	}
}
