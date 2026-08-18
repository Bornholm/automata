package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
)

// TestWebSearchContainer interroge le conteneur SearXNG + MCP de
// misc/web-search, quand il tourne. Il est ignoré par défaut : la suite ne
// doit dépendre ni de Docker ni du réseau. Pour le lancer :
//
//	make web-search-up
//	AUTOMATA_WEB_SEARCH_URL=http://127.0.0.1:3000/mcp \
//	  AUTOMATA_WEB_SEARCH_TOKEN=<jeton> go test ./internal/mcp/ -run WebSearchContainer -v
//
// Il vérifie ce qu'aucun test à serveur factice ne peut vérifier : que le
// transport streamable-http parle bien à ce serveur-là, et qu'une recherche
// traverse réellement SearXNG.
func TestWebSearchContainer(t *testing.T) {
	url := os.Getenv("AUTOMATA_WEB_SEARCH_URL")
	if url == "" {
		t.Skip("AUTOMATA_WEB_SEARCH_URL non défini : conteneur misc/web-search absent")
	}

	headers := map[string]string{}
	if token := os.Getenv("AUTOMATA_WEB_SEARCH_TOKEN"); token != "" {
		headers["Authorization"] = "Bearer " + token
	}

	cfg := &config.Config{
		MCPServers: map[string]config.MCPServer{
			"recherche": {Transport: "streamable-http", URL: url, Headers: headers},
		},
	}

	m := NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tools, err := m.GetTools(ctx, "session-test", "recherche", Limits{})
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	t.Logf("outils exposés: %s", strings.Join(names, ", "))

	searchTool := -1
	for i, tool := range tools {
		if tool.Name() == "searxng_web_search" || tool.Name() == "web_search" {
			searchTool = i
			break
		}
	}
	if searchTool < 0 {
		t.Fatalf("aucun outil de recherche parmi %s", strings.Join(names, ", "))
	}
	result, err := tools[searchTool].Execute(ctx, map[string]any{"query": "protocole MCP anthropic"})
	if err != nil {
		t.Fatalf("Execute(%s): %v", tools[searchTool].Name(), err)
	}
	if strings.TrimSpace(result.Text()) == "" {
		t.Fatal("recherche sans résultat : SearXNG répond-il en json ?")
	}
	t.Logf("premiers résultats: %.400s", result.Text())
}
