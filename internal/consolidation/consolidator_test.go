package consolidation

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()

	cfg := config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}

	db, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// fakeStore implémente memory.Store en mémoire, avec les métadonnées de
// portée nécessaires au regroupement du consolidateur.
type fakeStore struct {
	mu         sync.Mutex
	memories   map[string]memory.Memory
	nextID     int
	remembered []memory.NewMemory
	forgotten  []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{memories: map[string]memory.Memory{}}
}

func (s *fakeStore) seed(id, content string, orgID, scope, scopeID, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memories[id] = memory.Memory{
		ID:      id,
		Content: content,
		Metadata: map[string]string{
			"org_id":             orgID,
			"scope":              scope,
			"scope_id":           scopeID,
			"owner_principal_id": owner,
		},
		CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (s *fakeStore) Remember(ctx context.Context, mem memory.NewMemory) (memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	id := fmt.Sprintf("new-%d", s.nextID)
	s.memories[id] = memory.Memory{ID: id, Content: mem.Content, Metadata: map[string]string{
		"org_id":   string(mem.OrgID),
		"scope":    string(mem.Scope),
		"scope_id": string(mem.ScopeID),
	}}
	s.remembered = append(s.remembered, mem)
	return s.memories[id], nil
}

func (s *fakeStore) Search(ctx context.Context, q memory.Query) ([]memory.Memory, error) {
	return nil, nil
}

func (s *fakeStore) GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (memory.Memory, bool, error) {
	return memory.Memory{}, false, nil
}

func (s *fakeStore) Forget(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.memories, id)
	s.forgotten = append(s.forgotten, id)
	return nil
}

func (s *fakeStore) Reindex(ctx context.Context) error { return nil }

func (s *fakeStore) List(ctx context.Context) ([]memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var all []memory.Memory
	for _, m := range s.memories {
		all = append(all, m)
	}
	return all, nil
}

var _ memory.Store = &fakeStore{}

// scriptedClient retourne la même réponse à chaque appel et compte les
// appels.
type scriptedClient struct {
	mu       sync.Mutex
	response string
	calls    int
}

func (c *scriptedClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, c.response), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

func newConsolidator(t *testing.T, store memory.Store, client llm.ChatCompletionClient, minMemories int) *Consolidator {
	t.Helper()

	c, err := New(openTestDB(t), store, client, config.MemoryConsolidation{
		Enabled:     true,
		MinMemories: minMemories,
	}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestConsolidate_AppliesMergesAndForgets(t *testing.T) {
	store := newFakeStore()
	store.seed("a", "Alice aime le thé", "home", "personal", "alice", "alice")
	store.seed("b", "Alice préfère le thé au café", "home", "personal", "alice", "alice")
	store.seed("c", "Il pleuvait le 3 août", "home", "personal", "alice", "alice")

	client := &scriptedClient{response: `{"merges": [{"ids": ["a", "b"], "content": "Alice préfère le thé au café"}], "forget": ["c"]}`}
	consolidator := newConsolidator(t, store, client, 2)

	if err := consolidator.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if len(store.remembered) != 1 {
		t.Fatalf("souvenirs écrits = %d, attendu 1", len(store.remembered))
	}

	merged := store.remembered[0]
	if merged.Content != "Alice préfère le thé au café" {
		t.Errorf("contenu fusionné = %q", merged.Content)
	}
	if merged.Scope != model.ScopePersonal || merged.ScopeID != model.ScopeID("alice") || merged.OrgID != model.OrgID("home") {
		t.Errorf("portée = %s/%s/%s, attendu celle du groupe", merged.OrgID, merged.Scope, merged.ScopeID)
	}
	if merged.OwnerPrincipalID != model.PrincipalID("alice") {
		t.Errorf("owner = %q, attendu alice (propriétaire uniforme)", merged.OwnerPrincipalID)
	}
	if merged.CreatedBy != consolidatorPrincipal {
		t.Errorf("created_by = %q, attendu %q", merged.CreatedBy, consolidatorPrincipal)
	}
	if merged.Origin != "consolidation" {
		t.Errorf("origin = %q, attendu \"consolidation\"", merged.Origin)
	}

	wantForgotten := map[string]bool{"a": true, "b": true, "c": true}
	if len(store.forgotten) != 3 {
		t.Fatalf("souvenirs oubliés = %v, attendu a, b et c", store.forgotten)
	}
	for _, id := range store.forgotten {
		if !wantForgotten[id] {
			t.Errorf("oubli inattendu de %q", id)
		}
	}
}

func TestConsolidate_ScopeBelowThresholdIsUntouched(t *testing.T) {
	store := newFakeStore()
	store.seed("a", "Alice aime le thé", "home", "personal", "alice", "alice")

	client := &scriptedClient{response: `{"merges": [], "forget": ["a"]}`}
	consolidator := newConsolidator(t, store, client, 2)

	if err := consolidator.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if client.calls != 0 {
		t.Errorf("appels llm = %d, attendu 0 sous le seuil", client.calls)
	}
	if len(store.forgotten) != 0 {
		t.Errorf("souvenirs oubliés = %v, attendu aucun", store.forgotten)
	}
}

func TestConsolidate_NeverCrossesScopes(t *testing.T) {
	store := newFakeStore()
	store.seed("a", "Fait personnel", "home", "personal", "alice", "alice")
	store.seed("b", "Fait de groupe", "home", "group", "famille", "alice")

	// Même avec un plan valide en apparence, chaque portée est traitée
	// séparément : sous le seuil de 2, aucune n'est consolidée.
	client := &scriptedClient{response: `{"merges": [{"ids": ["a", "b"], "content": "fuite"}], "forget": []}`}
	consolidator := newConsolidator(t, store, client, 2)

	if err := consolidator.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if client.calls != 0 {
		t.Errorf("appels llm = %d, attendu 0 (une mémoire par portée)", client.calls)
	}
}

func TestConsolidate_RejectsInvalidPlans(t *testing.T) {
	cases := []struct {
		name     string
		response string
		wantErr  string
	}{
		{"identifiant inconnu", `{"merges": [{"ids": ["a", "zz"], "content": "x"}], "forget": []}`, "identifiant inconnu"},
		{"identifiant réutilisé", `{"merges": [{"ids": ["a", "b"], "content": "x"}], "forget": ["a"]}`, "utilisé deux fois"},
		{"fusion à un seul souvenir", `{"merges": [{"ids": ["a"], "content": "x"}], "forget": []}`, "au moins deux"},
		{"fusion sans contenu", `{"merges": [{"ids": ["a", "b"], "content": "  "}], "forget": []}`, "sans contenu"},
		{"oublis excessifs", `{"merges": [], "forget": ["a", "b"]}`, "oublis secs"},
		{"réponse illisible", `pas du json`, "illisible"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.seed("a", "Fait un", "home", "personal", "alice", "alice")
			store.seed("b", "Fait deux", "home", "personal", "alice", "alice")
			store.seed("c", "Fait trois", "home", "personal", "alice", "alice")

			client := &scriptedClient{response: tc.response}
			consolidator := newConsolidator(t, store, client, 2)

			err := consolidator.Consolidate(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("erreur = %v, attendu une erreur contenant %q", err, tc.wantErr)
			}

			if len(store.remembered) != 0 || len(store.forgotten) != 0 {
				t.Errorf("un plan invalide ne doit rien appliquer (écrits=%d, oubliés=%v)", len(store.remembered), store.forgotten)
			}
		})
	}
}

func TestTick_FirstRunOnlyAnchors(t *testing.T) {
	store := newFakeStore()
	for i := range 3 {
		store.seed(fmt.Sprintf("m%d", i), "Fait", "home", "personal", "alice", "alice")
	}

	client := &scriptedClient{response: `{"merges": [], "forget": []}`}
	consolidator := newConsolidator(t, store, client, 2)

	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.Local)
	consolidator.WithClock(func() time.Time { return now })

	// Premier tick : ancrage seulement, aucune consolidation.
	if err := consolidator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick initial: %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("appels llm après ancrage = %d, attendu 0", client.calls)
	}

	// Avant l'échéance suivante (4h40 le lendemain) : rien.
	now = now.Add(2 * time.Hour)
	if err := consolidator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick avant échéance: %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("appels llm avant échéance = %d, attendu 0", client.calls)
	}

	// Après l'échéance : consolidation.
	now = now.Add(24 * time.Hour)
	if err := consolidator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick après échéance: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("appels llm après échéance = %d, attendu 1", client.calls)
	}

	// L'exécution est enregistrée : le tick suivant ne relance rien.
	if err := consolidator.Tick(context.Background()); err != nil {
		t.Fatalf("Tick de contrôle: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("appels llm au tick de contrôle = %d, attendu 1", client.calls)
	}
}

// Un insight est un nouveau souvenir de synthèse : écrit dans la portée du
// lot avec l'origine "reflection", sans supprimer les souvenirs sources.
func TestConsolidate_WritesInsightsWithoutRemovingSources(t *testing.T) {
	store := newFakeStore()
	store.seed("a", "Alice a demandé la météo lundi", "home", "personal", "alice", "alice")
	store.seed("b", "Alice a demandé la météo mardi", "home", "personal", "alice", "alice")

	client := &scriptedClient{response: `{"merges": [], "forget": [], "insights": ["Alice consulte la météo chaque matin"]}`}
	consolidator := newConsolidator(t, store, client, 2)

	if err := consolidator.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if len(store.forgotten) != 0 {
		t.Errorf("souvenirs oubliés = %v, attendu aucun : un insight ne remplace rien", store.forgotten)
	}
	if len(store.remembered) != 1 {
		t.Fatalf("souvenirs écrits = %d, attendu 1", len(store.remembered))
	}

	insight := store.remembered[0]
	if insight.Content != "Alice consulte la météo chaque matin" {
		t.Errorf("contenu = %q", insight.Content)
	}
	if insight.Origin != "reflection" {
		t.Errorf("origin = %q, attendu \"reflection\"", insight.Origin)
	}
	if insight.Scope != model.ScopePersonal || insight.ScopeID != model.ScopeID("alice") || insight.OrgID != model.OrgID("home") {
		t.Errorf("portée = %s/%s/%s, attendu celle du lot", insight.OrgID, insight.Scope, insight.ScopeID)
	}
	if insight.CreatedBy != consolidatorPrincipal {
		t.Errorf("created_by = %q, attendu %q", insight.CreatedBy, consolidatorPrincipal)
	}
	if insight.OwnerPrincipalID != "" {
		t.Errorf("owner = %q, attendu vide (la synthèse appartient à la portée)", insight.OwnerPrincipalID)
	}
}

// La réflexion est la partie la plus spéculative de la consolidation : un
// plan qui déborde du quota d'insights (ou en propose de vides) est refusé
// en bloc.
func TestConsolidate_RejectsExcessiveOrEmptyInsights(t *testing.T) {
	cases := []struct {
		name     string
		response string
		wantErr  string
	}{
		{"trop d'insights", `{"merges": [], "forget": [], "insights": ["un", "deux", "trois"]}`, "insights proposés"},
		{"insight vide", `{"merges": [], "forget": [], "insights": ["  "]}`, "insight sans contenu"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.seed("a", "Fait un", "home", "personal", "alice", "alice")
			store.seed("b", "Fait deux", "home", "personal", "alice", "alice")

			client := &scriptedClient{response: tc.response}
			consolidator := newConsolidator(t, store, client, 2)

			err := consolidator.Consolidate(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("erreur = %v, attendu une erreur contenant %q", err, tc.wantErr)
			}
			if len(store.remembered) != 0 {
				t.Errorf("un plan invalide ne doit rien écrire (écrits=%d)", len(store.remembered))
			}
		})
	}
}
