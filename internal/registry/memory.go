package registry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bornholm/amoxtli"
	amoxtlibleve "github.com/bornholm/amoxtli/index/bleve"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	amoxtligorm "github.com/bornholm/amoxtli/ingest/gorm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/observability"
)

// memoryCollectionLabel est le label de la collection amoxtli unique créée
// au démarrage (voir le commentaire de internal/memory.AmoxtliStore sur la
// gestion des collections : V1 mono-organisation, une seule collection
// suffit, elle n'est pas un périmètre de sécurité).
const memoryCollectionLabel = "automata-memory"

// memoryResources regroupe le store et l'agent.MemoryTools construits pour
// brancher la mémoire (PLAN.md §8, Phase 10), ainsi que les ressources à
// fermer proprement à l'arrêt (store SQLite et index bleve, possédés par
// l'appelant selon la convention amoxtli — voir README amoxtli "L'appelant
// possède les ressources qu'il crée et doit les fermer").
type memoryResources struct {
	store   *memory.AmoxtliStore
	closers []closerFunc
}

type closerFunc func() error

// close ferme les ressources dans l'ordre inverse de leur construction,
// journalise toute erreur de fermeture individuelle sans interrompre les
// suivantes (même politique que registry.Run pour la fermeture de la
// persistance applicative).
func (r *memoryResources) close(logger interface {
	Error(msg string, args ...any)
}) {
	for i := len(r.closers) - 1; i >= 0; i-- {
		if err := r.closers[i](); err != nil {
			logger.Error("registry: échec de la fermeture d'une ressource mémoire", "error", err)
		}
	}
}

// buildMemory construit la mémoire persistante Amoxtli décrite par
// cfg.Memory (store SQLite + index bleve), si cfg.Memory.Store.Path est
// renseigné. Si aucun store n'est configuré, buildMemory retourne
// (nil, memoryResources{}, nil) : la mémoire est alors indisponible, et tout
// agent déclarant memory.search/remember/forget se voit tout de même
// construit, mais sans aucun outil mémoire exposé (voir
// agent.NewRegistryWithMemory, qui gère nativement une MemoryTools à zéro).
func buildMemory(ctx context.Context, cfg *config.Config) (memoryResources, error) {
	if cfg.Memory.Store.Path == "" {
		return memoryResources{}, nil
	}

	if cfg.Memory.Store.Driver != "" && cfg.Memory.Store.Driver != "sqlite" {
		return memoryResources{}, fmt.Errorf("registry: mémoire: pilote de stockage %q non supporté (seul \"sqlite\" l'est en V1)", cfg.Memory.Store.Driver)
	}

	res := memoryResources{}

	if dir := filepath.Dir(cfg.Memory.Store.Path); dir != "" && dir != "." {
		// 0o700 : la mémoire persistante contient des données personnelles
		// (PLAN.md Phase 19, point 5, même raisonnement que
		// internal/persistence/db.go pour la base applicative).
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return res, fmt.Errorf("registry: mémoire: création du répertoire %q: %w", dir, err)
		}
	}

	// La même clé protège les conversations et les souvenirs : ce sont
	// les deux moitiés du même contenu personnel. L'index plein texte,
	// lui, reste en clair — chiffrer des termes indexés casserait la
	// recherche — et le répertoire de l'index doit être protégé par les
	// permissions du système.
	var storeOpts []amoxtligorm.StoreOptionFunc
	if cfg.Storage.EncryptionKey != "" {
		storeOpts = append(storeOpts, amoxtligorm.WithEncryptionKey(cfg.Storage.EncryptionKey))
	}

	store, err := amoxtligorm.NewSQLiteStore(cfg.Memory.Store.Path, storeOpts...)
	if err != nil {
		return res, fmt.Errorf("registry: mémoire: ouverture du store sqlite %q: %w", cfg.Memory.Store.Path, err)
	}
	res.closers = append(res.closers, store.Close)

	var indexers []amoxtli.Indexer
	for _, idxCfg := range cfg.Memory.Indexes {
		switch idxCfg.Type {
		case "bleve":
			if dir := filepath.Dir(idxCfg.Path); dir != "" && dir != "." {
				// 0o700 : la mémoire persistante contient des données
				// personnelles (PLAN.md Phase 19, point 5, même raisonnement
				// que internal/persistence/db.go pour la base applicative).
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return res, fmt.Errorf("registry: mémoire: création du répertoire %q: %w", dir, err)
				}
			}

			bleveIdx, err := amoxtlibleve.OpenOrCreate(ctx, idxCfg.Path)
			if err != nil {
				return res, fmt.Errorf("registry: mémoire: ouverture de l'index bleve %q (memory.indexes[%q]): %w", idxCfg.Path, idxCfg.ID, err)
			}
			res.closers = append(res.closers, bleveIdx.Close)

			weight := idxCfg.Weight
			if weight == 0 {
				weight = 1
			}

			indexers = append(indexers, amoxtli.Indexer{ID: idxCfg.ID, Index: bleveIdx, Weight: weight})
		case "sqlitevec":
			if dir := filepath.Dir(idxCfg.Path); dir != "" && dir != "." {
				// 0o700 : même raisonnement que pour l'index bleve.
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return res, fmt.Errorf("registry: mémoire: création du répertoire %q: %w", dir, err)
				}
			}

			// Le client est garanti présent par config.Validate ; son modèle
			// (ex: mistral-embed) est celui utilisé pour les embeddings.
			clientCfg := cfg.LLMClients[idxCfg.Client]
			embeddings, err := agent.BuildEmbeddingsClient(ctx, clientCfg)
			if err != nil {
				return res, fmt.Errorf("registry: mémoire: client d'embeddings de memory.indexes[%q]: %w", idxCfg.ID, err)
			}

			// Installe l'extension sqlite-vec (WASM) avant toute ouverture,
			// comme le fait le runtime amoxtli lui-même.
			sqlitevecIndex.EnsureVecWASM()

			vecIdx, err := sqlitevecIndex.NewIndexAtPath(idxCfg.Path, embeddings,
				sqlitevecIndex.WithEmbeddingsModel(clientCfg.Model),
			)
			if err != nil {
				return res, fmt.Errorf("registry: mémoire: ouverture de l'index sqlitevec %q (memory.indexes[%q]): %w", idxCfg.Path, idxCfg.ID, err)
			}
			res.closers = append(res.closers, vecIdx.Close)

			weight := idxCfg.Weight
			if weight == 0 {
				weight = 1
			}

			indexers = append(indexers, amoxtli.Indexer{ID: idxCfg.ID, Index: vecIdx, Weight: weight})
		default:
			return res, fmt.Errorf("registry: mémoire: type d'index %q (memory.indexes[%q]) non supporté (types: \"bleve\", \"sqlitevec\")", idxCfg.Type, idxCfg.ID)
		}
	}

	if len(indexers) == 0 {
		return res, fmt.Errorf("registry: mémoire: au moins un index (memory.indexes) est requis")
	}

	codexOpts := []amoxtli.Option{
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(indexers...),
	}

	// Profils de recherche, calqués sur ceux du runtime amoxtli : "fast"
	// (défaut) coupe HyDE et Judge — aucun appel LLM à la recherche ;
	// "balanced" garde HyDE (un appel de complétion par requête distincte)
	// et coupe le Judge.
	switch cfg.Memory.Retrieval.Profile {
	case "", "fast":
		codexOpts = append(codexOpts, amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge())
	case "balanced":
		// Le client est garanti présent par config.Validate.
		hydeClient, err := agent.BuildLLMClient(ctx, cfg.LLMClients[cfg.Memory.Retrieval.Client])
		if err != nil {
			return res, fmt.Errorf("registry: mémoire: client HyDE (memory.retrieval.client): %w", err)
		}
		codexOpts = append(codexOpts, amoxtli.WithDisableJudge(), amoxtli.WithLLMClient(hydeClient))
	default:
		return res, fmt.Errorf("registry: mémoire: profil de recherche %q non supporté (profils: \"fast\", \"balanced\")", cfg.Memory.Retrieval.Profile)
	}

	codex, err := amoxtli.New(ctx, codexOpts...)
	if err != nil {
		return res, fmt.Errorf("registry: mémoire: construction du codex amoxtli: %w", err)
	}
	res.closers = append(res.closers, codex.Close)

	amoxtliStore, err := memory.NewAmoxtliStore(ctx, codex, memoryCollectionLabel)
	if err != nil {
		return res, fmt.Errorf("registry: mémoire: %w", err)
	}

	res.store = amoxtliStore

	return res, nil
}

// MemoryReindex construit la mémoire décrite par cfg.Memory et déclenche une
// réindexation complète (Store.Reindex), en attendant sa fin. Utilisée par
// la commande CLI "automata memory reindex" (PLAN.md §8.6, Phase 10). Les
// ressources construites sont fermées avant le retour, quel que soit le
// résultat.
func MemoryReindex(ctx context.Context, logger interface {
	Error(msg string, args ...any)
}, cfg *config.Config) error {
	res, err := buildMemory(ctx, cfg)
	defer res.close(logger)
	if err != nil {
		return fmt.Errorf("registry: construction de la mémoire: %w", err)
	}

	if res.store == nil {
		return fmt.Errorf("registry: mémoire non configurée (memory.store.path vide)")
	}

	if err := res.store.Reindex(ctx); err != nil {
		return fmt.Errorf("registry: réindexation: %w", err)
	}

	return nil
}

// buildMemoryTools construit l'agent.MemoryTools partagé par tous les
// agents orchestrateurs (voir agent.NewRegistryWithMemory, qui recroise ces
// capacités globales avec les booléens propres à chaque agent). Si store est
// nil (mémoire non configurée), retourne la valeur zéro d'agent.MemoryTools
// : aucun outil mémoire n'est alors jamais exposé, quelle que soit la
// configuration des agents.
func buildMemoryTools(cfg *config.Config, store *memory.AmoxtliStore, metrics *observability.Metrics) agent.MemoryTools {
	if store == nil {
		return agent.MemoryTools{}
	}

	return agent.MemoryTools{
		Store:      store,
		Authorizer: authorization.NewAuthorizer(cfg),
		Search:     true,
		Remember:   true,
		Forget:     true,
		Episodes:   store,
		History:    true,
		Recall:     true,
		MaxResults: 5,
		Metrics:    metrics,
	}
}
