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

// Le semis reprend les clients du YAML une seule fois : un second appel ne
// doit rien réécrire, sinon une modification faite dans l'administration
// serait perdue à chaque redémarrage.
func TestSeedIsAppliedOnlyOnce(t *testing.T) {
	db := testDB(t)
	box := testBox(t)
	ctx := context.Background()

	cfg := &config.Config{
		LLMClients: map[string]config.LLMClient{
			"main": {Provider: "openrouter", Model: "deepseek/deepseek-chat", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1"},
		},
		ImageClients: map[string]config.ImageClient{
			"imagine": {Provider: "openai", Model: "gpt-image-1", APIKey: "sk-image"},
		},
	}

	if err := llmclients.Seed(ctx, db, box, cfg, nil); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	store := llmclients.NewStore(db, box)

	// Modification faite « depuis l'administration ».
	seedClient(t, db, store, "main", config.LLMClient{Provider: "openai", Model: "gpt-5", APIKey: "sk-autre"})

	if err := llmclients.Seed(ctx, db, box, cfg, nil); err != nil {
		t.Fatalf("Seed (2): %v", err)
	}

	row, found, err := store.Get(ctx, "main")
	if err != nil || !found {
		t.Fatalf("Get: %v (trouvé: %t)", err, found)
	}
	if row.Model != "gpt-5" {
		t.Errorf("modèle %q, attendu gpt-5 : le semis a écrasé une modification", row.Model)
	}

	// Le client d'images a bien été semé, dans sa propre famille.
	image, found, err := store.Get(ctx, "imagine")
	if err != nil || !found {
		t.Fatalf("Get(imagine): %v (trouvé: %t)", err, found)
	}
	if image.Kind != persistence.LLMClientKindImage {
		t.Errorf("famille %q, attendue %q", image.Kind, persistence.LLMClientKindImage)
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

// La surcharge d'une organisation prime sur le défaut de l'instance ; une
// organisation sans choix, ou inconnue, retombe sur ce défaut.
func TestResolverPrefersOrganizationChoice(t *testing.T) {
	db := testDB(t)
	box := testBox(t)
	store := llmclients.NewStore(db, box)
	ctx := context.Background()

	seedClient(t, db, store, "instance", config.LLMClient{Provider: "openai", Model: "gpt-5", APIKey: "sk-a"})
	seedClient(t, db, store, "premium", config.LLMClient{Provider: "openai", Model: "gpt-5-pro", APIKey: "sk-b"})

	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		return persistence.NewOrgAgentClientRepository().Set(ctx, tx, persistence.OrgAgentClient{
			OrgID: "atelier", Role: "main", ClientName: "premium", UpdatedAt: time.Now(),
		})
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	calls := 0
	pool := llmclients.NewPool(store, countingBuilder(&calls), nil)
	resolver := llmclients.NewResolver(pool, store, map[string]string{"main": "instance"}, nil)

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

	// Un rôle inconnu n'a pas de défaut : l'appelant garde son client.
	if _, err := resolver.ResolveClient(ctx, "inexistant", model.OrgID("atelier")); !errors.Is(err, llmclients.ErrUnknownClient) {
		t.Errorf("erreur %v, attendue ErrUnknownClient", err)
	}
}

// DefaultRoles dresse la table des rôles depuis la configuration — les
// clients de la mémoire en sont volontairement absents.
func TestDefaultRolesExcludesMemoryClients(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.Agent{
			"main":     {Client: "principal"},
			"research": {Client: "rapide"},
		},
		Plugins: config.Plugins{Client: "atelier", VisionClient: "regard"},
		Conversation: config.Conversation{
			Compaction: config.Compaction{Client: "resume"},
		},
		Memory: config.Memory{
			Consolidation: config.MemoryConsolidation{Client: "consolidation"},
			Retrieval:     config.MemoryRetrieval{Client: "hyde"},
		},
	}

	roles := llmclients.DefaultRoles(cfg)

	expected := map[string]string{
		"main":                       "principal",
		"research":                   "rapide",
		llmclients.RolePlugins:       "atelier",
		llmclients.RolePluginsVision: "regard",
		llmclients.RoleCompaction:    "resume",
	}
	for role, want := range expected {
		if roles[role] != want {
			t.Errorf("rôle %q : client %q, attendu %q", role, roles[role], want)
		}
	}
	if len(roles) != len(expected) {
		t.Errorf("%d rôles, attendus %d : les clients de la mémoire ne doivent pas y figurer (%v)",
			len(roles), len(expected), roles)
	}
}
