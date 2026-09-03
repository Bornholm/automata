package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain intercepte la ré-exécution du binaire de test en serveur MCP
// stdio : la variable présente, le processus n'est pas une session de test
// mais le serveur enfant lancé par le pool. C'est le seul moyen
// d'éprouver le transport stdio de bout en bout sans embarquer un
// exécutable extérieur — même dispositif que les tests du gestionnaire MCP
// de l'hôte.
func TestMain(m *testing.M) {
	if os.Getenv("SUBAGENTS_TEST_FAKE_MCP") == "1" {
		runFakeServer()
		return
	}

	os.Exit(m.Run())
}

// runFakeServer sert un MCP minimal avec un outil « whoami » qui répète le
// jeton reçu par l'environnement : c'est ce qui prouve, côté client, que
// les identifiants du bon membre ont été injectés.
func runFakeServer() {
	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{
		Name:        "whoami",
		Description: "Repeats the token it was started with.",
		Annotations: &goMCP.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *goMCP.CallToolRequest, params struct{}) (*goMCP.CallToolResult, any, error) {
		text := "token=" + os.Getenv("FAKE_TOKEN")
		return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: text}}}, nil, nil
	})

	// Un second outil sans annotation : l'hôte doit le traiter comme une
	// écriture, faute de quoi il s'exécuterait sans confirmation.
	goMCP.AddTool(server, &goMCP.Tool{
		Name:        "mutate",
		Description: "Changes something, without saying so.",
	}, func(ctx context.Context, req *goMCP.CallToolRequest, params struct{}) (*goMCP.CallToolResult, any, error) {
		return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "done"}}}, nil, nil
	})

	if err := server.Run(context.Background(), &goMCP.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fake mcp:", err)
		os.Exit(1)
	}
}

// fakeAgent déclare une entrée dont le serveur stdio relance ce binaire de
// test, avec un identifiant à injecter.
func fakeAgent() catalogAgent {
	return catalogAgent{
		Name:         "probe",
		Description:  "probes things",
		SystemPrompt: "You are a prober.",
		Credentials:  []credentialField{{Key: "token", Required: true}},
		Servers: []serverSpec{{
			Name:      "fake",
			Transport: transportStdio,
			Command:   []string{os.Args[0]},
			Env: map[string]string{
				"SUBAGENTS_TEST_FAKE_MCP": "1",
				"FAKE_TOKEN":              "{{token}}",
			},
		}},
	}
}

// Le pool se connecte, découvre les outils, et l'annotation du serveur
// décide de ce qui s'exécute pendant le tour.
func TestPool_DiscoversToolsAndHonorsAnnotations(t *testing.T) {
	p := newPool()
	t.Cleanup(p.close)

	agent := fakeAgent()
	conn := connection{
		agent: agent, server: agent.Servers[0],
		orgID: "atelier", memberID: "cam",
		values: map[string]string{"token": "jeton-de-cam"},
	}

	tools, err := p.tools(context.Background(), conn)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("%d outil(s), attendu 2: %+v", len(tools), tools)
	}

	byName := map[string]toolDescriptor{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	if !byName["whoami"].ReadOnly {
		t.Error("un outil annoté readOnlyHint devrait s'exécuter pendant le tour")
	}
	// Sans annotation ni épinglage au catalogue : écriture, donc
	// confirmation. Un outil muet n'est pas un outil inoffensif.
	if byName["mutate"].ReadOnly {
		t.Error("un outil non annoté a été pris pour une lecture")
	}
	if !strings.Contains(byName["whoami"].SchemaJSON, "object") {
		t.Errorf("schéma inattendu: %s", byName["whoami"].SchemaJSON)
	}

	result, err := p.call(context.Background(), conn, "whoami", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(result, "token=jeton-de-cam") {
		t.Errorf("identifiant du membre non injecté: %s", result)
	}
}

// Deux membres ne partagent JAMAIS une connexion dès qu'un identifiant
// entre en jeu : sans cela, l'un parlerait au serveur avec le jeton de
// l'autre.
func TestPool_NeverSharesAnAuthenticatedConnection(t *testing.T) {
	p := newPool()
	t.Cleanup(p.close)

	agent := fakeAgent()
	conn := func(member, token string) connection {
		return connection{
			agent: agent, server: agent.Servers[0],
			orgID: "atelier", memberID: member,
			values: map[string]string{"token": token},
		}
	}

	cam, yann := conn("cam", "jeton-de-cam"), conn("yann", "jeton-de-yann")
	if cam.key() == yann.key() {
		t.Fatalf("deux membres partagent la même clé de connexion: %s", cam.key())
	}

	for _, tc := range []struct {
		conn connection
		want string
	}{
		{cam, "token=jeton-de-cam"},
		{yann, "token=jeton-de-yann"},
	} {
		result, err := p.call(context.Background(), tc.conn, "whoami", nil)
		if err != nil {
			t.Fatalf("call(%s): %v", tc.conn.memberID, err)
		}
		if !strings.Contains(result, tc.want) {
			t.Errorf("%s a obtenu %q, attendu %q", tc.conn.memberID, result, tc.want)
		}
	}
}

// Un serveur sans identifiant n'a rien de personnel à isoler : la
// connexion est partagée par l'organisation, un processus au lieu d'un par
// membre.
func TestPool_SharesAnAnonymousConnectionWithinTheOrg(t *testing.T) {
	agent := fakeAgent()
	agent.Credentials = nil
	agent.Servers[0].Env = map[string]string{"SUBAGENTS_TEST_FAKE_MCP": "1"}

	cam := connection{agent: agent, server: agent.Servers[0], orgID: "atelier", memberID: "cam"}
	yann := connection{agent: agent, server: agent.Servers[0], orgID: "atelier", memberID: "yann"}

	if cam.key() != yann.key() {
		t.Errorf("connexion anonyme dupliquée par membre: %s vs %s", cam.key(), yann.key())
	}

	autre := connection{agent: agent, server: agent.Servers[0], orgID: "autre", memberID: "cam"}
	if cam.key() == autre.key() {
		t.Error("deux organisations partagent la même connexion")
	}
}

// Un patron sans valeur est une erreur, et cette erreur ne cite que le
// NOM : les valeurs sont les identifiants de la personne.
func TestRenderStdio_MissingCredentialNamesOnlyTheKey(t *testing.T) {
	server := serverSpec{
		Transport: transportStdio,
		Command:   []string{"serveur", "--token", "{{token}}"},
		Env:       map[string]string{"OTHER": "{{other}}"},
	}

	_, _, err := renderStdio(server, map[string]string{})
	if err == nil {
		t.Fatal("une commande à patron non résolu a été acceptée")
	}
	for _, name := range []string{"token", "other"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("message sans le nom %q: %v", name, err)
		}
	}
}

func TestRenderHTTP_ResolvesURLAndHeaders(t *testing.T) {
	server := serverSpec{
		Transport: transportStreamable,
		URL:       "https://tracker.test/{{tenant}}/mcp",
		Headers:   map[string]string{"Authorization": "Bearer {{token}}"},
	}

	url, headers, err := renderHTTP(server, map[string]string{"tenant": "atelier", "token": "s3cr3t"})
	if err != nil {
		t.Fatalf("renderHTTP: %v", err)
	}
	if url != "https://tracker.test/atelier/mcp" {
		t.Errorf("URL rendue: %s", url)
	}
	if headers["Authorization"] != "Bearer s3cr3t" {
		t.Errorf("en-tête rendu: %s", headers["Authorization"])
	}

	if _, _, err := renderHTTP(server, map[string]string{"tenant": "atelier"}); err == nil {
		t.Error("un en-tête à patron non résolu a été accepté")
	} else if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("une valeur d'identifiant a fuité dans l'erreur: %v", err)
	}
}
