package memory_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli"
	amoxtlibleve "github.com/bornholm/amoxtli/index/bleve"
	amoxtligorm "github.com/bornholm/amoxtli/ingest/gorm"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
)

// newTestStore construit un memory.AmoxtliStore adossé à un Amoxtli réel
// (store sqlite + index bleve, sur des répertoires temporaires), conforme à
// PLAN.md §16 ("Tests d'intégration : ... Amoxtli réel", pas de mock du
// Codex).
func newTestStore(t *testing.T) *memory.AmoxtliStore {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	store, err := amoxtligorm.NewSQLiteStore(filepath.Join(dir, "amoxtli.sqlite"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	idx, err := amoxtlibleve.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("bleve.OpenOrCreate: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: idx, Weight: 1}),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("amoxtli.New: %v", err)
	}
	t.Cleanup(func() { _ = codex.Close() })

	s, err := memory.NewAmoxtliStore(ctx, codex, "test-memory")
	if err != nil {
		t.Fatalf("NewAmoxtliStore: %v", err)
	}

	return s
}

func TestRememberAndSearchPersonal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mem, err := s.Remember(ctx, memory.NewMemory{
		Content:              "Alice adore le café au lait le matin.",
		Scope:                model.ScopePersonal,
		ScopeID:              "alice",
		OrgID:                "home",
		OwnerPrincipalID:     "alice",
		CreatedBy:            "alice",
		SourceConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if mem.ID == "" {
		t.Fatalf("Remember: identifiant vide")
	}

	results, err := s.Search(ctx, memory.Query{
		Text:    "café au lait",
		OrgID:   "home",
		Scope:   model.ScopePersonal,
		ScopeID: "alice",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search: %d résultat(s), voulu 1: %+v", len(results), results)
	}
	if results[0].ID != mem.ID {
		t.Errorf("ID = %q, voulu %q", results[0].ID, mem.ID)
	}
	if !strings.Contains(results[0].Content, "café au lait") {
		t.Errorf("Content = %q, ne contient pas le texte attendu", results[0].Content)
	}
}

func TestRememberAndSearchGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mem, err := s.Remember(ctx, memory.NewMemory{
		Content:              "La réunion de famille a lieu samedi.",
		Scope:                model.ScopeGroup,
		ScopeID:              "main-group",
		OrgID:                "home",
		OwnerPrincipalID:     "alice",
		CreatedBy:            "alice",
		SourceConversationID: "conv-group",
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	results, err := s.Search(ctx, memory.Query{
		Text:    "réunion de famille",
		OrgID:   "home",
		Scope:   model.ScopeGroup,
		ScopeID: "main-group",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ID != mem.ID {
		t.Fatalf("Search: résultats inattendus: %+v", results)
	}
}

func TestSearchCloisonnement(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Mémoire de groupe A.
	if _, err := s.Remember(ctx, memory.NewMemory{
		Content: "Secret du groupe A: le mot de passe wifi est licorne42.",
		Scope:   model.ScopeGroup, ScopeID: "group-a", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice", SourceConversationID: "conv-a",
	}); err != nil {
		t.Fatalf("Remember (groupe A): %v", err)
	}

	// Mémoire personnelle d'Alice.
	if _, err := s.Remember(ctx, memory.NewMemory{
		Content: "Secret personnel d'Alice: elle déteste les épinards.",
		Scope:   model.ScopePersonal, ScopeID: "alice", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice", SourceConversationID: "conv-p",
	}); err != nil {
		t.Fatalf("Remember (personnel): %v", err)
	}

	// La mémoire du groupe A ne doit apparaître ni dans le groupe B...
	resultsB, err := s.Search(ctx, memory.Query{Text: "Secret", OrgID: "home", Scope: model.ScopeGroup, ScopeID: "group-b"})
	if err != nil {
		t.Fatalf("Search (groupe B): %v", err)
	}
	if len(resultsB) != 0 {
		t.Errorf("Search (groupe B): fuite détectée: %+v", resultsB)
	}

	// ... ni dans une recherche personnelle d'un autre principal.
	resultsBob, err := s.Search(ctx, memory.Query{Text: "Secret", OrgID: "home", Scope: model.ScopePersonal, ScopeID: "bob"})
	if err != nil {
		t.Fatalf("Search (personnel bob): %v", err)
	}
	if len(resultsBob) != 0 {
		t.Errorf("Search (personnel bob): fuite détectée: %+v", resultsBob)
	}

	// La recherche dans la bonne portée retrouve bien chaque mémoire.
	resultsA, err := s.Search(ctx, memory.Query{Text: "Secret", OrgID: "home", Scope: model.ScopeGroup, ScopeID: "group-a"})
	if err != nil {
		t.Fatalf("Search (groupe A): %v", err)
	}
	if len(resultsA) != 1 {
		t.Fatalf("Search (groupe A): %d résultat(s), voulu 1: %+v", len(resultsA), resultsA)
	}

	resultsAlice, err := s.Search(ctx, memory.Query{Text: "Secret", OrgID: "home", Scope: model.ScopePersonal, ScopeID: "alice"})
	if err != nil {
		t.Fatalf("Search (personnel alice): %v", err)
	}
	if len(resultsAlice) != 1 {
		t.Fatalf("Search (personnel alice): %d résultat(s), voulu 1: %+v", len(resultsAlice), resultsAlice)
	}
}

func TestForget(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mem, err := s.Remember(ctx, memory.NewMemory{
		Content: "Mémoire temporaire à oublier.",
		Scope:   model.ScopePersonal, ScopeID: "alice", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice", SourceConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	if err := s.Forget(ctx, mem.ID); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	results, err := s.Search(ctx, memory.Query{Text: "temporaire à oublier", OrgID: "home", Scope: model.ScopePersonal, ScopeID: "alice"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("Search après Forget: mémoire toujours présente: %+v", results)
	}
}

func TestGetByID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mem, err := s.Remember(ctx, memory.NewMemory{
		Content: "Contenu identifiable précisément.",
		Scope:   model.ScopePersonal, ScopeID: "alice", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice", SourceConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	found, ok, err := s.GetByID(ctx, "home", model.ScopePersonal, "alice", mem.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !ok {
		t.Fatalf("GetByID: mémoire non trouvée")
	}
	if found.Content != mem.Content {
		t.Errorf("Content = %q, voulu %q", found.Content, mem.Content)
	}

	// La même mémoire n'existe pas dans une autre portée : ce n'est pas une
	// erreur, seulement ok=false (PLAN.md §8.3).
	_, ok, err = s.GetByID(ctx, "home", model.ScopeGroup, "main-group", mem.ID)
	if err != nil {
		t.Fatalf("GetByID (mauvaise portée): %v", err)
	}
	if ok {
		t.Errorf("GetByID (mauvaise portée): trouvée à tort")
	}
}

func TestReindex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Remember(ctx, memory.NewMemory{
		Content: "Donnée de test pour la réindexation.",
		Scope:   model.ScopePersonal, ScopeID: "alice", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice", SourceConversationID: "conv-1",
	}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	if err := s.Reindex(ctx); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	results, err := s.Search(ctx, memory.Query{Text: "réindexation", OrgID: "home", Scope: model.ScopePersonal, ScopeID: "alice"})
	if err != nil {
		t.Fatalf("Search après Reindex: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search après Reindex: %d résultat(s), voulu 1: %+v", len(results), results)
	}
}

// TestRemember_IndexingFailureNoPhantomMemory simule une panne d'indexation
// (index inaccessible, ici fermé délibérément) et vérifie que Remember
// retourne une erreur claire, sans laisser de mémoire fantôme retrouvable :
// amoxtli effectue lui-même le rollback de la saga d'ingestion (voir
// docs/integration-inventory.md §3, "Garanties transactionnelles"), ce que
// ce test vérifie en rouvrant un index neuf sur le même store et en
// constatant l'absence de tout résultat.
func TestRemember_IndexingFailureNoPhantomMemory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.bleve")

	store, err := amoxtligorm.NewSQLiteStore(filepath.Join(dir, "amoxtli.sqlite"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	idx, err := amoxtlibleve.OpenOrCreate(ctx, indexPath)
	if err != nil {
		t.Fatalf("bleve.OpenOrCreate: %v", err)
	}

	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: idx, Weight: 1}),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("amoxtli.New: %v", err)
	}

	s, err := memory.NewAmoxtliStore(ctx, codex, "fail-test")
	if err != nil {
		t.Fatalf("NewAmoxtliStore: %v", err)
	}

	// Simule un index inaccessible : les écritures suivantes échouent.
	if err := idx.Close(); err != nil {
		t.Fatalf("idx.Close: %v", err)
	}

	_, err = s.Remember(ctx, memory.NewMemory{
		Content: "Cette mémoire ne doit jamais être retrouvable.",
		Scope:   model.ScopePersonal, ScopeID: "alice", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice", SourceConversationID: "conv-1",
	})
	if err == nil {
		t.Fatalf("Remember: erreur attendue après fermeture de l'index")
	}

	if err := codex.Close(); err != nil {
		t.Fatalf("codex.Close: %v", err)
	}

	// Rouvre un index neuf sur le même chemin, adossé au même store, pour
	// vérifier qu'aucune mémoire fantôme n'a survécu au rollback.
	freshIdx, err := amoxtlibleve.OpenOrCreate(ctx, indexPath)
	if err != nil {
		t.Fatalf("bleve.OpenOrCreate (fresh): %v", err)
	}
	t.Cleanup(func() { _ = freshIdx.Close() })

	freshCodex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: freshIdx, Weight: 1}),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("amoxtli.New (fresh): %v", err)
	}
	t.Cleanup(func() { _ = freshCodex.Close() })

	freshStore, err := memory.NewAmoxtliStore(ctx, freshCodex, "fail-test-fresh")
	if err != nil {
		t.Fatalf("NewAmoxtliStore (fresh): %v", err)
	}

	results, err := freshStore.Search(ctx, memory.Query{Text: "jamais être retrouvable", OrgID: "home", Scope: model.ScopePersonal, ScopeID: "alice"})
	if err != nil {
		t.Fatalf("Search (fresh): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("mémoire fantôme trouvée après échec d'indexation: %+v", results)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seeds := []memory.NewMemory{
		{Content: "Alice adore le café au lait.", Scope: model.ScopePersonal, ScopeID: "alice", OrgID: "home", OwnerPrincipalID: "alice", CreatedBy: "alice", Origin: "compaction"},
		{Content: "La famille dîne ensemble le dimanche.", Scope: model.ScopeGroup, ScopeID: "famille", OrgID: "home", OwnerPrincipalID: "bob", CreatedBy: "bob"},
	}

	ids := map[string]memory.NewMemory{}
	for _, seed := range seeds {
		mem, err := s.Remember(ctx, seed)
		if err != nil {
			t.Fatalf("Remember: %v", err)
		}
		ids[mem.ID] = seed
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != len(seeds) {
		t.Fatalf("List: %d mémoires, attendu %d", len(all), len(seeds))
	}

	for _, mem := range all {
		seed, ok := ids[mem.ID]
		if !ok {
			t.Errorf("List: identifiant inattendu %q", mem.ID)
			continue
		}
		if mem.Content != seed.Content {
			t.Errorf("List: contenu %q, attendu %q (le préfixe d'identifiant doit être retiré)", mem.Content, seed.Content)
		}
		if mem.Metadata["scope"] != string(seed.Scope) || mem.Metadata["scope_id"] != string(seed.ScopeID) || mem.Metadata["org_id"] != string(seed.OrgID) {
			t.Errorf("List: métadonnées de portée %v, attendu %s/%s/%s", mem.Metadata, seed.OrgID, seed.Scope, seed.ScopeID)
		}
		if mem.Metadata["owner_principal_id"] != string(seed.OwnerPrincipalID) {
			t.Errorf("List: owner %q, attendu %q", mem.Metadata["owner_principal_id"], seed.OwnerPrincipalID)
		}
		if seed.Origin != "" && mem.Metadata["origin"] != seed.Origin {
			t.Errorf("List: origin %q, attendu %q", mem.Metadata["origin"], seed.Origin)
		}
		if mem.CreatedAt.IsZero() {
			t.Errorf("List: created_at absent pour %q", mem.ID)
		}
	}

	// Une mémoire oubliée disparaît de l'énumération.
	var firstID string
	for id := range ids {
		firstID = id
		break
	}
	if err := s.Forget(ctx, firstID); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	all, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List après Forget: %v", err)
	}
	if len(all) != len(seeds)-1 {
		t.Fatalf("List après Forget: %d mémoires, attendu %d", len(all), len(seeds)-1)
	}
}
