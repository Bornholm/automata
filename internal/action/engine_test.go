package action_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// --- fixtures ---------------------------------------------------------

func testStorageConfig(t *testing.T) config.StorageApplication {
	t.Helper()
	return config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}
}

func openTestDB(t *testing.T, storageCfg config.StorageApplication) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(context.Background(), storageCfg)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// appConfig construit une configuration minimale suffisante pour
// authorization.NewAuthorizer. withDelete contrôle si le rôle "adult"
// accorde memory.personal.delete (utilisé par le test "permissions
// retirées" pour simuler un retrait de permission entre la proposition et
// la confirmation).
func appConfig(withDelete bool) *config.Config {
	perms := []string{"memory.personal.read", "memory.personal.write"}
	if withDelete {
		perms = append(perms, "memory.personal.delete")
	}

	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: perms},
			},
			Principals: []config.Principal{
				{ID: "alice", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
				{ID: "eve", Kind: config.PrincipalKindHuman, DisplayName: "Eve", Roles: []string{"adult"}},
			},
		},
	}
}

func privateIdentity(principalID model.PrincipalID, convID model.ConversationID) model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    principalID,
		OrgID:          "home",
		ConversationID: convID,
		Provider:       "test",
		ChannelID:      string(convID),
		ChannelKind:    model.ChannelPrivate,
		Scope:          model.ScopePersonal,
		ScopeID:        model.ScopeID(principalID),
	}
}

func testConversation(id model.ConversationID) model.Conversation {
	return model.Conversation{
		ID:        id,
		OrgID:     "home",
		Provider:  "test",
		ChannelID: string(id),
		Kind:      model.ChannelPrivate,
		Scope:     model.ScopePersonal,
		ScopeID:   "alice",
	}
}

// ensureConversation insère conv en base : action_plans.conversation_id
// référence conversations(id) par contrainte de clé étrangère. En
// production, c'est internal/conversation.Handler qui garantit cette
// insertion avant tout appel à Engine (voir ensureConversation dans
// handler.go) ; les tests doivent la reproduire explicitement.
func ensureConversation(t *testing.T, db *persistence.DB, conv model.Conversation) {
	t.Helper()

	convRepo := persistence.NewConversationRepository()
	now := time.Now().UTC().Format(time.RFC3339)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return convRepo.Insert(context.Background(), tx, persistence.Conversation{
			ID:                conv.ID,
			OrgID:             conv.OrgID,
			Provider:          conv.Provider,
			ExternalChannelID: conv.ChannelID,
			Kind:              conv.Kind,
			Scope:             conv.Scope,
			ScopeID:           conv.ScopeID,
			CreatedAt:         now,
			UpdatedAt:         now,
		})
	})
	if err != nil {
		t.Fatalf("ensureConversation: %v", err)
	}
}

// fakeMemoryStore est une implémentation en mémoire minimale de
// memory.Store, suffisante pour tester l'exécuteur mémoire interne
// (memory.forget) sans dépendre d'Amoxtli.
type fakeMemoryStore struct {
	mu        sync.Mutex
	memories  map[string]memory.Memory
	scopes    map[string]model.ScopeID
	forgotten []string
}

func newFakeMemoryStore() *fakeMemoryStore {
	return &fakeMemoryStore{memories: map[string]memory.Memory{}, scopes: map[string]model.ScopeID{}}
}

func (f *fakeMemoryStore) seed(id, content string, scopeID model.ScopeID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memories[id] = memory.Memory{ID: id, Content: content}
	f.scopes[id] = scopeID
}

func (f *fakeMemoryStore) Remember(ctx context.Context, mem memory.NewMemory) (memory.Memory, error) {
	return memory.Memory{}, nil
}

func (f *fakeMemoryStore) Search(ctx context.Context, q memory.Query) ([]memory.Memory, error) {
	return nil, nil
}

func (f *fakeMemoryStore) GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (memory.Memory, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.memories[id]
	if !ok || f.scopes[id] != scopeID {
		return memory.Memory{}, false, nil
	}
	return m, true, nil
}

func (f *fakeMemoryStore) Forget(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.memories[id]; !ok {
		return nil
	}
	delete(f.memories, id)
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeMemoryStore) Reindex(ctx context.Context) error { return nil }

var _ memory.Store = &fakeMemoryStore{}

// fakeExecutor est un action.Executor factice, permettant de tester
// l'exécution d'actions "MCP-like" sans transport réel (autorisé par
// PLAN.md Phase 15, "un exécuteur fake injectable si c'est plus simple").
type fakeExecutor struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (f *fakeExecutor) Execute(ctx context.Context, identity model.ExecutionIdentity, plan persistence.ActionPlan, act persistence.Action, args map[string]any) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.fail {
		return "", errFakeExecution
	}
	return "ok", nil
}

var errFakeExecution = &fakeExecErr{}

type fakeExecErr struct{}

func (e *fakeExecErr) Error() string { return "échec simulé de l'outil" }

// memoryForgetProposal construit une delegation.ProposedAction pour
// supprimer la mémoire id, dans la portée personnelle du principal
// "alice".
func memoryForgetProposal(id, summary string) delegation.ProposedAction {
	return delegation.ProposedAction{
		Summary:            summary,
		MCPServer:          action.InternalServer,
		ToolName:           action.MemoryForgetTool,
		Arguments:          map[string]any{"id": id},
		RequiredPermission: "memory.personal.delete",
		Scope:              model.ScopePersonal,
		ScopeID:            "alice",
	}
}

// --- tests --------------------------------------------------------------

func TestEngine_ConfirmationValide(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, planText, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if !strings.Contains(planText, "Supprimer la mémoire m1") {
		t.Fatalf("texte de proposition inattendu: %q", planText)
	}

	cmd, ok := action.ParseCommand("confirmer")
	if !ok {
		t.Fatal("ParseCommand(\"confirmer\") a échoué")
	}

	report, err := engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "succès") {
		t.Fatalf("rapport de succès attendu: %q", report)
	}

	if _, found, _ := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", "m1"); found {
		t.Fatal("la mémoire n'a pas été supprimée")
	}
}

func TestEngine_MauvaisUtilisateur(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	creator := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), creator, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// eve tente de confirmer dans la même conversation (au sens de
	// ConversationID) qu'alice, mais n'est pas l'auteure de la proposition.
	wrongUser := privateIdentity("eve", "conv-1")

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), wrongUser, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "pas autorisé") {
		t.Fatalf("refus attendu, obtenu: %q", report)
	}

	// La mémoire existe toujours et le plan est resté en attente : une
	// confirmation ultérieure par le bon principal doit encore fonctionner.
	if _, found, _ := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", "m1"); !found {
		t.Fatal("la mémoire a été supprimée malgré un confirmateur non autorisé")
	}

	report, err = engine.HandleCommand(context.Background(), creator, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand (bon utilisateur): %v", err)
	}
	if !strings.Contains(report, "succès") {
		t.Fatalf("le bon utilisateur devrait pouvoir confirmer: %q", report)
	}
}

func TestEngine_MauvaiseConversation(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	creator := privateIdentity("alice", "conv-1")
	ensureConversation(t, db, testConversation("conv-1"))

	_, _, err := engine.CreatePlan(context.Background(), creator, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	otherConv := testConversation("conv-2")
	identityInOtherConv := privateIdentity("alice", "conv-2")

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), identityInOtherConv, otherConv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "Aucun plan") {
		t.Fatalf("aucun plan attendu dans une autre conversation, obtenu: %q", report)
	}
}

func TestEngine_PlanExpire(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))

	now := time.Now()
	clock := &now

	engine := action.NewEngine(db, authorizer, nil, appConfig(true),
		action.WithMemoryStore(store),
		action.WithPlanTTL(time.Minute),
		action.WithClock(func() time.Time { return *clock }),
	)

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Avance l'horloge au-delà de l'expiration.
	later := now.Add(2 * time.Minute)
	clock = &later

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "expiré") {
		t.Fatalf("refus pour expiration attendu, obtenu: %q", report)
	}

	if _, found, _ := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", "m1"); !found {
		t.Fatal("la mémoire ne devrait pas avoir été supprimée (plan expiré)")
	}
}

func TestEngine_PlanAnnule(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	cancelCmd, _ := action.ParseCommand("annuler")
	report, err := engine.HandleCommand(context.Background(), identity, conv, cancelCmd)
	if err != nil {
		t.Fatalf("HandleCommand (annuler): %v", err)
	}
	if !strings.Contains(report, "annulé") {
		t.Fatalf("annulation attendue, obtenu: %q", report)
	}

	if _, found, _ := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", "m1"); !found {
		t.Fatal("la mémoire ne devrait pas avoir été supprimée (plan annulé)")
	}

	// Une confirmation ultérieure doit être refusée.
	confirmCmd, _ := action.ParseCommand("confirmer")
	report, err = engine.HandleCommand(context.Background(), identity, conv, confirmCmd)
	if err != nil {
		t.Fatalf("HandleCommand (confirmer après annulation): %v", err)
	}
	if !strings.Contains(report, "Aucun plan") {
		t.Fatalf("plus aucun plan actif après annulation, obtenu: %q", report)
	}
}

func TestEngine_DoubleConfirmation(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	cmd, _ := action.ParseCommand("confirmer")

	report, err := engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand (1ère confirmation): %v", err)
	}
	if !strings.Contains(report, "succès") {
		t.Fatalf("succès attendu à la première confirmation, obtenu: %q", report)
	}
	if len(store.forgotten) != 1 {
		t.Fatalf("suppression unique attendue, obtenu: %v", store.forgotten)
	}

	// PLAN.md §10.4 : aucun plan actif restant, donc "aucun plan" plutôt
	// qu'une double exécution.
	report, err = engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand (2e confirmation): %v", err)
	}
	if !strings.Contains(report, "Aucun plan") {
		t.Fatalf("aucune nouvelle exécution attendue, obtenu: %q", report)
	}
	if len(store.forgotten) != 1 {
		t.Fatalf("aucune double suppression attendue, obtenu: %v", store.forgotten)
	}
}

func TestEngine_PermissionsRetirees(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note protégée", "alice")

	// Au moment de la proposition, alice a memory.personal.delete.
	authorizerAtProposal := authorization.NewAuthorizer(appConfig(true))
	engineAtProposal := action.NewEngine(db, authorizerAtProposal, nil, appConfig(true), action.WithMemoryStore(store))

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	fake := &fakeExecutor{}

	_, _, err := engineAtProposal.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire protégée"),
		{
			Summary:            "Action annexe toujours autorisée",
			MCPServer:          "fake-server",
			ToolName:           "fake-tool",
			Arguments:          map[string]any{},
			RequiredPermission: "memory.personal.write",
			Scope:              model.ScopePersonal,
			ScopeID:            "alice",
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Au moment de la confirmation, un nouvel Engine est construit sur la
	// MÊME base avec un autorizer où memory.personal.delete a été retiré du
	// rôle d'alice (memory.personal.write reste accordé) : PLAN.md §10.5,
	// "ne jamais faire confiance uniquement à l'autorisation obtenue lors
	// de la proposition".
	authorizerAtConfirm := authorization.NewAuthorizer(appConfig(false))
	engineAtConfirm := action.NewEngine(db, authorizerAtConfirm, nil, appConfig(false),
		action.WithMemoryStore(store),
		action.WithExecutor("fake-server", fake),
	)

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engineAtConfirm.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "partiellement") {
		t.Fatalf("exécution partielle attendue, obtenu: %q", report)
	}

	if _, found, _ := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", "m1"); !found {
		t.Fatal("la mémoire protégée n'aurait pas dû être supprimée (permission retirée)")
	}
	if fake.calls != 1 {
		t.Fatalf("l'action annexe (toujours autorisée) aurait dû s'exécuter, calls=%d", fake.calls)
	}
}

func TestEngine_ActionPartiellementReussie(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	authorizer := authorization.NewAuthorizer(appConfig(true))

	failing := &fakeExecutor{fail: true}
	succeeding := &fakeExecutor{}

	engine := action.NewEngine(db, authorizer, nil, appConfig(true),
		action.WithExecutor("server-ok", succeeding),
		action.WithExecutor("server-fail", failing),
	)

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		{
			Summary:            "Action qui réussit",
			MCPServer:          "server-ok",
			ToolName:           "tool-ok",
			RequiredPermission: "memory.personal.write",
			Scope:              model.ScopePersonal,
			ScopeID:            "alice",
		},
		{
			Summary:            "Action qui échoue",
			MCPServer:          "server-fail",
			ToolName:           "tool-fail",
			RequiredPermission: "memory.personal.write",
			Scope:              model.ScopePersonal,
			ScopeID:            "alice",
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if !strings.Contains(report, "partiellement") {
		t.Fatalf("exécution partielle attendue, obtenu: %q", report)
	}
	if !strings.Contains(report, "réussi") || !strings.Contains(report, "échec") {
		t.Fatalf("rapport détaillé attendu (succès ET échec), obtenu: %q", report)
	}
	if succeeding.calls != 1 || failing.calls != 1 {
		t.Fatalf("les deux actions auraient dû être tentées: ok=%d fail=%d", succeeding.calls, failing.calls)
	}
}

func TestEngine_RedemarrageAvantConfirmation(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "alice")

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	identity := privateIdentity("alice", "conv-1")
	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	_, _, err := engine.CreatePlan(context.Background(), identity, []delegation.ProposedAction{
		memoryForgetProposal("m1", "Supprimer la mémoire m1"),
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Simule un redémarrage du processus : fermeture de la connexion, puis
	// réouverture d'une nouvelle instance *persistence.DB sur le même
	// fichier, et construction d'un nouvel Engine dessus.
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	db2, err := persistence.Open(context.Background(), storageCfg)
	if err != nil {
		t.Fatalf("persistence.Open (redémarrage): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	engine2 := action.NewEngine(db2, authorizer, nil, appConfig(true), action.WithMemoryStore(store))

	cmd, _ := action.ParseCommand("confirmer")
	report, err := engine2.HandleCommand(context.Background(), identity, conv, cmd)
	if err != nil {
		t.Fatalf("HandleCommand après redémarrage: %v", err)
	}
	if !strings.Contains(report, "succès") {
		t.Fatalf("succès attendu après redémarrage, obtenu: %q", report)
	}

	if _, found, _ := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", "m1"); found {
		t.Fatal("la mémoire aurait dû être supprimée après redémarrage")
	}
}

// --- Phase 18 : reprise des plans interrompus ---------------------------

func TestEngine_RecoverInterrupted_NotStarted(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	authorizer := authorization.NewAuthorizer(appConfig(true))
	fake := &fakeExecutor{}
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithExecutor("server-x", fake))

	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	now := time.Now().UTC().Format(time.RFC3339)
	plan := persistence.ActionPlan{
		ID:             persistence.ActionPlanID(uuid.NewString()),
		OrgID:          conv.OrgID,
		ConversationID: conv.ID,
		CreatedBy:      "alice",
		Scope:          model.ScopePersonal,
		ScopeID:        "alice",
		Status:         action.StatusExecuting,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	act := persistence.Action{
		ID:                 persistence.ActionID(uuid.NewString()),
		PlanID:             plan.ID,
		Position:           0,
		MCPServer:          "server-x",
		ToolName:           "tool-x",
		ArgumentsJSON:      "{}",
		Summary:            "Action jamais commencée",
		RequiredPermission: "memory.personal.write",
		Status:             "proposed",
		CreatedAt:          now,
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		plans := persistence.NewActionPlanRepository()
		if err := plans.Insert(context.Background(), tx, plan); err != nil {
			return err
		}
		actions := persistence.NewActionRepository()
		return actions.Insert(context.Background(), tx, act)
	})
	if err != nil {
		t.Fatalf("insertion directe du plan interrompu: %v", err)
	}

	if err := engine.RecoverInterrupted(context.Background()); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	reloadedAct := findAction(t, db, act.ID)
	if reloadedAct.Status != action.StatusFailed {
		t.Fatalf("statut de l'action non commencée: got %q, expected %q", reloadedAct.Status, action.StatusFailed)
	}
	if reloadedAct.ErrorCode == nil || *reloadedAct.ErrorCode != "interrupted_not_started" {
		t.Fatalf("error_code attendu %q, obtenu %v", "interrupted_not_started", reloadedAct.ErrorCode)
	}

	reloadedPlan := findPlan(t, db, plan.ID)
	if reloadedPlan.Status != action.StatusFailed {
		t.Fatalf("statut du plan: got %q, expected %q", reloadedPlan.Status, action.StatusFailed)
	}

	if fake.calls != 0 {
		t.Fatalf("l'exécuteur n'aurait jamais dû être appelé, calls=%d", fake.calls)
	}

	// Idempotence : un second appel ne change plus rien (aucun plan
	// "executing" ne subsiste).
	if err := engine.RecoverInterrupted(context.Background()); err != nil {
		t.Fatalf("second RecoverInterrupted: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("l'exécuteur n'aurait toujours pas dû être appelé, calls=%d", fake.calls)
	}
}

func TestEngine_RecoverInterrupted_UnknownOutcome(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	authorizer := authorization.NewAuthorizer(appConfig(true))
	fake := &fakeExecutor{}
	engine := action.NewEngine(db, authorizer, nil, appConfig(true), action.WithExecutor("server-x", fake))

	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	now := time.Now().UTC().Format(time.RFC3339)
	plan := persistence.ActionPlan{
		ID:             persistence.ActionPlanID(uuid.NewString()),
		OrgID:          conv.OrgID,
		ConversationID: conv.ID,
		CreatedBy:      "alice",
		Scope:          model.ScopePersonal,
		ScopeID:        "alice",
		Status:         action.StatusExecuting,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	act := persistence.Action{
		ID:                 persistence.ActionID(uuid.NewString()),
		PlanID:             plan.ID,
		Position:           0,
		MCPServer:          "server-x",
		ToolName:           "tool-x",
		ArgumentsJSON:      "{}",
		Summary:            "Action interrompue après l'appel externe",
		RequiredPermission: "memory.personal.write",
		Status:             action.StatusExecuting,
		CreatedAt:          now,
		StartedAt:          &now,
		// CompletedAt volontairement nil : simule un crash entre l'appel
		// MCP et l'enregistrement du résultat.
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		plans := persistence.NewActionPlanRepository()
		if err := plans.Insert(context.Background(), tx, plan); err != nil {
			return err
		}
		actions := persistence.NewActionRepository()
		return actions.Insert(context.Background(), tx, act)
	})
	if err != nil {
		t.Fatalf("insertion directe du plan interrompu: %v", err)
	}

	if err := engine.RecoverInterrupted(context.Background()); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	reloadedAct := findAction(t, db, act.ID)
	if reloadedAct.Status != action.StatusFailed {
		t.Fatalf("statut de l'action interrompue: got %q, expected %q", reloadedAct.Status, action.StatusFailed)
	}
	if reloadedAct.ErrorCode == nil || *reloadedAct.ErrorCode != "interrupted_unknown_outcome" {
		t.Fatalf("error_code attendu %q, obtenu %v", "interrupted_unknown_outcome", reloadedAct.ErrorCode)
	}
	if reloadedAct.CompletedAt == nil {
		t.Fatal("completed_at aurait dû être renseigné par la récupération")
	}

	reloadedPlan := findPlan(t, db, plan.ID)
	if reloadedPlan.Status != action.StatusFailed {
		t.Fatalf("statut du plan: got %q, expected %q", reloadedPlan.Status, action.StatusFailed)
	}

	if fake.calls != 0 {
		t.Fatalf("l'exécuteur n'aurait jamais dû être rappelé, calls=%d", fake.calls)
	}
}

func TestEngine_RecoverInterrupted_PartiallySucceeded(t *testing.T) {
	storageCfg := testStorageConfig(t)
	db := openTestDB(t, storageCfg)

	authorizer := authorization.NewAuthorizer(appConfig(true))
	engine := action.NewEngine(db, authorizer, nil, appConfig(true))

	conv := testConversation("conv-1")
	ensureConversation(t, db, conv)

	now := time.Now().UTC().Format(time.RFC3339)
	plan := persistence.ActionPlan{
		ID:             persistence.ActionPlanID(uuid.NewString()),
		OrgID:          conv.OrgID,
		ConversationID: conv.ID,
		CreatedBy:      "alice",
		Scope:          model.ScopePersonal,
		ScopeID:        "alice",
		Status:         action.StatusExecuting,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	succeededAct := persistence.Action{
		ID:                 persistence.ActionID(uuid.NewString()),
		PlanID:             plan.ID,
		Position:           0,
		MCPServer:          "server-x",
		ToolName:           "tool-x",
		ArgumentsJSON:      "{}",
		Summary:            "Action déjà réussie avant le crash",
		RequiredPermission: "memory.personal.write",
		Status:             action.StatusSucceeded,
		CreatedAt:          now,
		StartedAt:          &now,
		CompletedAt:        &now,
	}
	interruptedAct := persistence.Action{
		ID:                 persistence.ActionID(uuid.NewString()),
		PlanID:             plan.ID,
		Position:           1,
		MCPServer:          "server-x",
		ToolName:           "tool-y",
		ArgumentsJSON:      "{}",
		Summary:            "Action interrompue par le crash",
		RequiredPermission: "memory.personal.write",
		Status:             action.StatusExecuting,
		CreatedAt:          now,
		StartedAt:          &now,
	}

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		plans := persistence.NewActionPlanRepository()
		if err := plans.Insert(context.Background(), tx, plan); err != nil {
			return err
		}
		actions := persistence.NewActionRepository()
		if err := actions.Insert(context.Background(), tx, succeededAct); err != nil {
			return err
		}
		return actions.Insert(context.Background(), tx, interruptedAct)
	})
	if err != nil {
		t.Fatalf("insertion directe du plan interrompu: %v", err)
	}

	if err := engine.RecoverInterrupted(context.Background()); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	reloadedPlan := findPlan(t, db, plan.ID)
	if reloadedPlan.Status != action.StatusPartiallySucceeded {
		t.Fatalf("statut du plan: got %q, expected %q", reloadedPlan.Status, action.StatusPartiallySucceeded)
	}

	reloadedSucceeded := findAction(t, db, succeededAct.ID)
	if reloadedSucceeded.Status != action.StatusSucceeded {
		t.Fatalf("l'action déjà réussie ne devrait pas être modifiée, got %q", reloadedSucceeded.Status)
	}

	reloadedInterrupted := findAction(t, db, interruptedAct.ID)
	if reloadedInterrupted.Status != action.StatusFailed {
		t.Fatalf("statut de l'action interrompue: got %q, expected %q", reloadedInterrupted.Status, action.StatusFailed)
	}
	if reloadedInterrupted.ErrorCode == nil || *reloadedInterrupted.ErrorCode != "interrupted_unknown_outcome" {
		t.Fatalf("error_code attendu %q, obtenu %v", "interrupted_unknown_outcome", reloadedInterrupted.ErrorCode)
	}
}

func findAction(t *testing.T, db *persistence.DB, id persistence.ActionID) persistence.Action {
	t.Helper()

	var (
		a     persistence.Action
		found bool
	)
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		a, found, err = persistence.NewActionRepository().FindByID(context.Background(), tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("findAction: %v", err)
	}
	if !found {
		t.Fatalf("findAction: action %q introuvable", id)
	}
	return a
}

func findPlan(t *testing.T, db *persistence.DB, id persistence.ActionPlanID) persistence.ActionPlan {
	t.Helper()

	var (
		p     persistence.ActionPlan
		found bool
	)
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		p, found, err = persistence.NewActionPlanRepository().FindByID(context.Background(), tx, id)
		return err
	})
	if err != nil {
		t.Fatalf("findPlan: %v", err)
	}
	if !found {
		t.Fatalf("findPlan: plan %q introuvable", id)
	}
	return p
}
