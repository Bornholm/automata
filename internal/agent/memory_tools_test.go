package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
)

// fakeMemoryStore implémente memory.Store en mémoire, pour tester
// l'orchestration des outils search_memory/remember/forget_memory
// (cloisonnement, confirmation) indépendamment d'un Amoxtli réel : c'est ce
// dernier qui est testé avec un Amoxtli réel dans internal/memory (PLAN.md
// §16). Ce que ce fichier teste, c'est une couche différente : comment
// internal/agent applique internal/authorization.Authorizer et résout la
// portée avant d'appeler Store, jamais la persistance elle-même.
type fakeMemoryStore struct {
	mu       sync.Mutex
	memories map[string]memory.Memory
	scopes   map[string]struct {
		orgID   model.OrgID
		scope   model.Scope
		scopeID model.ScopeID
	}
	forgotten     []string
	rememberCalls []memory.NewMemory
}

func newFakeMemoryStore() *fakeMemoryStore {
	return &fakeMemoryStore{
		memories: map[string]memory.Memory{},
		scopes: map[string]struct {
			orgID   model.OrgID
			scope   model.Scope
			scopeID model.ScopeID
		}{},
	}
}

func (f *fakeMemoryStore) seed(id, content string, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memories[id] = memory.Memory{ID: id, Content: content}
	f.scopes[id] = struct {
		orgID   model.OrgID
		scope   model.Scope
		scopeID model.ScopeID
	}{orgID, scope, scopeID}
}

func (f *fakeMemoryStore) Remember(ctx context.Context, mem memory.NewMemory) (memory.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := fmt.Sprintf("mem-%d", len(f.memories)+1)
	f.memories[id] = memory.Memory{ID: id, Content: mem.Content}
	f.scopes[id] = struct {
		orgID   model.OrgID
		scope   model.Scope
		scopeID model.ScopeID
	}{mem.OrgID, mem.Scope, mem.ScopeID}
	f.rememberCalls = append(f.rememberCalls, mem)

	return f.memories[id], nil
}

func (f *fakeMemoryStore) Search(ctx context.Context, q memory.Query) ([]memory.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var results []memory.Memory
	for id, m := range f.memories {
		s := f.scopes[id]
		if s.orgID != q.OrgID || s.scope != q.Scope || s.scopeID != q.ScopeID {
			continue
		}
		if !strings.Contains(m.Content, q.Text) {
			continue
		}
		results = append(results, m)
	}
	return results, nil
}

func (f *fakeMemoryStore) GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (memory.Memory, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	m, ok := f.memories[id]
	if !ok {
		return memory.Memory{}, false, nil
	}
	s := f.scopes[id]
	if s.orgID != orgID || s.scope != scope || s.scopeID != scopeID {
		return memory.Memory{}, false, nil
	}
	return m, true, nil
}

func (f *fakeMemoryStore) Forget(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.memories, id)
	delete(f.scopes, id)
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeMemoryStore) Reindex(ctx context.Context) error { return nil }

var _ memory.Store = &fakeMemoryStore{}

// memoryTestConfig construit une configuration minimale suffisante pour
// authorization.NewAuthorizer : une seule organisation "home", des rôles
// aux permissions mémoire variées pour couvrir les cas de test (lecture
// seule org, pas de suppression, etc.).
func memoryTestConfig() *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: []string{
					"memory.personal.read", "memory.personal.write", "memory.personal.delete",
					"memory.group.read", "memory.group.write", "memory.group.delete",
					"memory.org.read", "memory.org.write", "memory.org.delete",
				}},
				"org-reader-only": {Permissions: []string{"memory.org.read"}},
				"no-delete":       {Permissions: []string{"memory.personal.read", "memory.personal.write"}},
			},
			Principals: []config.Principal{
				{ID: "alice", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
				{ID: "leo", Kind: config.PrincipalKindHuman, DisplayName: "Léo", Roles: []string{"org-reader-only"}},
				{ID: "bob", Kind: config.PrincipalKindHuman, DisplayName: "Bob", Roles: []string{"no-delete"}},
			},
		},
	}
}

func privateIdentity(principalID model.PrincipalID) model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    principalID,
		OrgID:          "home",
		ConversationID: model.ConversationID("conv-" + string(principalID)),
		Provider:       "test",
		ChannelID:      "chan-" + string(principalID),
		ChannelKind:    model.ChannelPrivate,
		Scope:          model.ScopePersonal,
		ScopeID:        model.ScopeID(principalID),
	}
}

func groupIdentity(principalID model.PrincipalID, groupID model.ScopeID) model.ExecutionIdentity {
	return model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    principalID,
		OrgID:          "home",
		ConversationID: model.ConversationID("conv-group-" + string(groupID)),
		Provider:       "test",
		ChannelID:      "chan-group-" + string(groupID),
		ChannelKind:    model.ChannelGroup,
		Scope:          model.ScopeGroup,
		ScopeID:        groupID,
	}
}

// toolCallArgs sérialise args en JSON pour construire un llm.ToolCall
// scripté.
func toolCallArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(raw)
}

// executeMemoryTool construit un OrchestratorAgent doté de tools, l'exécute
// avec un client scripté qui déclenche un unique tool-call (name, args) puis
// renvoie une réponse finale, et retourne le texte du résultat d'outil
// observé au tour suivant (ce que le modèle verrait réellement).
func executeMemoryTool(t *testing.T, tools agent.MemoryTools, identity model.ExecutionIdentity, name string, args map[string]any) string {
	t.Helper()

	var toolResultText string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", name, toolCallArgs(t, args))), nil
			}

			for _, m := range opts.Messages {
				if m.Role() == llm.RoleTool {
					toolResultText = m.Content()
				}
			}

			return scriptedFinalResponse("ok"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", "Test Org", map[string]delegation.Specialist{}, 5).WithMemoryTools(tools)

	if _, err := a.Execute(context.Background(), agent.Request{Identity: identity, Input: "test"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	return toolResultText
}

func TestSearchMemory_PrivateConversationCoversPersonalAndOrgOnly(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m-personal", "note personnelle d'alice", "home", model.ScopePersonal, "alice")
	store.seed("m-org", "note partagée org", "home", model.ScopeOrg, "home")
	store.seed("m-group", "note du groupe main-group", "home", model.ScopeGroup, "main-group")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Search:     true,
	}

	text := executeMemoryTool(t, tools, privateIdentity("alice"), "search_memory", map[string]any{"query": "note"})

	if !strings.Contains(text, "m-personal") {
		t.Errorf("résultat personnel manquant: %q", text)
	}
	if !strings.Contains(text, "m-org") {
		t.Errorf("résultat org manquant: %q", text)
	}
	if strings.Contains(text, "m-group") {
		t.Errorf("fuite: résultat de groupe présent dans une conversation privée: %q", text)
	}
}

func TestSearchMemory_UnauthorizedScopeIgnoredSilently(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m-personal", "note personnelle de léo", "home", model.ScopePersonal, "leo")
	store.seed("m-org", "note partagée org", "home", model.ScopeOrg, "home")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Search:     true,
	}

	// leo n'a que memory.org.read (voir memoryTestConfig) : la recherche ne
	// doit pas échouer, elle doit simplement ignorer la portée personnelle
	// non autorisée (PLAN.md, Phase 10).
	text := executeMemoryTool(t, tools, privateIdentity("leo"), "search_memory", map[string]any{"query": "note"})

	if strings.Contains(text, "m-personal") {
		t.Errorf("résultat personnel non autorisé présent: %q", text)
	}
	if !strings.Contains(text, "m-org") {
		t.Errorf("résultat org (autorisé) manquant: %q", text)
	}
}

func TestSearchMemory_GroupConversationNeverCoversPersonal(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m-personal", "note personnelle d'alice", "home", model.ScopePersonal, "alice")
	store.seed("m-group", "note du groupe main-group", "home", model.ScopeGroup, "main-group")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Search:     true,
	}

	text := executeMemoryTool(t, tools, groupIdentity("alice", "main-group"), "search_memory", map[string]any{"query": "note"})

	if !strings.Contains(text, "m-group") {
		t.Errorf("résultat de groupe manquant: %q", text)
	}
	if strings.Contains(text, "m-personal") {
		t.Errorf("fuite: résultat personnel présent dans une conversation de groupe: %q", text)
	}
}

func TestSearchMemory_CrossGroupIsolation(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m-group-a", "secret du groupe A", "home", model.ScopeGroup, "group-a")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Search:     true,
	}

	text := executeMemoryTool(t, tools, groupIdentity("alice", "group-b"), "search_memory", map[string]any{"query": "secret"})

	if strings.Contains(text, "m-group-a") {
		t.Errorf("fuite entre groupes: %q", text)
	}
}

func TestRemember_WritesToConversationScopeOnly(t *testing.T) {
	store := newFakeMemoryStore()

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Remember:   true,
	}

	identity := privateIdentity("alice")
	text := executeMemoryTool(t, tools, identity, "remember", map[string]any{"content": "Alice aime le thé vert."})

	if !strings.Contains(text, "Mémorisé") {
		t.Fatalf("résultat inattendu: %q", text)
	}

	if len(store.rememberCalls) != 1 {
		t.Fatalf("Remember appelé %d fois, voulu 1", len(store.rememberCalls))
	}

	call := store.rememberCalls[0]
	if call.Scope != model.ScopePersonal {
		t.Errorf("Scope = %q, voulu personal", call.Scope)
	}
	if call.ScopeID != "alice" {
		t.Errorf("ScopeID = %q, voulu alice", call.ScopeID)
	}
	if call.OwnerPrincipalID != "alice" || call.CreatedBy != "alice" {
		t.Errorf("propriétaire/auteur inattendus: %+v", call)
	}
	if call.SourceConversationID != identity.ConversationID {
		t.Errorf("SourceConversationID = %q, voulu %q", call.SourceConversationID, identity.ConversationID)
	}
}

// TestRemember_NeverWritesOrgFromPrivateConversation vérifie qu'une
// conversation privée ne peut jamais écrire en portée org via remember,
// même si le principal a par ailleurs memory.org.write (règle invariante de
// internal/authorization, PLAN.md §3.2, §8.4 : "JAMAIS org"). La portée
// n'étant pas un paramètre exposé au modèle, ce test constate simplement
// qu'un remember en conversation privée produit toujours un appel Store en
// portée personal, jamais org, quel que soit le contenu demandé.
func TestRemember_NeverWritesOrgFromPrivateConversation(t *testing.T) {
	store := newFakeMemoryStore()

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Remember:   true,
	}

	executeMemoryTool(t, tools, privateIdentity("alice"), "remember", map[string]any{"content": "Une information quelconque."})

	if len(store.rememberCalls) != 1 {
		t.Fatalf("Remember appelé %d fois, voulu 1", len(store.rememberCalls))
	}
	if store.rememberCalls[0].Scope == model.ScopeOrg {
		t.Fatalf("écriture org depuis une conversation privée: %+v", store.rememberCalls[0])
	}
}

func TestForgetMemory_AmbiguousQueryListsWithoutDeleting(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m1", "rendez-vous chez le dentiste", "home", model.ScopePersonal, "alice")
	store.seed("m2", "rendez-vous chez le médecin", "home", model.ScopePersonal, "alice")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Forget:     true,
	}

	text := executeMemoryTool(t, tools, privateIdentity("alice"), "forget_memory", map[string]any{"query": "rendez-vous"})

	if !strings.Contains(text, "m1") || !strings.Contains(text, "m2") {
		t.Errorf("liste de candidats incomplète: %q", text)
	}
	if len(store.forgotten) != 0 {
		t.Fatalf("suppression déclenchée par une requête textuelle ambiguë: %v", store.forgotten)
	}

	// Les deux mémoires existent toujours.
	if len(store.memories) != 2 {
		t.Fatalf("mémoires perdues: %d restantes, voulu 2", len(store.memories))
	}
}

func TestForgetMemory_ConfirmationFlow(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m1", "note à supprimer", "home", model.ScopePersonal, "alice")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Forget:     true,
	}

	identity := privateIdentity("alice")

	// id fourni, confirm=false: doit décrire la mémoire et demander une
	// confirmation, sans rien supprimer.
	text := executeMemoryTool(t, tools, identity, "forget_memory", map[string]any{"id": "m1", "confirm": false})
	if !strings.Contains(text, "note à supprimer") {
		t.Errorf("contenu de confirmation manquant: %q", text)
	}
	if len(store.forgotten) != 0 {
		t.Fatalf("suppression prématurée avant confirmation: %v", store.forgotten)
	}

	// id fourni, confirm=true: supprime effectivement.
	text = executeMemoryTool(t, tools, identity, "forget_memory", map[string]any{"id": "m1", "confirm": true})
	if !strings.Contains(text, "supprimée") {
		t.Errorf("confirmation de suppression manquante: %q", text)
	}
	if len(store.forgotten) != 1 || store.forgotten[0] != "m1" {
		t.Fatalf("Forget: appels inattendus: %v", store.forgotten)
	}

	// La mémoire n'est plus retrouvable.
	if _, ok := store.memories["m1"]; ok {
		t.Errorf("mémoire toujours présente après suppression confirmée")
	}
}

func TestForgetMemory_UnauthorizedDeleteNeverCallsStore(t *testing.T) {
	store := newFakeMemoryStore()
	store.seed("m1", "note de bob", "home", model.ScopePersonal, "bob")

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Forget:     true,
	}

	// bob n'a que memory.personal.read/write, jamais memory.personal.delete
	// (voir memoryTestConfig).
	identity := privateIdentity("bob")

	text := executeMemoryTool(t, tools, identity, "forget_memory", map[string]any{"id": "m1", "confirm": false})
	if strings.Contains(text, "note de bob") {
		t.Errorf("contenu exposé malgré l'absence d'autorisation de suppression: %q", text)
	}
	if len(store.forgotten) != 0 {
		t.Fatalf("Forget appelé malgré l'absence d'autorisation: %v", store.forgotten)
	}
	if _, ok := store.memories["m1"]; !ok {
		t.Errorf("mémoire supprimée à tort")
	}

	// Même en tentant directement confirm=true : la revérification
	// explicite avant l'action destructive doit aussi échouer, puisque le
	// GetByID initial échoue déjà à trouver une portée autorisée.
	text = executeMemoryTool(t, tools, identity, "forget_memory", map[string]any{"id": "m1", "confirm": true})
	if strings.Contains(text, "supprimée") {
		t.Fatalf("suppression effectuée malgré l'absence d'autorisation: %q", text)
	}
	if len(store.forgotten) != 0 {
		t.Fatalf("Forget appelé malgré l'absence d'autorisation: %v", store.forgotten)
	}
}

func TestMemoryTools_DisabledWhenFlagFalse(t *testing.T) {
	store := newFakeMemoryStore()

	tools := agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(memoryTestConfig()),
		Search:     false,
		Remember:   false,
		Forget:     false,
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, tool := range opts.Tools {
				if tool.Name() == "search_memory" || tool.Name() == "remember" || tool.Name() == "forget_memory" {
					t.Errorf("outil mémoire %q exposé alors que désactivé", tool.Name())
				}
			}
			return scriptedFinalResponse("ok"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", "Test Org", map[string]delegation.Specialist{}, 5).WithMemoryTools(tools)

	if _, err := a.Execute(context.Background(), agent.Request{Identity: privateIdentity("alice"), Input: "test"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
