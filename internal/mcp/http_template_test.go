package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/config"
)

// requestRecorder capture, pour chaque requête HTTP reçue par le serveur
// MCP factice, la clé d'API passée en variable d'URL et l'en-tête
// d'autorisation : de quoi vérifier que les patrons {{nom}} de l'URL et des
// en-têtes ont bien été résolus avec les values du principal.
type requestRecorder struct {
	mu      sync.Mutex
	apiKeys map[string]bool
	auths   map[string]bool
}

func (r *requestRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key := req.URL.Query().Get("api_key"); key != "" {
		r.apiKeys[key] = true
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		r.auths[auth] = true
	}
}

func (r *requestRecorder) sawAPIKey(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.apiKeys[key]
}

func (r *requestRecorder) sawAuth(auth string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.auths[auth]
}

func newRecordingMCPServer(t *testing.T) (*httptest.Server, *requestRecorder) {
	t.Helper()

	recorder := &requestRecorder{apiKeys: map[string]bool{}, auths: map[string]bool{}}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake", Version: "v0.0.1"}, nil)
	goMCP.AddTool(server, &goMCP.Tool{Name: "ping", Description: "pong"},
		func(ctx context.Context, req *goMCP.CallToolRequest, params struct{}) (*goMCP.CallToolResult, any, error) {
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "pong"}}}, nil, nil
		})

	sse := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server { return server }, nil)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		sse.ServeHTTP(w, req)
	}))
	t.Cleanup(httpServer.Close)

	return httpServer, recorder
}

func TestManagerHTTPTemplatedCredentials(t *testing.T) {
	httpServer, recorder := newRecordingMCPServer(t)

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"meteo": {
				Transport: "http",
				URL:       httpServer.URL + "?api_key={{api_key}}",
				Headers:   map[string]string{"Authorization": "Bearer {{token}}"},
			},
		},
		Identities: config.Identities{
			Principals: []config.Principal{
				{
					ID:   "alice",
					Kind: config.PrincipalKindHuman,
					MCP: map[string]config.MCPOverride{
						"meteo": {Values: map[string]string{"api_key": "alice-key", "token": "alice-token"}},
					},
				},
			},
		},
	}

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := m.GetToolsFor(ctx, "whatsapp:private-alice", "alice", "meteo", Limits{})
	if err != nil {
		t.Fatalf("GetToolsFor(alice): %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("aucun outil retourné")
	}

	if !recorder.sawAPIKey("alice-key") {
		t.Error("la clé d'API d'alice n'a pas été reçue en variable d'URL")
	}
	if !recorder.sawAuth("Bearer alice-token") {
		t.Error("le jeton d'alice n'a pas été reçu en en-tête Authorization")
	}
	if recorder.sawAPIKey("{{api_key}}") || recorder.sawAuth("Bearer {{token}}") {
		t.Error("un patron littéral a atteint le serveur")
	}
}

func TestManagerHTTPTemplateWithoutValuesIsUnavailable(t *testing.T) {
	httpServer, _ := newRecordingMCPServer(t)

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"meteo": {
				Transport: "http",
				URL:       httpServer.URL + "?api_key={{api_key}}",
			},
		},
	}

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Sans principal (connexion commune) comme avec un principal sans
	// values : le serveur à patrons est indisponible, jamais appelé avec un
	// patron littéral.
	_, err := m.GetTools(ctx, "session-a", "meteo", Limits{})
	if err == nil || !strings.Contains(err.Error(), "patrons sans valeur") {
		t.Fatalf("erreur = %v, attendu un refus pour patrons sans valeur", err)
	}
}
