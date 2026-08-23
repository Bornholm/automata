package memory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
)

func recordTestEpisode(t *testing.T, s *memory.AmoxtliStore, content string, scope model.Scope, scopeID model.ScopeID) memory.Episode {
	t.Helper()

	ep, err := s.RecordEpisode(context.Background(), memory.NewEpisode{
		Content:        content,
		OrgID:          "home",
		Scope:          scope,
		ScopeID:        scopeID,
		ConversationID: "conv-1",
		From:           time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
		To:             time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RecordEpisode: %v", err)
	}
	return ep
}

func TestRecordAndSearchEpisode(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	recordTestEpisode(t, s,
		"[2026-08-01 09:00] Alice: on part sur un déploiement dokku pour le projet\n[2026-08-01 09:02] assistant: noté, dokku retenu",
		model.ScopeGroup, "grp-1")

	episodes, err := s.SearchEpisodes(ctx, memory.EpisodeQuery{
		Text:  "déploiement dokku",
		OrgID: "home",
		Scope: model.ScopeGroup, ScopeID: "grp-1",
	})
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("épisodes trouvés = %d, attendu 1", len(episodes))
	}

	ep := episodes[0]
	if !strings.Contains(ep.Content, "dokku") {
		t.Errorf("contenu inattendu: %q", ep.Content)
	}
	if ep.ConversationID != "conv-1" {
		t.Errorf("conversation_id = %q", ep.ConversationID)
	}
	if ep.From.IsZero() || ep.To.IsZero() {
		t.Errorf("bornes temporelles perdues: from=%v to=%v", ep.From, ep.To)
	}
	if !ep.To.After(ep.From) {
		t.Errorf("bornes incohérentes: from=%v to=%v", ep.From, ep.To)
	}
}

// Le cloisonnement par portée est le même contrat que pour les souvenirs :
// un épisode de groupe ne doit jamais remonter dans la recherche d'un autre
// groupe ni d'une conversation personnelle.
func TestSearchEpisodes_ScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	recordTestEpisode(t, s, "discussion sur le budget du groupe un", model.ScopeGroup, "grp-1")

	episodes, err := s.SearchEpisodes(ctx, memory.EpisodeQuery{
		Text:  "budget",
		OrgID: "home",
		Scope: model.ScopeGroup, ScopeID: "grp-2",
	})
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Fatalf("fuite inter-groupes: %d épisode(s) trouvé(s)", len(episodes))
	}

	episodes, err = s.SearchEpisodes(ctx, memory.EpisodeQuery{
		Text:  "budget",
		OrgID: "home",
		Scope: model.ScopePersonal, ScopeID: "alice",
	})
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(episodes) != 0 {
		t.Fatalf("fuite de portée: %d épisode(s) trouvé(s)", len(episodes))
	}
}

// Épisodes et souvenirs partagent le même codex : chaque famille de
// recherche doit rester aveugle à l'autre. Un épisode qui remonterait dans
// search_memory serait présenté comme un fait ; un souvenir dans la
// recherche d'historique se ferait passer pour du verbatim.
func TestEpisodesAndMemoriesDoNotCrossContaminate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Remember(ctx, memory.NewMemory{
		Content:          "Alice préfère le café colombien",
		Scope:            model.ScopePersonal,
		ScopeID:          "alice",
		OrgID:            "home",
		OwnerPrincipalID: "alice",
		CreatedBy:        "alice",
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	recordTestEpisode(t, s, "[2026-08-01 09:00] Alice: je préfère le café colombien au robusta", model.ScopePersonal, "alice")

	memories, err := s.Search(ctx, memory.Query{
		Text:  "café colombien",
		OrgID: "home",
		Scope: model.ScopePersonal, ScopeID: "alice",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("souvenirs trouvés = %d, attendu 1 (l'épisode ne doit pas remonter)", len(memories))
	}

	episodes, err := s.SearchEpisodes(ctx, memory.EpisodeQuery{
		Text:  "café colombien",
		OrgID: "home",
		Scope: model.ScopePersonal, ScopeID: "alice",
	})
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("épisodes trouvés = %d, attendu 1 (le souvenir ne doit pas remonter)", len(episodes))
	}
}

// La consolidation périodique passe par Store.List : elle ne doit JAMAIS
// voir les épisodes, sans quoi elle fusionnerait ou réécrirait du verbatim.
func TestList_NeverReturnsEpisodes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Remember(ctx, memory.NewMemory{
		Content: "un souvenir ordinaire",
		Scope:   model.ScopePersonal, ScopeID: "alice",
		OrgID:            "home",
		OwnerPrincipalID: "alice",
		CreatedBy:        "alice",
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	recordTestEpisode(t, s, "un fragment de conversation", model.ScopePersonal, "alice")

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("List = %d entrée(s), attendu 1 : les épisodes ne doivent pas apparaître", len(all))
	}
	if all[0].Content != "un souvenir ordinaire" {
		t.Errorf("List a retourné autre chose que le souvenir: %q", all[0].Content)
	}
}

func TestListEpisodes_ReturnsScopesAndNeverMemories(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Remember(ctx, memory.NewMemory{
		Content: "un souvenir ordinaire",
		Scope:   model.ScopePersonal, ScopeID: "alice",
		OrgID:            "home",
		OwnerPrincipalID: "alice",
		CreatedBy:        "alice",
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	recordTestEpisode(t, s, "premier fragment", model.ScopePersonal, "alice")
	recordTestEpisode(t, s, "second fragment", model.ScopeGroup, "grp-1")

	episodes, err := s.ListEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(episodes) != 2 {
		t.Fatalf("ListEpisodes = %d entrée(s), attendu 2 : jamais de souvenir sémantique", len(episodes))
	}

	byContent := map[string]memory.Episode{}
	for _, ep := range episodes {
		byContent[ep.Content] = ep
		if ep.OrgID != "home" {
			t.Errorf("org_id = %q, attendu home", ep.OrgID)
		}
		if ep.CreatedAt.IsZero() {
			t.Errorf("created_at absent pour %q", ep.ID)
		}
		if ep.ConversationID != "conv-1" {
			t.Errorf("conversation_id = %q", ep.ConversationID)
		}
	}

	first, ok := byContent["premier fragment"]
	if !ok || first.Scope != model.ScopePersonal || first.ScopeID != "alice" {
		t.Errorf("portée du premier fragment perdue: %+v", first)
	}
	second, ok := byContent["second fragment"]
	if !ok || second.Scope != model.ScopeGroup || second.ScopeID != "grp-1" {
		t.Errorf("portée du second fragment perdue: %+v", second)
	}
}

func TestForgetEpisode_RemovesFromListAndSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	kept := recordTestEpisode(t, s, "fragment conservé sur dokku", model.ScopePersonal, "alice")
	doomed := recordTestEpisode(t, s, "fragment purgé sur dokku", model.ScopePersonal, "alice")

	if err := s.ForgetEpisode(ctx, doomed.ID); err != nil {
		t.Fatalf("ForgetEpisode: %v", err)
	}

	episodes, err := s.ListEpisodes(ctx)
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if len(episodes) != 1 || episodes[0].ID != kept.ID {
		t.Fatalf("ListEpisodes après purge = %+v, attendu uniquement %q", episodes, kept.ID)
	}

	found, err := s.SearchEpisodes(ctx, memory.EpisodeQuery{
		Text:  "dokku",
		OrgID: "home",
		Scope: model.ScopePersonal, ScopeID: "alice",
	})
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	for _, ep := range found {
		if ep.ID == doomed.ID {
			t.Errorf("l'épisode purgé %q apparaît encore dans la recherche", doomed.ID)
		}
	}
}
