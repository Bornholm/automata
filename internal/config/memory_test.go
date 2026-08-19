package config

import "testing"

// Un index sémantique sans client d'embeddings ne peut pas fonctionner : la
// panne n'apparaîtrait qu'au démarrage du registre, loin de la configuration
// fautive. Elle doit être refusée au chargement, comme un llm_client
// incomplet.
func TestValidateMemory_SqlitevecRequiresEmbeddingsClient(t *testing.T) {
	cfg := &Config{
		Memory: Memory{
			Indexes: []MemoryIndex{
				{ID: "vector", Type: "sqlitevec", Path: "data/vector.sqlite"},
			},
		},
	}
	assertHasError(t, validateMemory(cfg), "memory.indexes[0].client: requis")

	cfg.Memory.Indexes[0].Client = "fantome"
	assertHasError(t, validateMemory(cfg), `client llm "fantome" introuvable`)

	cfg.LLMClients = map[string]LLMClient{
		"embeddings": {Provider: "mistral", Model: "mistral-embed", APIKey: "k", BaseURL: "https://api.mistral.test/v1"},
	}
	cfg.Memory.Indexes[0].Client = "embeddings"
	if errs := validateMemory(cfg); len(errs) != 0 {
		t.Fatalf("index sqlitevec complet refusé: %v", errs)
	}
}

func TestValidateMemory_UnknownIndexTypeIsRejected(t *testing.T) {
	cfg := &Config{
		Memory: Memory{
			Indexes: []MemoryIndex{{ID: "idx", Type: "pinecone", Path: "x"}},
		},
	}
	assertHasError(t, validateMemory(cfg), `memory.indexes[0].type: "pinecone" non supporté`)
}

// Le profil "balanced" déclenche un appel LLM (HyDE) à chaque requête
// distincte : sans client configuré, la recherche mémoire entière tomberait
// en panne au premier usage.
func TestValidateMemory_BalancedProfileRequiresClient(t *testing.T) {
	cfg := &Config{
		Memory: Memory{Retrieval: MemoryRetrieval{Profile: "balanced"}},
	}
	assertHasError(t, validateMemory(cfg), "memory.retrieval.client: requis")

	cfg.Memory.Retrieval.Client = "fantome"
	assertHasError(t, validateMemory(cfg), `client llm "fantome" introuvable`)

	cfg.LLMClients = map[string]LLMClient{
		"small": {Provider: "openai", Model: "m", APIKey: "k", BaseURL: "https://x.test/v1"},
	}
	cfg.Memory.Retrieval.Client = "small"
	if errs := validateMemory(cfg); len(errs) != 0 {
		t.Fatalf("profil balanced complet refusé: %v", errs)
	}

	cfg.Memory.Retrieval = MemoryRetrieval{Profile: "précis"}
	assertHasError(t, validateMemory(cfg), `memory.retrieval.profile: "précis" non supporté`)
}
