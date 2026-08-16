package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"
	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
)

// --- serveur MCP HTTP (SSE) factice simulant un gestionnaire de tâches :
// list_tasks (lecture) et create_task (écriture, avec échec simulé si
// title == "FAIL"). Même approche que internal/agent/agenda_test.go (Phase
// 13) : aucun vrai serveur MCP de gestion de tâches n'est disponible dans ce
// dépôt (PLAN.md Phase 14).

type listTasksParams struct {
	ListID string `json:"list_id"`
}

type createTaskParams struct {
	ListID string `json:"list_id"`
	Title  string `json:"title"`
}

// todoSpy enregistre les appels reçus côté serveur fake, notamment le
// list_id effectivement reçu (jamais celui prétendu par le modèle).
type todoSpy struct {
	mu sync.Mutex

	listCalls   int
	createCalls int

	lastListListID string
	createdListIDs []string
	createdTitles  []string
}

func (s *todoSpy) recordList(listID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.lastListListID = listID
}

func (s *todoSpy) recordCreate(listID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.createdListIDs = append(s.createdListIDs, listID)
	s.createdTitles = append(s.createdTitles, title)
}

func (s *todoSpy) snapshot() (listCalls, createCalls int, lastListListID string, createdListIDs, createdTitles []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.createCalls, s.lastListListID, append([]string(nil), s.createdListIDs...), append([]string(nil), s.createdTitles...)
}

func newFakeTodoServer(t *testing.T) (*httptest.Server, *todoSpy) {
	t.Helper()

	spy := &todoSpy{}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-todo", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "list_tasks", Description: "list tasks of a given list"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args listTasksParams) (*goMCP.CallToolResult, any, error) {
			spy.recordList(args.ListID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Aucune tâche trouvée."}}}, nil, nil
		})

	goMCP.AddTool(server, &goMCP.Tool{Name: "create_task", Description: "create a task in a list"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args createTaskParams) (*goMCP.CallToolResult, any, error) {
			spy.recordCreate(args.ListID, args.Title)
			if args.Title == "FAIL" {
				return nil, nil, errFakeTodoCreateFailed
			}
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Tâche créée avec succès."}}}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, spy
}

var errFakeTodoCreateFailed = &fakeTodoCreateError{}

type fakeTodoCreateError struct{}

func (e *fakeTodoCreateError) Error() string {
	return "titre refusé par le serveur (simulation d'échec)"
}

// testTodoConfig construit une configuration avec deux canaux déclarant une
// ressource "todo" distincte : personnel (alice) et groupe (main-group).
func testTodoConfig(t *testing.T, todoServerURL string) *config.Config {
	t.Helper()

	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		MCPServers: map[string]config.MCPServer{
			"todo": {Transport: "http", URL: todoServerURL},
		},
		Channels: []config.Channel{
			{
				Provider: "whatsapp", ChannelID: "alice-priv",
				Kind: config.ChannelKindPrivate, Scope: config.ScopePersonal, ScopeID: "alice",
				Resources: map[string]string{"todo": "alice-personal-list"},
			},
			{
				Provider: "whatsapp", ChannelID: "group-chan",
				Kind: config.ChannelKindGroup, Scope: config.ScopeGroup, ScopeID: "main-group",
				Resources: map[string]string{"todo": "main-group-list"},
			},
		},
	}
}

func newTodoAgent(t *testing.T, cfg *config.Config, client llm.ChatCompletionClient, maxSequentialToolCalls int) agent.Agent {
	t.Helper()

	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	return agent.NewTodoToolAgent(client, "system", "todo", "Maison", m, []string{"todo"}, mcp.Limits{}, maxSequentialToolCalls, cfg)
}

func todoPrivateConversation(id model.ConversationID) model.Conversation {
	return model.Conversation{ID: id, OrgID: "home", Provider: "whatsapp", ChannelID: "alice-priv", Kind: model.ChannelPrivate, Scope: model.ScopePersonal, ScopeID: "alice"}
}

func todoGroupConversation(id model.ConversationID) model.Conversation {
	return model.Conversation{ID: id, OrgID: "home", Provider: "whatsapp", ChannelID: "group-chan", Kind: model.ChannelGroup, Scope: model.ScopeGroup, ScopeID: "main-group"}
}

func TestTodoToolAgent_ReadPersonalFromPrivate(t *testing.T) {
	httpServer, spy := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_tasks", `{"list_id":""}`)), nil
			}
			return scriptedFinalResponse("Aucune tâche en attente."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: todoPrivateConversation("conv-1"), Input: "Qu'est-ce que j'ai à faire ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	listCalls, createCalls, lastListListID, _, _ := spy.snapshot()
	if listCalls != 1 {
		t.Fatalf("list_tasks aurait dû être appelé une fois sans confirmation, appelé %d fois", listCalls)
	}
	if createCalls != 0 {
		t.Fatalf("create_task n'aurait jamais dû être appelé, appelé %d fois", createCalls)
	}
	if lastListListID != "alice-personal-list" {
		t.Fatalf("list_id reçu = %q, attendu %q", lastListListID, "alice-personal-list")
	}
	if !strings.Contains(result.Reply, "tâche") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

func TestTodoToolAgent_ReadGroupFromGroup(t *testing.T) {
	httpServer, spy := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_tasks", `{}`)), nil
			}
			return scriptedFinalResponse("Rien à faire pour le groupe."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: todoGroupConversation("conv-2"), Input: "Qu'est-ce qu'on a à faire ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListListID, _, _ := spy.snapshot()
	if lastListListID != "main-group-list" {
		t.Fatalf("list_id reçu = %q, attendu %q", lastListListID, "main-group-list")
	}
}

func TestTodoToolAgent_PersonalNeverLeaksFromGroup(t *testing.T) {
	httpServer, spy := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	// Le modèle tente d'obtenir la liste personnelle d'Alice depuis une
	// conversation de groupe : list_id est toujours écrasé, donc ce test
	// vérifie que la conversation de groupe n'aboutit jamais sur la liste
	// personnelle.
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_tasks", `{"list_id":"alice-personal-list"}`)), nil
			}
			return scriptedFinalResponse("Voilà."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: todoGroupConversation("conv-3"), Input: "Donne-moi la liste perso d'Alice"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListListID, _, _ := spy.snapshot()
	if lastListListID != "main-group-list" {
		t.Fatalf("list_id reçu = %q, ne doit jamais être la liste personnelle", lastListListID)
	}
}

func TestTodoToolAgent_ForgedListIDIgnored(t *testing.T) {
	httpServer, spy := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_tasks", `{"list_id":"forged-id-from-model"}`)), nil
			}
			return scriptedFinalResponse("Voilà."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: todoPrivateConversation("conv-4"), Input: "Ma liste ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, _, lastListListID, _, _ := spy.snapshot()
	if lastListListID == "forged-id-from-model" {
		t.Fatal("le list_id forgé par le modèle n'aurait jamais dû atteindre le serveur mcp")
	}
	if lastListListID != "alice-personal-list" {
		t.Fatalf("list_id reçu = %q, attendu %q", lastListListID, "alice-personal-list")
	}
}

func TestTodoToolAgent_MultipleCreatesProposedNotExecuted(t *testing.T) {
	httpServer, spy := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(
					llm.NewToolCall("call-1", "create_task", `{"title":"Acheter du pain"}`),
					llm.NewToolCall("call-2", "create_task", `{"title":"Appeler le plombier"}`),
					llm.NewToolCall("call-3", "create_task", `{"title":"Réserver le resto"}`),
				), nil
			}
			return scriptedFinalResponse("Je te propose ces trois tâches, confirme si ça te va."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: todoPrivateConversation("conv-5"), Input: "Ajoute ces trois tâches"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, createCalls, _, _, _ := spy.snapshot()
	if createCalls != 0 {
		t.Fatalf("create_task n'aurait jamais dû être exécuté réellement avant confirmation, appelé %d fois", createCalls)
	}
	if !strings.Contains(result.Reply, "propose") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}

	// Les trois actions remontent ensemble à l'orchestrateur : elles
	// formeront UN seul plan, confirmé d'un coup et exécuté séquentiellement
	// dans cet ordre par internal/action.Engine (PLAN.md §6.4, Phase 15).
	if len(result.ProposedActions) != 3 {
		t.Fatalf("actions proposées: got %d, expected 3", len(result.ProposedActions))
	}

	wantTitles := []string{"Acheter du pain", "Appeler le plombier", "Réserver le resto"}
	for i, want := range wantTitles {
		got := result.ProposedActions[i]

		if got.Arguments["title"] != want {
			t.Errorf("action %d: title = %v, attendu %q (l'ordre de proposition doit être préservé)", i+1, got.Arguments["title"], want)
		}
		if got.ToolName != "create_task" {
			t.Errorf("action %d: tool_name = %q, attendu create_task", i+1, got.ToolName)
		}
		if got.RequiredPermission != "todo.personal.write" {
			t.Errorf("action %d: permission = %q, attendu todo.personal.write", i+1, got.RequiredPermission)
		}
		// Résolu à la confirmation, jamais figé (PLAN.md §10.5 point 6).
		if _, present := got.Arguments["list_id"]; present {
			t.Errorf("action %d: list_id ne doit jamais être figé dans une action proposée", i+1)
		}
	}
}

func TestTodoToolAgent_DuplicateCreateProposalIsDeduped(t *testing.T) {
	httpServer, spy := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				// Deux propositions de création strictement identiques dans
				// le même tour.
				return scriptedToolCallResponse(
					llm.NewToolCall("call-1", "create_task", `{"title":"Acheter du pain"}`),
					llm.NewToolCall("call-2", "create_task", `{"title":"Acheter du pain"}`),
				), nil
			}
			return scriptedFinalResponse("Une seule tâche proposée, le doublon a été évité."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: todoPrivateConversation("conv-9"), Input: "Ajoute deux fois la même tâche"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	_, createCalls, _, _, _ := spy.snapshot()
	if createCalls != 0 {
		t.Fatalf("aucune création réelle n'aurait dû avoir lieu avant confirmation, appelé %d fois", createCalls)
	}

	if got := len(result.ProposedActions); got != 1 {
		t.Fatalf("actions proposées: got %d, expected 1 (le doublon du tour doit être écarté)", got)
	}
}

// TestTodoToolAgent_DistinctCreatesAreNotDeduped garde la contrepartie du
// test précédent : la déduplication ne doit écarter que des créations
// réellement identiques, jamais deux tâches distinctes.
func TestTodoToolAgent_DistinctCreatesAreNotDeduped(t *testing.T) {
	httpServer, _ := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(
					llm.NewToolCall("call-1", "create_task", `{"title":"Acheter du pain"}`),
					llm.NewToolCall("call-2", "create_task", `{"title":"Acheter du lait"}`),
				), nil
			}
			return scriptedFinalResponse("Deux tâches proposées."), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: todoPrivateConversation("conv-10"), Input: "Ajoute deux tâches"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if got := len(result.ProposedActions); got != 2 {
		t.Fatalf("actions proposées: got %d, expected 2", got)
	}
}

func TestTodoToolAgent_UnconfiguredScopeRejected(t *testing.T) {
	httpServer, _ := newFakeTodoServer(t)
	cfg := testTodoConfig(t, httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			t.Fatal("le client llm ne devrait jamais être appelé si la liste todo n'est pas configurée pour la portée")
			return llm.ChatCompletionResponse(nil), nil
		},
	}

	a := newTodoAgent(t, cfg, client, 5)

	conv := model.Conversation{ID: "conv-10", OrgID: "home", Provider: "whatsapp", ChannelID: "bob-priv", Kind: model.ChannelPrivate, Scope: model.ScopePersonal, ScopeID: "bob"}

	_, err := a.Execute(context.Background(), agent.Request{Conversation: conv, Input: "Ma liste ?"})
	if err == nil {
		t.Fatal("une erreur claire était attendue pour une portée sans liste todo configurée")
	}
}
