package agent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
)

// --- serveur MCP HTTP (SSE) factice simulant un outil de recherche
// Internet, même approche que internal/mcp/manager_test.go (Phase 11).

type webSearchParams struct {
	Query string `json:"query"`
}

// webSearchResultText permet de scripter le texte retourné par l'outil
// web_search, par appel (dans l'ordre) : la première valeur est utilisée
// pour le premier appel, etc. Le dernier élément est réutilisé pour tout
// appel excédentaire.
func newFakeSearchServer(t *testing.T, results []string) (*httptest.Server, *int) {
	t.Helper()

	calls := 0

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-search", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "web_search", Description: "search the web"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args webSearchParams) (*goMCP.CallToolResult, any, error) {
			text := ""
			if len(results) > 0 {
				idx := calls
				if idx >= len(results) {
					idx = len(results) - 1
				}
				text = results[idx]
			}
			calls++

			return &goMCP.CallToolResult{
				Content: []goMCP.Content{&goMCP.TextContent{Text: text}},
			}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, &calls
}

// countingMCPServer sert de spy : tout appel à goMCP.NewSSEHandler
// incrémente hits, ce qui permet de vérifier qu'un serveur n'a jamais été
// sollicité (test "aucun accès à Agenda ou Todo").
func newCountingMCPServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()

	hits := 0

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-other", Version: "v0.0.1"}, nil)
	goMCP.AddTool(server, &goMCP.Tool{Name: "noop", Description: "noop"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args webSearchParams) (*goMCP.CallToolResult, any, error) {
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "noop"}}}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		hits++
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, &hits
}

func testConversation() model.Conversation {
	return model.Conversation{
		ID:       "conv-research-1",
		OrgID:    "home",
		Provider: "whatsapp",
	}
}

func TestMCPToolAgent_SimpleSearch(t *testing.T) {
	httpServer, calls := newFakeSearchServer(t, []string{"La capitale du Portugal est Lisbonne. Source: https://example.test/lisbonne"})

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: httpServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "web_search", `{"query":"capitale du Portugal"}`)), nil
			}
			return scriptedFinalResponse("La capitale du Portugal est Lisbonne."), nil
		},
	}

	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Quelle est la capitale du Portugal ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if *calls != 1 {
		t.Fatalf("web_search aurait dû être appelé une fois, appelé %d fois", *calls)
	}

	if !strings.Contains(result.Reply, "Lisbonne") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

func TestMCPToolAgent_MultipleResultsAllReferencesCollected(t *testing.T) {
	httpServer, _ := newFakeSearchServer(t, []string{
		"Résultats: https://example.test/a et aussi https://example.test/b, voir aussi https://example.test/c.",
	})

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: httpServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "web_search", `{"query":"exemples"}`)), nil
			}
			return scriptedFinalResponse("Voici plusieurs sources pertinentes."), nil
		},
	}

	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Cherche des exemples"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	want := []string{"https://example.test/a", "https://example.test/b", "https://example.test/c"}
	if len(result.References) != len(want) {
		t.Fatalf("References = %v, attendu %v", result.References, want)
	}
	for i, w := range want {
		if result.References[i] != w {
			t.Errorf("References[%d] = %q, attendu %q", i, result.References[i], w)
		}
	}
}

func TestMCPToolAgent_NetworkErrorReturnsClearError(t *testing.T) {
	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: "http://127.0.0.1:1"},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			t.Fatal("le client llm ne devrait jamais être appelé si les outils mcp sont indisponibles")
			return llm.ChatCompletionResponse(nil), nil
		},
	}

	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, 5)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := a.Execute(ctx, agent.Request{Conversation: testConversation(), Input: "Cherche quelque chose"})
	if err == nil {
		t.Fatal("une erreur claire était attendue lorsque le serveur mcp est injoignable")
	}
}

func TestMCPToolAgent_NoRelevantResultEmptyReferences(t *testing.T) {
	httpServer, _ := newFakeSearchServer(t, []string{"Aucun résultat pertinent trouvé."})

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: httpServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "web_search", `{"query":"rien à voir"}`)), nil
			}
			return scriptedFinalResponse("Je n'ai rien trouvé de pertinent."), nil
		},
	}

	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Cherche quelque chose d'improbable"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if len(result.References) != 0 {
		t.Fatalf("References devrait être vide, obtenu %v", result.References)
	}
	if result.Reply == "" {
		t.Fatal("une réponse finale exploitable était attendue")
	}
}

func TestMCPToolAgent_TruncatedResultStillCompletesTurn(t *testing.T) {
	const bigText = "Une très longue réponse contenant une url https://example.test/truncated qui dépasse largement la limite configurée pour ce test."

	httpServer, _ := newFakeSearchServer(t, []string{bigText})

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: httpServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	var sawTruncationMarker bool

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "web_search", `{"query":"long"}`)), nil
			}

			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleTool && strings.Contains(msg.Content(), "tronqué") {
					sawTruncationMarker = true
				}
			}

			return scriptedFinalResponse("Résumé malgré la troncature."), nil
		},
	}

	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{MaxToolResultBytes: 40}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Donne-moi une réponse longue"})
	if err != nil {
		t.Fatalf("Execute ne devrait pas échouer malgré la troncature: %v", err)
	}

	if !sawTruncationMarker {
		t.Error("la troncature aurait dû être signalée au modèle")
	}
	if result.Reply == "" {
		t.Fatal("le tour aurait dû se terminer normalement malgré la troncature")
	}
}

func TestMCPToolAgent_ReferenceKeptFromToolResult(t *testing.T) {
	const preciseURL = "https://example.test/article-precis"

	httpServer, _ := newFakeSearchServer(t, []string{"Voici la source: " + preciseURL})

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: httpServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "web_search", `{"query":"article"}`)), nil
			}
			return scriptedFinalResponse("Voici un article pertinent."), nil
		},
	}

	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Trouve un article"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	found := false
	for _, ref := range result.References {
		if ref == preciseURL {
			found = true
		}
	}
	if !found {
		t.Fatalf("l'url précise %q devrait être conservée dans References, obtenu %v", preciseURL, result.References)
	}
}

func TestMCPToolAgent_NoAccessToOtherSpecialistMCPServers(t *testing.T) {
	searchServer, searchHits := newCountingMCPServer(t)
	calendarServer, calendarHits := newCountingMCPServer(t)
	todoServer, todoHits := newCountingMCPServer(t)

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: searchServer.URL},
			"google-calendar": {Transport: "http", URL: calendarServer.URL},
			"todo":            {Transport: "http", URL: todoServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Rien à faire."), nil
		},
	}

	// Le spécialiste "research" ne déclare que "internet-search" dans
	// MCPServers : voir NewMCPToolAgent, mcpManager.GetTools ne doit jamais
	// être appelé pour "google-calendar" ou "todo" pour cette session.
	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, 5)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Bonjour"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if *searchHits == 0 {
		t.Error("le serveur internet-search aurait dû être sollicité")
	}
	if *calendarHits != 0 {
		t.Errorf("le serveur google-calendar n'aurait jamais dû être sollicité, hits=%d", *calendarHits)
	}
	if *todoHits != 0 {
		t.Errorf("le serveur todo n'aurait jamais dû être sollicité, hits=%d", *todoHits)
	}
}

func TestMCPToolAgent_MaxToolCallsReached(t *testing.T) {
	httpServer, _ := newFakeSearchServer(t, []string{"résultat"})

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"internet-search": {Transport: "http", URL: httpServer.URL},
		},
	}
	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedToolCallResponse(llm.NewToolCall("call-x", "web_search", `{"query":"boucle"}`)), nil
		},
	}

	const maxSequentialToolCalls = 3
	a := agent.NewMCPToolAgent(client, "system", "research", "Test Org", m, []string{"internet-search"}, mcp.Limits{}, maxSequentialToolCalls)

	_, err := a.Execute(context.Background(), agent.Request{Conversation: testConversation(), Input: "Boucle"})
	if !errors.Is(err, agent.ErrMaxToolCallsReached) {
		t.Fatalf("erreur ErrMaxToolCallsReached attendue, obtenu: %v", err)
	}
}
