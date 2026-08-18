package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

// TestMain intercepte la ré-exécution du binaire de test en serveur MCP
// stdio factice (voir fakeStdioServerConfig) : quand la variable
// d'environnement est présente, le processus N'EST PAS une session de test
// mais le serveur enfant lancé par le manager — il sert le protocole MCP
// sur stdin/stdout puis se termine. C'est le seul moyen de tester le
// transport stdio de bout en bout sans embarquer un exécutable externe.
func TestMain(m *testing.M) {
	if os.Getenv("AUTOMATA_TEST_FAKE_STDIO_MCP") == "1" {
		runFakeStdioServer()
		return
	}

	os.Exit(m.Run())
}

// runFakeStdioServer sert un serveur MCP minimal sur stdio, avec un outil
// "whoami" qui répète la variable d'environnement FAKE_USER et l'argument
// de commande reçu après "--marker" : de quoi vérifier, côté client, que
// les patrons {{...}} de command et env ont bien été résolus avec les
// values du principal.
func runFakeStdioServer() {
	marker := ""
	for i, arg := range os.Args {
		if arg == "--marker" && i+1 < len(os.Args) {
			marker = os.Args[i+1]
		}
	}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-stdio", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "whoami", Description: "identité du serveur"}, func(ctx context.Context, req *goMCP.CallToolRequest, params struct{}) (*goMCP.CallToolResult, any, error) {
		text := fmt.Sprintf("user=%s marker=%s", os.Getenv("FAKE_USER"), marker)
		return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: text}}}, nil, nil
	})

	if err := server.Run(context.Background(), &goMCP.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fake stdio mcp:", err)
		os.Exit(1)
	}
}

// fakeStdioServerConfig déclare un serveur stdio qui relance ce binaire de
// test en mode serveur factice, avec des patrons {{user}} et {{marker}}.
func fakeStdioServerConfig() config.MCPServer {
	return config.MCPServer{
		Transport: "stdio",
		Command:   []string{os.Args[0], "--marker", "{{marker}}"},
		Env: map[string]string{
			"AUTOMATA_TEST_FAKE_STDIO_MCP": "1",
			"FAKE_USER":                    "{{user}}",
		},
	}
}

func newStdioTestConfig(serverName string, principals ...config.Principal) *config.Config {
	return &config.Config{
		MCPServers: map[string]config.MCPServer{
			serverName: fakeStdioServerConfig(),
		},
		Identities: config.Identities{Principals: principals},
	}
}

func callWhoami(t *testing.T, m *Manager, ctx context.Context, session SessionKey, principal, serverName string) string {
	t.Helper()

	tools, err := m.GetToolsFor(ctx, session, model.PrincipalID(principal), serverName, Limits{})
	if err != nil {
		t.Fatalf("GetToolsFor(%s): %v", principal, err)
	}

	for _, tool := range tools {
		if tool.Name() != "whoami" {
			continue
		}
		result, err := tool.Execute(ctx, map[string]any{})
		if err != nil {
			t.Fatalf("whoami(%s): %v", principal, err)
		}
		return result.Text()
	}

	t.Fatalf("outil whoami introuvable pour %s", principal)
	return ""
}

func TestManagerStdioPerPrincipal(t *testing.T) {
	cfg := newStdioTestConfig("imap",
		config.Principal{
			ID:   "alice",
			Kind: config.PrincipalKindHuman,
			MCP: map[string]config.MCPOverride{
				"imap": {Values: map[string]string{"user": "alice@example.com", "marker": "alice-marker"}},
			},
		},
		config.Principal{
			ID:   "bob",
			Kind: config.PrincipalKindHuman,
			MCP: map[string]config.MCPOverride{
				"imap": {Values: map[string]string{"user": "bob@example.com", "marker": "bob-marker"}},
			},
		},
	)

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Chaque principal obtient SON processus, avec SES valeurs substituées
	// dans la commande et l'environnement.
	got := callWhoami(t, m, ctx, "whatsapp:group", "alice", "imap")
	if !strings.Contains(got, "user=alice@example.com") || !strings.Contains(got, "marker=alice-marker") {
		t.Errorf("whoami(alice) = %q, valeurs d'alice attendues", got)
	}

	got = callWhoami(t, m, ctx, "whatsapp:group", "bob", "imap")
	if !strings.Contains(got, "user=bob@example.com") || !strings.Contains(got, "marker=bob-marker") {
		t.Errorf("whoami(bob) = %q, valeurs de bob attendues", got)
	}

	// Le processus d'un principal est partagé entre ses sessions : la clé
	// de cache est (principal, serveur), pas (session, principal, serveur).
	_ = callWhoami(t, m, ctx, "whatsapp:private-alice", "alice", "imap")

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sessions) != 2 {
		keys := make([]string, 0, len(m.sessions))
		for k := range m.sessions {
			keys = append(keys, string(k))
		}
		t.Errorf("clés de cache = %v, attendu une par principal", keys)
	}
	if _, ok := m.sessions[SessionKey("principal|alice")]; !ok {
		t.Error("clé de cache principal|alice absente")
	}
}

func TestManagerStdioWithoutValuesIsUnavailable(t *testing.T) {
	// leo n'a aucune surcharge : le serveur à patrons doit lui être refusé
	// avec une erreur claire, jamais lancé avec des patrons littéraux ni
	// avec les valeurs d'un autre principal.
	cfg := newStdioTestConfig("imap", config.Principal{ID: "leo", Kind: config.PrincipalKindHuman})

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := m.GetToolsFor(ctx, "whatsapp:private-leo", "leo", "imap", Limits{})
	if err == nil || !strings.Contains(err.Error(), "patrons sans valeur") {
		t.Fatalf("erreur = %v, attendu un refus pour patrons sans valeur", err)
	}
}

func TestRenderStdioCommand(t *testing.T) {
	serverCfg := config.MCPServer{
		Transport: "stdio",
		Command:   []string{"imap-mcp", "--host", "{{host}}"},
		Env:       map[string]string{"IMAP_PASSWORD": "{{password}}"},
	}

	command, env, err := renderStdioCommand(serverCfg, config.MCPOverride{Values: map[string]string{
		"host":     "imap.example.com",
		"password": "s3cret",
	}})
	if err != nil {
		t.Fatalf("renderStdioCommand: %v", err)
	}

	if command[2] != "imap.example.com" {
		t.Errorf("command = %v, hôte substitué attendu", command)
	}

	found := false
	for _, entry := range env {
		if entry == "IMAP_PASSWORD=s3cret" {
			found = true
		}
	}
	if !found {
		t.Errorf("IMAP_PASSWORD substitué absent de l'environnement")
	}

	// L'environnement du worker est conservé (PATH, etc.).
	if len(env) <= len(serverCfg.Env) {
		t.Errorf("l'environnement du worker doit être hérité (%d entrées)", len(env))
	}

	// Patrons manquants : erreur citant les noms, jamais les valeurs.
	_, _, err = renderStdioCommand(serverCfg, config.MCPOverride{})
	if err == nil || !strings.Contains(err.Error(), "{{host}}") || !strings.Contains(err.Error(), "{{password}}") {
		t.Fatalf("erreur = %v, attendu la liste des patrons manquants", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatal("une valeur ne doit jamais apparaître dans une erreur")
	}
}
