package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/config"
)

// --- serveur MCP HTTP (SSE) factice pour les tests ---
//
// genai/mcp/http.NewClient (mcp/http/client.go) se connecte via
// goMCP.SSEClientTransport (protocole SSE de la spec MCP 2024-11-05, voir
// modelcontextprotocol/go-sdk/mcp/sse.go). Le SDK go-sdk fournit
// goMCP.NewSSEHandler pour servir ce protocole côté serveur (voir son
// exemple mcp/sse_example_test.go) : on l'utilise directement plutôt que de
// réimplémenter le protocole SSE/MCP à la main.

type echoParams struct {
	Text string `json:"text"`
}

// newFakeMCPServer démarre un httptest.Server exposant un serveur MCP réel
// (via goMCP) avec les outils suivants :
//   - "echo" : renvoie params.Text.
//   - "slow" : attend delay (ou l'annulation de ctx) avant de répondre.
//   - "big"  : renvoie une chaîne de bigSize octets.
//
// connections compte le nombre de sessions SSE établies (une par
// client.Start réussi), ce qui permet de vérifier l'isolation entre
// sessions applicatives.
func newFakeMCPServer(t *testing.T, delay time.Duration, bigSize int) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var connections atomic.Int64

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "echo", Description: "echo text"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args echoParams) (*goMCP.CallToolResult, any, error) {
			return &goMCP.CallToolResult{
				Content: []goMCP.Content{&goMCP.TextContent{Text: args.Text}},
			}, nil, nil
		})

	goMCP.AddTool(server, &goMCP.Tool{Name: "slow", Description: "sleeps before answering"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args echoParams) (*goMCP.CallToolResult, any, error) {
			select {
			case <-time.After(delay):
				return &goMCP.CallToolResult{
					Content: []goMCP.Content{&goMCP.TextContent{Text: "done"}},
				}, nil, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		})

	goMCP.AddTool(server, &goMCP.Tool{Name: "big", Description: "returns a large payload"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args echoParams) (*goMCP.CallToolResult, any, error) {
			return &goMCP.CallToolResult{
				Content: []goMCP.Content{&goMCP.TextContent{Text: strings.Repeat("a", bigSize)}},
			}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		connections.Add(1)
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, &connections
}

func newTestConfig(serverName, url string) *config.Config {
	return &config.Config{
		MCPServers: map[string]config.MCPServer{
			serverName: {
				Transport: "http",
				URL:       url,
			},
		},
	}
}

func TestManagerGetToolsConnects(t *testing.T) {
	httpServer, _ := newFakeMCPServer(t, 0, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tools, err := m.GetTools(ctx, "session-a", "fake", Limits{})
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatalf("expected at least one tool, got 0")
	}

	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	if !contains(names, "echo") {
		t.Errorf("expected tool %q among %v", "echo", names)
	}
}

func TestManagerServerUnavailable(t *testing.T) {
	cfg := newTestConfig("fake", "http://127.0.0.1:1")
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := m.GetTools(ctx, "session-a", "fake", Limits{})
	if err == nil {
		t.Fatalf("expected an error for an unreachable server")
	}
}

func TestManagerUnknownServerName(t *testing.T) {
	cfg := newTestConfig("fake", "http://example.invalid")
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.GetTools(context.Background(), "session-a", "does-not-exist", Limits{})
	if err == nil {
		t.Fatalf("expected an error for an undeclared server name")
	}
}

func TestManagerUnsupportedTransport(t *testing.T) {
	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"fake": {Transport: "stdio", URL: "irrelevant"},
		},
	}
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.GetTools(context.Background(), "session-a", "fake", Limits{})
	if err == nil {
		t.Fatalf("expected an error for an unsupported transport")
	}
}

func TestManagerToolTimeout(t *testing.T) {
	httpServer, _ := newFakeMCPServer(t, 2*time.Second, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tools, err := m.GetTools(ctx, "session-a", "fake", Limits{ToolTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	slow := findTool(t, tools, "slow")

	start := time.Now()
	_, err = slow.Execute(ctx, map[string]any{"text": "x"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected the call to fail quickly (timeout), took %s", elapsed)
	}
}

func TestManagerContextCancellation(t *testing.T) {
	httpServer, _ := newFakeMCPServer(t, 2*time.Second, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()

	tools, err := m.GetTools(setupCtx, "session-a", "fake", Limits{})
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	slow := findTool(t, tools, "slow")

	callCtx, callCancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		callCancel()
	}()

	start := time.Now()
	_, err = slow.Execute(callCtx, map[string]any{"text": "x"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error after context cancellation")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected the call to fail quickly (cancellation), took %s", elapsed)
	}
}

// TestManagerUnknownToolName vérifie que l'exécution d'un nom d'outil qui
// n'apparaît pas parmi les outils annoncés par le serveur (le cas
// "hallucination" côté modèle) produit une réponse claire, sans erreur Go
// ni panique — comportement délégué à llm.ExecuteToolCall (genai), exercé
// ici avec l'ensemble d'outils réellement retourné par Manager.
func TestManagerUnknownToolName(t *testing.T) {
	httpServer, _ := newFakeMCPServer(t, 0, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tools, err := m.GetTools(ctx, "session-a", "fake", Limits{})
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	tc := llm.NewToolCall("call-1", "does_not_exist", "{}")

	msg, err := llm.ExecuteToolCall(ctx, tc, tools...)
	if err != nil {
		t.Fatalf("ExecuteToolCall: unexpected Go error (should surface as a tool message, not panic/error): %v", err)
	}
	if msg == nil {
		t.Fatalf("expected a tool message")
	}
}

func TestManagerLargeResultTruncated(t *testing.T) {
	const bigSize = 10_000
	const maxBytes = 100

	httpServer, _ := newFakeMCPServer(t, 0, bigSize)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tools, err := m.GetTools(ctx, "session-a", "fake", Limits{MaxToolResultBytes: maxBytes})
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	big := findTool(t, tools, "big")

	result, err := big.Execute(ctx, map[string]any{"text": "x"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	text := result.Text()
	if len(text) > maxBytes+64 {
		t.Errorf("expected a truncated result, got %d bytes", len(text))
	}
	if !strings.Contains(text, "tronqué") {
		t.Errorf("expected an explicit truncation marker, got %q", text)
	}
}

func TestManagerSessionIsolation(t *testing.T) {
	httpServer, connections := newFakeMCPServer(t, 0, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.GetTools(ctx, "session-a", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools session-a: %v", err)
	}
	if _, err := m.GetTools(ctx, "session-a", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools session-a (reuse): %v", err)
	}
	if _, err := m.GetTools(ctx, "session-b", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools session-b: %v", err)
	}

	if got := connections.Load(); got != 2 {
		t.Errorf("expected 2 distinct connections (one per session), got %d", got)
	}

	m.mu.Lock()
	clientA := m.sessions["session-a"]["fake"]
	clientB := m.sessions["session-b"]["fake"]
	m.mu.Unlock()

	if clientA == clientB {
		t.Errorf("expected distinct clients for distinct sessions")
	}
}

// toolNamed retrouve un outil par son nom parmi ceux retournés par GetTools.
func toolNamed(t *testing.T, tools []llm.Tool, name string) llm.Tool {
	t.Helper()

	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}

	t.Fatalf("outil %q introuvable parmi les outils retournés", name)

	return nil
}

// TestManagerSessionSurvivesRequestContextCancellation vérifie qu'une
// connexion mise en cache reste utilisable après l'annulation du contexte de
// la requête qui l'a créée.
//
// Régression : le client était démarré avec le contexte de cette requête.
// Comme l'ingress borne le traitement d'un message par un timeout, la
// connexion mourait dès la fin du message tout en restant référencée, et le
// message suivant réutilisait un client fermé ("client is closing"). Le cas
// se produit dès qu'une session sert à plus d'un message — une action agenda
// ou todo proposée dans un message puis exécutée après confirmation dans un
// autre.
func TestManagerSessionSurvivesRequestContextCancellation(t *testing.T) {
	httpServer, connections := newFakeMCPServer(t, 0, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	// Premier message : établit la connexion, puis son contexte est annulé.
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := m.GetTools(firstCtx, "session-a", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools (premier message): %v", err)
	}
	cancelFirst()

	// Message suivant, même session : la connexion en cache doit encore
	// servir, et aucun outil ne doit échouer.
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSecond()

	tools, err := m.GetTools(secondCtx, "session-a", "fake", Limits{})
	if err != nil {
		t.Fatalf("GetTools après annulation du contexte du premier message: %v", err)
	}

	echo := toolNamed(t, tools, "echo")
	if _, err := echo.Execute(secondCtx, map[string]any{"text": "ping"}); err != nil {
		t.Fatalf("exécution d'un outil sur une session réutilisée: %v", err)
	}

	if got := connections.Load(); got != 1 {
		t.Errorf("connexions établies = %d, attendu 1 (la session doit être réutilisée, pas rouverte)", got)
	}
}

func TestManagerCloseSessionRecreatesConnection(t *testing.T) {
	httpServer, connections := newFakeMCPServer(t, 0, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.GetTools(ctx, "session-a", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if err := m.CloseSession("session-a"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// Idempotent.
	if err := m.CloseSession("session-a"); err != nil {
		t.Fatalf("CloseSession (second call): %v", err)
	}
	// No-op on an unknown session.
	if err := m.CloseSession("session-never-opened"); err != nil {
		t.Fatalf("CloseSession (unknown session): %v", err)
	}

	if _, err := m.GetTools(ctx, "session-a", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools after CloseSession: %v", err)
	}

	if got := connections.Load(); got != 2 {
		t.Errorf("expected a fresh connection after CloseSession, got %d total connections", got)
	}
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	httpServer, _ := newFakeMCPServer(t, 0, 0)
	cfg := newTestConfig("fake", httpServer.URL)
	m := NewManager(cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.GetTools(ctx, "session-a", "fake", Limits{}); err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close (second call): %v", err)
	}
}

// --- helpers ---

func contains(items []string, target string) bool {
	return slices.Contains(items, target)
}

func findTool(t *testing.T, tools []llm.Tool, name string) llm.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found among returned tools", name)
	return nil
}
