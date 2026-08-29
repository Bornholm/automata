package registry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bornholm/amoxtli"
	amoxtlibleve "github.com/bornholm/amoxtli/index/bleve"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	"github.com/bornholm/amoxtli/ingest"
	amoxtligorm "github.com/bornholm/amoxtli/ingest/gorm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
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
// models donne accès au catalogue de modèles : le client d'embeddings de
// chaque index sqlitevec et le client HyDE se règlent en ligne (rôles
// « embeddings:<id> » et « retrieval »). Nil : ces fonctions sont
// désactivées avec un avertissement, jamais un crash.
func buildMemory(ctx context.Context, cfg *config.Config, models *llmclients.Store) (memoryResources, error) {
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

	var (
		indexers []amoxtli.Indexer
		// indexRecreated signale qu'un index vient d'être reconstruit vide
		// (première ouverture, ou changement de mapping à une montée de
		// version). Sans réindexation, la recherche mémoire rendrait
		// silencieusement zéro résultat sur tout le corpus.
		indexRecreated bool
	)
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

			// Deux signaux de réindexation : l'index vient d'être
			// reconstruit (changement de mapping), ou il est vide alors
			// que le corpus ne l'est pas — l'état que laisse une
			// réindexation interrompue à un démarrage précédent.
			if bleveIdx.Recreated() {
				indexRecreated = true
			} else if count, err := bleveIdx.DocCount(); err == nil && count == 0 {
				page, limit := 0, 1
				docs, _, err := store.QueryDocuments(ctx, ingest.QueryDocumentsOptions{
					Page: &page, Limit: &limit, HeaderOnly: true,
				})
				if err == nil && len(docs) > 0 {
					indexRecreated = true
				}
			}

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

			// Le client d'embeddings se règle en ligne (rôle
			// « embeddings:<id> »). Sans catalogue ou sans défaut, l'index
			// sémantique est ignoré : la recherche mémoire dégrade sur les
			// autres index, l'instance continue de servir.
			clientCfg, clientName, ok := embeddingsConfig(ctx, models, idxCfg.ID)
			if !ok {
				slog.WarnContext(ctx, "registry: mémoire: index sémantique désactivé, aucun modèle configuré",
					"index", idxCfg.ID, "remède", "réglez le rôle embeddings:"+idxCfg.ID+" dans l'administration (Modèles)")
				continue
			}

			// VERROU : un index vectoriel est physiquement lié au modèle qui
			// a produit ses vecteurs. La sentinelle posée à côté du fichier
			// mémorise ce modèle au premier démarrage ; toute divergence est
			// FATALE — c'est le seul refus de démarrer de tout le catalogue,
			// et il attrape toutes les voies de contournement (rôle modifié,
			// client du catalogue édité).
			if err := checkEmbeddingsSentinel(idxCfg.Path, clientCfg.Provider, clientCfg.Model); err != nil {
				return res, fmt.Errorf("registry: mémoire: memory.indexes[%q]: %w", idxCfg.ID, err)
			}

			embeddings, err := agent.BuildEmbeddingsClient(ctx, clientCfg)
			if err != nil {
				return res, fmt.Errorf("registry: mémoire: client d'embeddings %q de memory.indexes[%q]: %w", clientName, idxCfg.ID, err)
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
		// Le modèle HyDE se règle en ligne (rôle « retrieval »). Sans
		// défaut, le profil dégrade en « fast » avec un avertissement :
		// la recherche continue, sans reformulation.
		hydeCfg, hydeName, ok := roleClientConfig(ctx, models, llmclients.RoleRetrieval)
		if !ok {
			slog.WarnContext(ctx, "registry: mémoire: profil balanced sans modèle HyDE, recherche en profil fast",
				"remède", "réglez le rôle retrieval dans l'administration (Modèles)")
			codexOpts = append(codexOpts, amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge())
			break
		}
		hydeClient, err := agent.BuildLLMClient(ctx, hydeCfg)
		if err != nil {
			return res, fmt.Errorf("registry: mémoire: client HyDE %q: %w", hydeName, err)
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

	if indexRecreated {
		// Réindexation au démarrage, par le worker lui-même : lui seul
		// détient les verrous de la base mémoire et de l'index, une
		// commande externe resterait bloquée dessus. La tâche est
		// asynchrone — le service répond pendant qu'elle avance.
		if _, err := codex.Reindex(ctx); err != nil {
			slog.ErrorContext(ctx, "registry: mémoire: réindexation après reconstruction de l'index échouée", "error", err)
		} else {
			slog.InfoContext(ctx, "registry: mémoire: index reconstruit, réindexation du corpus lancée")
		}
	}

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
//
// Le client d'embeddings d'un index sémantique vit au catalogue de modèles
// (base applicative) : la commande ouvre donc la base — le service doit
// être ARRÊTÉ, ce que le verrou d'instance garantit de toute façon.
func MemoryReindex(ctx context.Context, logger interface {
	Error(msg string, args ...any)
}, cfg *config.Config) error {
	db, err := persistence.OpenWithEncryption(ctx, cfg.Storage.Application, cfg.Storage.EncryptionKey)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer func() { _ = db.Close() }()

	llmBox, err := secretbox.NewLLMClients(cfg.Web.SessionSecret)
	if err != nil {
		return fmt.Errorf("registry: dérivation de la clé du catalogue de modèles: %w", err)
	}

	res, err := buildMemory(ctx, cfg, llmclients.NewStore(db, llmBox))
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
// L'Authorizer est celui de l'instance, source des rôles en ligne
// comprise : en reconstruire un ici perdrait les membres rattachés par
// jeton, qui se verraient refuser la mémoire alors que les rappels — même
// identité, autre outil — leur sont ouverts.
func buildMemoryTools(cfg *config.Config, authorizer *authorization.Authorizer, store *memory.AmoxtliStore, metrics *observability.Metrics) agent.MemoryTools {
	if store == nil {
		return agent.MemoryTools{}
	}

	return agent.MemoryTools{
		Store:      store,
		Authorizer: authorizer,
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

// roleClientConfig résout la définition du client servant un rôle
// d'instance : défaut du rôle en base, puis ligne du catalogue. Retourne
// false — jamais d'erreur — quand le catalogue est absent, le rôle non
// configuré ou le client disparu : ces fonctions se DÉGRADENT, l'appelant
// journalise le remède.
func roleClientConfig(ctx context.Context, models *llmclients.Store, role string) (config.LLMClient, string, bool) {
	if models == nil {
		return config.LLMClient{}, "", false
	}

	name, found, err := models.OrgChoice(ctx, "", role)
	if err != nil || !found || name == "" {
		return config.LLMClient{}, "", false
	}

	row, found, err := models.Get(ctx, name)
	if err != nil || !found {
		return config.LLMClient{}, "", false
	}

	cfg, err := models.Config(row)
	if err != nil {
		return config.LLMClient{}, "", false
	}

	return cfg, name, true
}

// embeddingsConfig résout le client d'embeddings d'un index sémantique.
func embeddingsConfig(ctx context.Context, models *llmclients.Store, indexID string) (config.LLMClient, string, bool) {
	return roleClientConfig(ctx, models, llmclients.EmbeddingsRole(indexID))
}

// checkEmbeddingsSentinel verrouille le modèle d'embeddings d'un index sur
// son premier démarrage réussi : le fichier « <index>.embedding » mémorise
// provider/model, et toute divergence ultérieure refuse de démarrer.
//
// Changer de modèle rendrait les vecteurs déjà écrits incomparables aux
// nouveaux : la recherche mémoire dégraderait EN SILENCE, sans erreur. Le
// geste de déverrouillage est volontairement manuel — supprimer l'index et
// sa sentinelle, puis laisser la réindexation reconstruire.
func checkEmbeddingsSentinel(indexPath, provider, model string) error {
	sentinelPath := indexPath + ".embedding"
	want := provider + "/" + model + "\n"

	existing, err := os.ReadFile(sentinelPath)
	switch {
	case err == nil:
		if string(existing) != want {
			return fmt.Errorf("cet index a été construit avec le modèle %q, le rôle désigne maintenant %q — "+
				"pour changer de modèle d'embeddings, supprimez l'index (%s) et sa sentinelle (%s), la réindexation le reconstruira",
				strings.TrimSpace(string(existing)), provider+"/"+model, indexPath, sentinelPath)
		}
		return nil
	case os.IsNotExist(err):
		if writeErr := os.WriteFile(sentinelPath, []byte(want), 0o600); writeErr != nil {
			return fmt.Errorf("écriture de la sentinelle d'embeddings %q: %w", sentinelPath, writeErr)
		}
		return nil
	default:
		return fmt.Errorf("lecture de la sentinelle d'embeddings %q: %w", sentinelPath, err)
	}
}
