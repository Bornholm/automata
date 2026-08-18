package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/config"
)

// newStreamableMCPServer monte un serveur MCP factice parlant la révision
// 2025-03-26 du protocole (« Streamable HTTP »), celle des serveurs récents
// comme mcp-searxng (voir misc/web-search). Le transport "http" d'automata,
// lui, parle l'ancien HTTP+SSE : ce test vérifie que les deux cohabitent et
// que le nôtre parle bien au bon.
func newStreamableMCPServer(t *testing.T, recorder *requestRecorder) *httptest.Server {
	t.Helper()

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake", Version: "v0.0.1"}, nil)
	goMCP.AddTool(server, &goMCP.Tool{Name: "web_search", Description: "recherche"},
		func(ctx context.Context, req *goMCP.CallToolRequest, params struct{}) (*goMCP.CallToolResult, any, error) {
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "resultat"}}}, nil, nil
		})

	handler := goMCP.NewStreamableHTTPHandler(func(*http.Request) *goMCP.Server { return server }, nil)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if recorder != nil {
			recorder.record(req)
		}
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(httpServer.Close)

	return httpServer
}

func TestManagerStreamableHTTP(t *testing.T) {
	recorder := &requestRecorder{apiKeys: map[string]bool{}, auths: map[string]bool{}}
	httpServer := newStreamableMCPServer(t, recorder)

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"recherche": {
				Transport: "streamable-http",
				URL:       httpServer.URL + "/mcp",
				Headers:   map[string]string{"Authorization": "Bearer {{token}}"},
			},
		},
		Identities: config.Identities{
			Principals: []config.Principal{
				{
					ID:   "alice",
					Kind: config.PrincipalKindHuman,
					MCP: map[string]config.MCPOverride{
						"recherche": {Values: map[string]string{"token": "alice-token"}},
					},
				},
			},
		},
	}

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := m.GetToolsFor(ctx, "whatsapp:private-alice", "alice", "recherche", Limits{})
	if err != nil {
		t.Fatalf("GetToolsFor(alice): %v", err)
	}
	if len(tools) != 1 || tools[0].Name() != "web_search" {
		t.Fatalf("outils = %v, attendu [web_search]", tools)
	}

	// Le templating par principal vaut pour les deux transports HTTP.
	if !recorder.sawAuth("Bearer alice-token") {
		t.Error("le jeton d'alice n'a pas été reçu en en-tête Authorization")
	}
}

// Un serveur Streamable HTTP rejette le GET permanent qu'ouvre le client
// HTTP+SSE : brancher le mauvais transport échoue, il faut donc que le nom
// choisi en configuration soit le bon.
func TestManagerHTTPTransportRejectedByStreamableServer(t *testing.T) {
	httpServer := newStreamableMCPServer(t, nil)

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"recherche": {Transport: "http", URL: httpServer.URL + "/mcp"},
		},
	}

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := m.GetTools(ctx, "session-a", "recherche", Limits{}); err == nil {
		t.Fatal("le transport http a réussi à se connecter à un serveur streamable, attendu un échec")
	}
}

func TestBuildClientUnknownTransportListsStreamable(t *testing.T) {
	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"x": {Transport: "websocket", URL: "http://example.com"},
		},
	}

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.buildClient("x", config.MCPOverride{})
	if err == nil || !strings.Contains(err.Error(), "streamable-http") {
		t.Fatalf("erreur = %v, attendu la liste des transports supportés", err)
	}
}
