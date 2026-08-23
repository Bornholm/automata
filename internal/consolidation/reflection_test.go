package consolidation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
)

// fakeEpisodes implémente memory.EpisodeMaintenance en mémoire.
type fakeEpisodes struct {
	mu        sync.Mutex
	episodes  map[string]memory.Episode
	forgotten []string
}

func newFakeEpisodes() *fakeEpisodes {
	return &fakeEpisodes{episodes: map[string]memory.Episode{}}
}

func (s *fakeEpisodes) seed(id, content string, orgID, scope, scopeID string, createdAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.episodes[id] = memory.Episode{
		ID:        id,
		Content:   content,
		OrgID:     model.OrgID(orgID),
		Scope:     model.Scope(scope),
		ScopeID:   model.ScopeID(scopeID),
		From:      createdAt.Add(-time.Hour),
		To:        createdAt,
		CreatedAt: createdAt,
	}
}

func (s *fakeEpisodes) ListEpisodes(ctx context.Context) ([]memory.Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var all []memory.Episode
	for _, ep := range s.episodes {
		all = append(all, ep)
	}
	return all, nil
}

func (s *fakeEpisodes) ForgetEpisode(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.episodes, id)
	s.forgotten = append(s.forgotten, id)
	return nil
}

var _ memory.EpisodeMaintenance = &fakeEpisodes{}

func newReflector(t *testing.T, store memory.Store, episodes memory.EpisodeMaintenance, client *scriptedClient, cfg config.MemoryReflection) *Consolidator {
	t.Helper()
	return newConsolidator(t, store, client, 2).WithEpisodes(episodes, cfg)
}

func TestReflect_WritesPatternMemories(t *testing.T) {
	store := newFakeStore()
	episodes := newFakeEpisodes()
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		episodes.seed(fmt.Sprintf("e%d", i), "Alice: peux-tu faire plus court ?", "home", "personal", "alice", base.Add(time.Duration(i)*24*time.Hour))
	}

	client := &scriptedClient{response: `["Alice semble préférer des réponses courtes"]`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5})

	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	if len(store.remembered) != 1 {
		t.Fatalf("souvenirs écrits = %d, attendu 1", len(store.remembered))
	}

	pattern := store.remembered[0]
	if pattern.Content != "Alice semble préférer des réponses courtes" {
		t.Errorf("contenu = %q", pattern.Content)
	}
	if pattern.Origin != "episode_reflection" {
		t.Errorf("origin = %q, attendu \"episode_reflection\"", pattern.Origin)
	}
	if pattern.Scope != model.ScopePersonal || pattern.ScopeID != model.ScopeID("alice") || pattern.OrgID != model.OrgID("home") {
		t.Errorf("portée = %s/%s/%s, attendu celle des épisodes", pattern.OrgID, pattern.Scope, pattern.ScopeID)
	}
	if pattern.CreatedBy != consolidatorPrincipal {
		t.Errorf("created_by = %q, attendu %q", pattern.CreatedBy, consolidatorPrincipal)
	}
	if pattern.OwnerPrincipalID != "" {
		t.Errorf("owner = %q, attendu vide (le motif appartient à la portée)", pattern.OwnerPrincipalID)
	}
	if len(episodes.forgotten) != 0 {
		t.Errorf("épisodes supprimés = %v, attendu aucun (lecture seule sans rétention)", episodes.forgotten)
	}

	// L'ancrage a avancé : une seconde passe sans épisode nouveau ne
	// rappelle pas le LLM.
	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect (2e passe): %v", err)
	}
	if client.calls != 1 {
		t.Errorf("appels llm = %d, attendu 1 : la fenêtre était vide", client.calls)
	}
}

func TestReflect_BelowThresholdDoesNothing(t *testing.T) {
	store := newFakeStore()
	episodes := newFakeEpisodes()
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for i := range 3 {
		episodes.seed(fmt.Sprintf("e%d", i), "fragment", "home", "personal", "alice", base.Add(time.Duration(i)*time.Hour))
	}

	client := &scriptedClient{response: `["un motif"]`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5})

	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	if client.calls != 0 {
		t.Errorf("appels llm = %d, attendu 0 sous le seuil", client.calls)
	}
	if len(store.remembered) != 0 {
		t.Errorf("souvenirs écrits = %d, attendu 0", len(store.remembered))
	}
}

func TestReflect_IgnoresOrgScope(t *testing.T) {
	store := newFakeStore()
	episodes := newFakeEpisodes()
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		episodes.seed(fmt.Sprintf("e%d", i), "fragment", "home", "org", "", base.Add(time.Duration(i)*time.Hour))
	}

	client := &scriptedClient{response: `["un motif"]`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 2})

	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	if client.calls != 0 {
		t.Errorf("appels llm = %d, attendu 0 : la portée org n'est jamais réfléchie", client.calls)
	}
}

func TestReflect_RejectsExcessiveOrEmptyPatterns(t *testing.T) {
	cases := []struct {
		name     string
		response string
		wantErr  string
	}{
		{"trop de motifs", `["un", "deux", "trois"]`, "motifs proposés"},
		{"motif vide", `["  "]`, "motif sans contenu"},
		{"réponse illisible", `pas du json`, "illisible"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			episodes := newFakeEpisodes()
			base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
			for i := range 5 {
				episodes.seed(fmt.Sprintf("e%d", i), "fragment", "home", "personal", "alice", base.Add(time.Duration(i)*time.Hour))
			}

			client := &scriptedClient{response: tc.response}
			c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5})

			err := c.Reflect(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("erreur = %v, attendu une erreur contenant %q", err, tc.wantErr)
			}
			if len(store.remembered) != 0 {
				t.Errorf("une réponse invalide ne doit rien écrire (écrits=%d)", len(store.remembered))
			}

			// L'ancrage n'a pas avancé : la portée sera retentée à la
			// prochaine passe.
			client.mu.Lock()
			client.response = `[]`
			client.mu.Unlock()
			if err := c.Reflect(context.Background()); err != nil {
				t.Fatalf("Reflect (2e passe): %v", err)
			}
			if client.calls != 2 {
				t.Errorf("appels llm = %d, attendu 2 : la fenêtre en échec doit être retentée", client.calls)
			}
		})
	}
}

// La purge ne supprime que des épisodes à la fois couverts par une
// réflexion réussie ET plus vieux que la rétention.
func TestReflect_PurgeRespectsRetentionAndAnchor(t *testing.T) {
	store := newFakeStore()
	episodes := newFakeEpisodes()

	now := time.Date(2026, 8, 23, 4, 40, 0, 0, time.UTC)
	// Cinq épisodes anciens (au-delà de la rétention de 30 jours) et un
	// récent.
	for i := range 5 {
		episodes.seed(fmt.Sprintf("old%d", i), "fragment ancien", "home", "personal", "alice", now.Add(-40*24*time.Hour).Add(time.Duration(i)*time.Hour))
	}
	episodes.seed("recent", "fragment récent", "home", "personal", "alice", now.Add(-24*time.Hour))

	client := &scriptedClient{response: `[]`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5, RetentionDays: 30})
	c.WithClock(func() time.Time { return now })

	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	// Les six épisodes ont été réfléchis (fenêtre complète), l'ancrage
	// couvre tout : seuls les cinq plus vieux que la rétention sont purgés.
	if len(episodes.forgotten) != 5 {
		t.Fatalf("épisodes purgés = %v, attendu les 5 anciens", episodes.forgotten)
	}
	for _, id := range episodes.forgotten {
		if !strings.HasPrefix(id, "old") {
			t.Errorf("purge inattendue de %q", id)
		}
	}
}

// Sans réflexion réussie, la rétention ne purge jamais rien : un épisode
// doit avoir été consolidé avant d'être oublié.
func TestReflect_NeverPurgesUnreflectedEpisodes(t *testing.T) {
	store := newFakeStore()
	episodes := newFakeEpisodes()

	now := time.Date(2026, 8, 23, 4, 40, 0, 0, time.UTC)
	// Trois épisodes très anciens, mais sous le seuil de réflexion.
	for i := range 3 {
		episodes.seed(fmt.Sprintf("old%d", i), "fragment ancien", "home", "personal", "alice", now.Add(-100*24*time.Hour).Add(time.Duration(i)*time.Hour))
	}

	client := &scriptedClient{response: `[]`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5, RetentionDays: 30})
	c.WithClock(func() time.Time { return now })

	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	if client.calls != 0 {
		t.Errorf("appels llm = %d, attendu 0 sous le seuil", client.calls)
	}
	if len(episodes.forgotten) != 0 {
		t.Errorf("épisodes purgés = %v, attendu aucun : jamais de purge sans réflexion réussie", episodes.forgotten)
	}
}

// Au-delà de la borne d'épisodes par passe, l'ancrage n'avance que jusqu'au
// dernier épisode soumis : le retard se résorbe passe après passe.
func TestReflect_BacklogIsDrainedAcrossPasses(t *testing.T) {
	store := newFakeStore()
	episodes := newFakeEpisodes()
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	for i := range maxReflectionEpisodes + 5 {
		episodes.seed(fmt.Sprintf("e%02d", i), "fragment", "home", "personal", "alice", base.Add(time.Duration(i)*time.Hour))
	}

	client := &scriptedClient{response: `[]`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5})

	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect (1re passe): %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("appels llm = %d, attendu 1", client.calls)
	}

	// Seconde passe : les 5 épisodes restants forment la fenêtre.
	if err := c.Reflect(context.Background()); err != nil {
		t.Fatalf("Reflect (2e passe): %v", err)
	}
	if client.calls != 2 {
		t.Errorf("appels llm = %d, attendu 2 : le reliquat doit être réfléchi", client.calls)
	}
}

// La réflexion est déclenchée par le même tick cron que la consolidation.
func TestTick_RunsReflectionAfterConsolidation(t *testing.T) {
	store := newFakeStore()
	for i := range 3 {
		store.seed(fmt.Sprintf("m%d", i), "Fait", "home", "personal", "alice", "alice")
	}
	episodes := newFakeEpisodes()
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		episodes.seed(fmt.Sprintf("e%d", i), "fragment", "home", "personal", "alice", base.Add(time.Duration(i)*time.Hour))
	}

	client := &scriptedClient{response: `{"merges": [], "forget": [], "insights": []}`}
	c := newReflector(t, store, episodes, client, config.MemoryReflection{Enabled: true, MinEpisodes: 5})

	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.Local)
	c.WithClock(func() time.Time { return now })

	// Ancrage initial, puis passage de l'échéance.
	if err := c.Tick(context.Background()); err != nil {
		t.Fatalf("Tick initial: %v", err)
	}
	now = now.Add(26 * time.Hour)
	if err := c.Tick(context.Background()); err != nil {
		t.Fatalf("Tick après échéance: %v", err)
	}

	// Deux appels : la consolidation de la portée, puis sa réflexion. La
	// réponse JSON objet est illisible comme tableau de motifs : l'échec de
	// la réflexion ne doit pas faire échouer le tick.
	if client.calls != 2 {
		t.Errorf("appels llm = %d, attendu 2 (consolidation puis réflexion)", client.calls)
	}
}
