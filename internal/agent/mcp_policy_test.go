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

// Ce fichier vérifie que le mécanisme des spécialistes MCP est agnostique du
// domaine : aucun service n'est connu du code, tout se déclare en
// configuration. Le domaine employé ici, la gestion de ruches, n'existe nulle
// part dans l'application. Si ces tests passent, brancher n'importe quel
// service se fait sans écrire une ligne de Go.

// Deux structures distinctes : le SDK MCP dérive le schéma de chaque outil de
// la sienne, et rend obligatoire tout champ déclaré. Une structure partagée
// exigerait un poids pour lister des ruches.
type listHivesParams struct {
	ApiaryID string `json:"apiary_id"`
}

type registerHarvestParams struct {
	ApiaryID string `json:"apiary_id"`
	Name     string `json:"name"`
	Weight   string `json:"weight"`
}

// hiveSpy enregistre ce que le serveur reçoit réellement.
type hiveSpy struct {
	mu sync.Mutex

	listCalls     int
	registerCalls int
	lastApiaryID  string
}

func (s *hiveSpy) record(kind, apiaryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if kind == "list" {
		s.listCalls++
	} else {
		s.registerCalls++
	}

	s.lastApiaryID = apiaryID
}

func (s *hiveSpy) snapshot() (list, register int, apiaryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.listCalls, s.registerCalls, s.lastApiaryID
}

// newFakeHiveServer expose deux outils : une lecture et une écriture.
func newFakeHiveServer(t *testing.T) (*httptest.Server, *hiveSpy) {
	t.Helper()

	spy := &hiveSpy{}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-hive", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "list_hives", Description: "liste les ruches d'un rucher"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args listHivesParams) (*goMCP.CallToolResult, any, error) {
			spy.record("list", args.ApiaryID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "3 ruches."}}}, nil, nil
		})

	goMCP.AddTool(server, &goMCP.Tool{Name: "register_harvest", Description: "enregistre une récolte"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args registerHarvestParams) (*goMCP.CallToolResult, any, error) {
			spy.record("register", args.ApiaryID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Récolte enregistrée."}}}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server { return server }, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, spy
}

// testHiveConfig déclare le domaine entièrement en configuration.
func testHiveConfig(serverURL string) *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		MCPServers: map[string]config.MCPServer{
			"hives": {
				Transport:        "http",
				URL:              serverURL,
				Resource:         &config.MCPResource{Key: "apiary", Parameter: "apiary_id"},
				PermissionDomain: "apiary",
				Tools: config.MCPTools{
					ConfirmWrites: true,
					ReadPrefixes:  []string{"list_"},
				},
			},
		},
		Channels: []config.Channel{
			{
				Provider: "whatsapp", ChannelID: "alice-priv",
				Kind: config.ChannelKindPrivate, Scope: config.ScopePersonal, ScopeID: "alice",
				Resources: map[string]string{"apiary": "rucher-des-tilleuls"},
			},
		},
	}
}

func newHiveAgent(t *testing.T, cfg *config.Config, client llm.ChatCompletionClient) agent.Agent {
	t.Helper()

	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	return agent.NewMCPToolAgent(client, "system", "apiculture", "Maison", cfg, m, []string{"hives"}, mcp.Limits{}, 5)
}

func hiveConversation() model.Conversation {
	return model.Conversation{
		ID: "conv-hive", OrgID: "home", Provider: "whatsapp", ChannelID: "alice-priv",
		Kind: model.ChannelPrivate, Scope: model.ScopePersonal, ScopeID: "alice",
	}
}

// TestUnknownDomain_ReadInjectsResource vérifie qu'un domaine inconnu du code
// bénéficie de l'injection de ressource, exactement comme un agenda.
func TestUnknownDomain_ReadInjectsResource(t *testing.T) {
	httpServer, spy := newFakeHiveServer(t)
	cfg := testHiveConfig(httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				// Le modèle tente au passage d'imposer son propre rucher.
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_hives", `{"apiary_id":"rucher-du-voisin"}`)), nil
			}
			return scriptedFinalResponse("Tu as 3 ruches."), nil
		},
	}

	a := newHiveAgent(t, cfg, client)

	if _, err := a.Execute(context.Background(), agent.Request{Conversation: hiveConversation(), Input: "Combien de ruches ?"}); err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	list, register, apiaryID := spy.snapshot()

	if list != 1 {
		t.Fatalf("list_hives appelé %d fois, attendu 1", list)
	}
	if register != 0 {
		t.Fatalf("aucune écriture ne devait avoir lieu, register_harvest appelé %d fois", register)
	}
	if apiaryID == "rucher-du-voisin" {
		t.Fatal("l'identifiant forgé par le modèle n'aurait jamais dû atteindre le serveur")
	}
	if apiaryID != "rucher-des-tilleuls" {
		t.Fatalf("apiary_id reçu = %q, attendu celui de la portée", apiaryID)
	}
}

// TestUnknownDomain_WriteBecomesProposedAction vérifie qu'un outil d'écriture
// d'un domaine inconnu devient une action à confirmer, avec la permission
// dérivée du domaine déclaré.
func TestUnknownDomain_WriteBecomesProposedAction(t *testing.T) {
	httpServer, spy := newFakeHiveServer(t)
	cfg := testHiveConfig(httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "register_harvest", `{"apiary_id":"forge","name":"Ruche 2","weight":"12kg"}`)), nil
			}
			return scriptedFinalResponse("Je te propose d'enregistrer cette récolte."), nil
		},
	}

	a := newHiveAgent(t, cfg, client)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: hiveConversation(), Input: "Note 12 kg sur la ruche 2"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if _, register, _ := spy.snapshot(); register != 0 {
		t.Fatalf("l'écriture ne devait pas être exécutée avant confirmation, appelée %d fois", register)
	}

	if len(result.ProposedActions) != 1 {
		t.Fatalf("actions proposées: got %d, expected 1", len(result.ProposedActions))
	}

	proposed := result.ProposedActions[0]

	if proposed.ToolName != "register_harvest" {
		t.Errorf("tool_name = %q", proposed.ToolName)
	}
	if proposed.MCPServer != "hives" {
		t.Errorf("mcp_server = %q, attendu hives", proposed.MCPServer)
	}
	if proposed.RequiredPermission != "apiary.personal.write" {
		t.Errorf("permission = %q, attendu apiary.personal.write (dérivée du domaine déclaré)", proposed.RequiredPermission)
	}
	if _, present := proposed.Arguments["apiary_id"]; present {
		t.Error("l'identifiant de ressource ne doit jamais être figé dans une action proposée")
	}
	if !strings.Contains(proposed.Summary, "Ruche 2") {
		t.Errorf("résumé = %q, attendu une description lisible", proposed.Summary)
	}
}

// TestUnknownDomain_WithoutConfirmWritesExecutesDirectly vérifie l'autre
// réglage : sans confirm_writes, tous les outils s'exécutent immédiatement,
// ce qui convient à un service en lecture seule comme une recherche web.
func TestUnknownDomain_WithoutConfirmWritesExecutesDirectly(t *testing.T) {
	httpServer, spy := newFakeHiveServer(t)

	cfg := testHiveConfig(httpServer.URL)
	server := cfg.MCPServers["hives"]
	server.Tools = config.MCPTools{}
	server.PermissionDomain = ""
	cfg.MCPServers["hives"] = server

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "register_harvest", `{"name":"Ruche 2","weight":"12kg"}`)), nil
			}
			return scriptedFinalResponse("C'est enregistré."), nil
		},
	}

	a := newHiveAgent(t, cfg, client)

	result, err := a.Execute(context.Background(), agent.Request{Conversation: hiveConversation(), Input: "Note la récolte"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if _, register, apiaryID := spy.snapshot(); register != 1 || apiaryID != "rucher-des-tilleuls" {
		t.Fatalf("l'outil devait s'exécuter directement avec la ressource injectée, appels=%d apiary=%q", register, apiaryID)
	}

	if len(result.ProposedActions) != 0 {
		t.Fatalf("aucune action à confirmer attendue, obtenu %d", len(result.ProposedActions))
	}
}

// TestUnknownDomain_UnconfiguredScopeRejected vérifie qu'une portée sans
// ressource déclarée est refusée avant tout appel au modèle, plutôt qu'au
// moment de confirmer un plan déjà annoncé.
func TestUnknownDomain_UnconfiguredScopeRejected(t *testing.T) {
	httpServer, _ := newFakeHiveServer(t)
	cfg := testHiveConfig(httpServer.URL)

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			t.Fatal("le modèle ne devrait jamais être appelé pour une portée sans ressource")
			return llm.ChatCompletionResponse(nil), nil
		},
	}

	a := newHiveAgent(t, cfg, client)

	conv := hiveConversation()
	conv.ScopeID = "bob" // aucun canal ne déclare de rucher pour bob

	if _, err := a.Execute(context.Background(), agent.Request{Conversation: conv, Input: "Combien de ruches ?"}); err == nil {
		t.Fatal("une erreur claire était attendue pour une portée sans ressource configurée")
	}
}
