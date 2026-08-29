package llmclients_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
)

// testSecret : au moins 32 octets, comme l'exige secretbox.
const testSecret = "un-secret-de-session-suffisamment-long"

func testDB(t *testing.T) *persistence.DB {
	t.Helper()

	db, err := persistence.Open(context.Background(), config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
	})
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()

	box, err := secretbox.NewLLMClients(testSecret)
	if err != nil {
		t.Fatalf("secretbox.NewLLMClients: %v", err)
	}

	return box
}

// fakeClient est un client de modèle inerte : le pool n'a besoin que d'une
// valeur distincte à chaque construction pour qu'on puisse compter.
type fakeClient struct{ llm.Client }

// countingBuilder compte les constructions, pour distinguer une
// réutilisation du cache d'une reconstruction.
func countingBuilder(calls *int) llmclients.Builder {
	return func(context.Context, config.LLMClient) (llm.Client, error) {
		*calls++
		return fakeClient{}, nil
	}
}

func seedClient(t *testing.T, db *persistence.DB, store *llmclients.Store, name string, cfg config.LLMClient) {
	t.Helper()

	row, err := store.Row(name, persistence.LLMClientKindLLM, cfg, time.Now())
	if err != nil {
		t.Fatalf("Row: %v", err)
	}
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewLLMClientRepository().Upsert(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

// La clé d'API est scellée en base et ne se relit qu'à la construction du
// client : c'est le seul endroit où elle revient en clair.
func TestStoreSealsAPIKey(t *testing.T) {
	db := testDB(t)
	box := testBox(t)
	store := llmclients.NewStore(db, box)

	seedClient(t, db, store, "main", config.LLMClient{Provider: "openai", Model: "gpt-5", APIKey: "sk-secret"})

	row, found, err := store.Get(context.Background(), "main")
	if err != nil || !found {
		t.Fatalf("Get: %v (trouvé: %t)", err, found)
	}
	if row.APIKey == "sk-secret" {
		t.Fatal("la clé est stockée en clair")
	}

	cfg, err := store.Config(row)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.APIKey != "sk-secret" {
		t.Errorf("clé ouverte %q, attendue sk-secret", cfg.APIKey)
	}
}

// Le pool ne reconstruit un client que si sa définition a changé : c'est ce
// qui rend la modification immédiate sans redémarrer, et sans reconstruire
// à chaque message.
func TestPoolRebuildsOnlyWhenDefinitionChanges(t *testing.T) {
	db := testDB(t)
	box := testBox(t)
	store := llmclients.NewStore(db, box)
	ctx := context.Background()

	seedClient(t, db, store, "main", config.LLMClient{Provider: "openai", Model: "gpt-5", APIKey: "sk-test"})

	calls := 0
	pool := llmclients.NewPool(store, countingBuilder(&calls), nil)

	for i := range 3 {
		resolved, err := pool.Get(ctx, "main")
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if resolved.Model != "gpt-5" {
			t.Errorf("modèle %q, attendu gpt-5", resolved.Model)
		}
	}
	if calls != 1 {
		t.Errorf("%d constructions, attendue 1 (le pool doit réutiliser)", calls)
	}

	// Le modèle change dans l'administration.
	seedClient(t, db, store, "main", config.LLMClient{Provider: "openai", Model: "gpt-5-mini", APIKey: "sk-test"})

	resolved, err := pool.Get(ctx, "main")
	if err != nil {
		t.Fatalf("Get après modification: %v", err)
	}
	if resolved.Model != "gpt-5-mini" {
		t.Errorf("modèle %q, attendu gpt-5-mini : la modification n'a pas pris effet", resolved.Model)
	}
	if calls != 2 {
		t.Errorf("%d constructions, attendues 2 (une reconstruction après modification)", calls)
	}
}

// Un nom absent du catalogue est une erreur reconnaissable : l'appelant
// doit pouvoir se rabattre sur son client de démarrage.
func TestPoolReportsUnknownClient(t *testing.T) {
	db := testDB(t)
	box := testBox(t)

	calls := 0
	pool := llmclients.NewPool(llmclients.NewStore(db, box), countingBuilder(&calls), nil)

	if _, err := pool.Get(context.Background(), "fantome"); !errors.Is(err, llmclients.ErrUnknownClient) {
		t.Errorf("erreur %v, attendue ErrUnknownClient", err)
	}
}

// setRole pose un choix de modèle en base — org_id vide pour le défaut de
// l'instance.
func setRole(t *testing.T, db *persistence.DB, orgID, role, clientName string) {
	t.Helper()

	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewOrgAgentClientRepository().Set(context.Background(), tx, persistence.OrgAgentClient{
			OrgID: orgID, Role: role, ClientName: clientName, UpdatedAt: time.Now(),
		})
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

// La surcharge d'une organisation prime sur le défaut de l'instance
// (org_id vide) ; le YAML ne participe plus à la résolution, et un rôle
// sans défaut rend ErrNoDefault — jamais un repli silencieux.
func TestResolverPrefersOrganizationChoice(t *testing.T) {
	db := testDB(t)
	box := testBox(t)
	store := llmclients.NewStore(db, box)
	ctx := context.Background()

	seedClient(t, db, store, "instance", config.LLMClient{Provider: "openai", Model: "gpt-5", APIKey: "sk-a"})
	seedClient(t, db, store, "premium", config.LLMClient{Provider: "openai", Model: "gpt-5-pro", APIKey: "sk-b"})

	setRole(t, db, "", "main", "instance")
	setRole(t, db, "atelier", "main", "premium")

	calls := 0
	pool := llmclients.NewPool(store, countingBuilder(&calls), nil)
	resolver := llmclients.NewResolver(pool, store, nil)

	resolved, err := resolver.ResolveClient(ctx, "main", model.OrgID("atelier"))
	if err != nil {
		t.Fatalf("ResolveClient(atelier): %v", err)
	}
	if resolved.Name != "premium" {
		t.Errorf("client %q, attendu premium (choix de l'organisation)", resolved.Name)
	}

	resolved, err = resolver.ResolveClient(ctx, "main", model.OrgID("autre"))
	if err != nil {
		t.Fatalf("ResolveClient(autre): %v", err)
	}
	if resolved.Name != "instance" {
		t.Errorf("client %q, attendu instance (défaut, aucune surcharge)", resolved.Name)
	}

	// Un rôle sans défaut d'instance : erreur nommée, à montrer à qui peut
	// la corriger.
	if _, err := resolver.ResolveClient(ctx, "inexistant", model.OrgID("atelier")); !errors.Is(err, llmclients.ErrNoDefault) {
		t.Errorf("erreur %v, attendue ErrNoDefault", err)
	}
}

// Roles dresse la liste depuis ce que la configuration déclare : agents,
// génération d'images, fonctions actives, index sémantiques — jamais de
// noms de clients.
func TestRolesFollowConfiguredFeatures(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.Agent{
			"main":    {},
			"imagine": {ImageGeneration: true},
		},
		Plugins: config.Plugins{Enabled: true},
		Conversation: config.Conversation{
			Compaction: config.Compaction{Enabled: true},
		},
		Audio: config.Audio{Enabled: true},
		Memory: config.Memory{
			Consolidation: config.MemoryConsolidation{Enabled: true},
			Retrieval:     config.MemoryRetrieval{Profile: "balanced"},
			Indexes: []config.MemoryIndex{
				{ID: "semantic", Type: "sqlitevec"},
				{ID: "fulltext", Type: "bleve"},
			},
		},
	}

	got := llmclients.Roles(cfg)
	want := map[string]bool{
		"main": true, "imagine": true, "image:imagine": true,
		llmclients.RolePlugins: true, llmclients.RolePluginsVision: true,
		llmclients.RoleCompaction: true, llmclients.RoleTranscription: true,
		llmclients.RoleConsolidation: true, llmclients.RoleRetrieval: true,
		"embeddings:semantic": true,
	}
	if len(got) != len(want) {
		t.Fatalf("%d rôles (%v), attendus %d", len(got), got, len(want))
	}
	for _, role := range got {
		if !want[role] {
			t.Errorf("rôle inattendu %q", role)
		}
	}
}
