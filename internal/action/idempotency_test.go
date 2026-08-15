package action_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// --- serveur MCP HTTP (SSE) factice, dédié à la vérification de
// l'injection best-effort de clé d'idempotence (PLAN.md §18) : deux outils,
// l'un déclarant "idempotency_key" dans son schéma d'entrée, l'autre non.
// Chaque outil consigne les arguments reçus (map[string]any brute, In ==
// map[string]any) pour permettre d'observer précisément ce qui a été
// injecté ou non côté serveur, sans dépendre d'une structure Go typée qui
// filtrerait silencieusement tout champ non déclaré.

func newIdempotencyFakeServer(t *testing.T) (*httptest.Server, *receivedArgs) {
	t.Helper()

	received := &receivedArgs{}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-idempotency", Version: "v0.0.1"}, nil)

	// "with-key" déclare explicitement idempotency_key dans son schéma
	// d'entrée : withIdempotencyKey doit y injecter l'ID de l'action.
	goMCP.AddTool(server, &goMCP.Tool{
		Name:        "with-key",
		Description: "outil déclarant idempotency_key",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"text":            {Type: "string"},
				"idempotency_key": {Type: "string"},
			},
		},
	}, func(ctx context.Context, req *goMCP.CallToolRequest, args map[string]any) (*goMCP.CallToolResult, any, error) {
		received.set(args)
		return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "ok"}}}, nil, nil
	})

	// "without-key" ne déclare aucune des propriétés conventionnelles :
	// aucune injection ne doit avoir lieu.
	goMCP.AddTool(server, &goMCP.Tool{
		Name:        "without-key",
		Description: "outil ne déclarant aucune clé d'idempotence",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"text": {Type: "string"},
			},
		},
	}, func(ctx context.Context, req *goMCP.CallToolRequest, args map[string]any) (*goMCP.CallToolResult, any, error) {
		received.set(args)
		return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "ok"}}}, nil, nil
	})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, received
}

type receivedArgs struct {
	args map[string]any
}

func (r *receivedArgs) set(args map[string]any) { r.args = args }

func idempotencyMCPProposal(toolName string) delegation.ProposedAction {
	return delegation.ProposedAction{
		Summary:            "Appel de l'outil MCP " + toolName,
		MCPServer:          "fake-idempotency",
		ToolName:           toolName,
		Arguments:          map[string]any{"text": "hello"},
		RequiredPermission: "memory.personal.write",
		Scope:              model.ScopePersonal,
		ScopeID:            "alice",
	}
}

func TestEngine_Idempotency_KeyInjectedWhenSchemaDeclaresIt(t *testing.T) {
	httpServer, received := newIdempotencyFakeServer(t)

	cfg := appConfig(true)
	cfg.MCPServers = map[string]config.MCPServer{
		"fake-idempotency": {Transport: "http", URL: httpServer.URL},
	}

	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	mcpManager := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = mcpManager.Close() })

	authorizer := authorization.NewAuthorizer(cfg)
	engine := action.NewEngine(db, authorizer, mcpManager, cfg)

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	plan, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		idempotencyMCPProposal("with-key"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !containsSuccess(report) {
		t.Fatalf("succès attendu, obtenu: %q", report)
	}

	actions := listPlanActions(t, db, plan.ID)
	if len(actions) != 1 {
		t.Fatalf("une seule action attendue, obtenu %d", len(actions))
	}
	wantID := string(actions[0].ID)

	if received.args == nil {
		t.Fatal("le serveur fake n'a reçu aucun argument")
	}
	gotKey, _ := received.args["idempotency_key"].(string)
	if gotKey != wantID {
		t.Fatalf("idempotency_key reçu: got %q, expected l'ID de l'action %q", gotKey, wantID)
	}
}

func TestEngine_Idempotency_NoInjectionWhenSchemaOmitsIt(t *testing.T) {
	httpServer, received := newIdempotencyFakeServer(t)

	cfg := appConfig(true)
	cfg.MCPServers = map[string]config.MCPServer{
		"fake-idempotency": {Transport: "http", URL: httpServer.URL},
	}

	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	mcpManager := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = mcpManager.Close() })

	authorizer := authorization.NewAuthorizer(cfg)
	engine := action.NewEngine(db, authorizer, mcpManager, cfg)

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		idempotencyMCPProposal("without-key"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !containsSuccess(report) {
		t.Fatalf("succès attendu, obtenu: %q", report)
	}

	if received.args == nil {
		t.Fatal("le serveur fake n'a reçu aucun argument")
	}
	if _, injected := received.args["idempotency_key"]; injected {
		t.Fatalf("aucune injection attendue (schéma sans idempotency_key), reçu: %v", received.args)
	}
	if len(received.args) != 1 {
		t.Fatalf("seul l'argument 'text' d'origine était attendu, reçu: %v", received.args)
	}
}

func containsSuccess(report string) bool {
	return strings.Contains(report, "succès")
}

// listPlanActions recharge les actions persistées du plan planID.
func listPlanActions(t *testing.T, db *persistence.DB, planID persistence.ActionPlanID) []persistence.Action {
	t.Helper()

	var actions []persistence.Action
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		actions, err = persistence.NewActionRepository().ListByPlanID(context.Background(), tx, planID)
		return err
	})
	if err != nil {
		t.Fatalf("listPlanActions: %v", err)
	}
	return actions
}
