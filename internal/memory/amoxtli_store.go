package memory

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bornholm/amoxtli"
	amoxtliindex "github.com/bornholm/amoxtli/index"
	amoxtliingest "github.com/bornholm/amoxtli/ingest"
	amoxtlimodel "github.com/bornholm/amoxtli/model"
	amoxtlitask "github.com/bornholm/amoxtli/task"

	"github.com/bornholm/automata/internal/model"
)

// sourceScheme est le schéma des URL sources synthétiques utilisées pour
// identifier chaque mémoire de façon stable. La façade *amoxtli.Codex
// n'expose la suppression que par URL source (Codex.DeleteBySource), jamais
// par identifiant de document arbitraire : voir
// docs/integration-inventory.md §3, décision actée d'adopter une URL
// synthétique plutôt que de modifier amoxtli.
const sourceScheme = "amoxtli"

// sourceHost est l'hôte conventionnel des URL sources synthétiques
// ("amoxtli://memory/<id>").
const sourceHost = "memory"

// memoryIDMetadataKey est une métadonnée additionnelle, au-delà des
// métadonnées obligatoires du plan (§8.2), stockée pour permettre une
// récupération fiable d'une mémoire par son identifiant exact (voir
// GetByID) sans dépendre d'une recherche plein texte hasardeuse sur
// l'identifiant.
const memoryIDMetadataKey = "memory_id"

// defaultIndexTimeout borne l'attente de la tâche d'indexation asynchrone
// déclenchée par Codex.IndexFile (PLAN.md §8.4 : "l'application doit suivre
// l'état via le task.Runner ... avant de considérer la mémoire comme
// persistée").
const defaultIndexTimeout = 30 * time.Second

// defaultReindexTimeout borne l'attente de Codex.Reindex, potentiellement
// plus long qu'une simple indexation de document.
const defaultReindexTimeout = 5 * time.Minute

// taskPollInterval est l'intervalle de scrutation de l'état d'une tâche
// asynchrone amoxtli. Il n'existe pas d'attente bloquante côté amoxtli
// (task.Runner n'expose qu'un accès par scrutation, voir
// example/sqlite/main.go du dépôt amoxtli) : c'est le pattern documenté par
// la bibliothèque elle-même.
const taskPollInterval = 100 * time.Millisecond

// AmoxtliStore implémente Store en s'appuyant sur un *amoxtli.Codex réel.
//
// Gestion des collections (PLAN.md, "Configurer les collections ou
// namespaces") : la V1 d'Automata ne supporte qu'une seule organisation par
// instance (voir internal/identity.EffectivePermissions), donc une seule
// collection amoxtli suffit pour tout le déploiement. Le cloisonnement
// personal/group/org n'est jamais assuré par la collection amoxtli : il est
// intégralement porté par les métadonnées obligatoires (§8.2) et les
// filtres appliqués à chaque Search (§8.3). La collection n'est donc pas un
// périmètre de sécurité, seulement un paramètre requis par l'API
// Codex.IndexFile ; en conséquence NewAmoxtliStore en crée une nouvelle à
// chaque démarrage sans se soucier de la retrouver d'un redémarrage à
// l'autre : Codex.Search ne restreint jamais par collection ici (aucun
// WithSearchCollections n'est utilisé), donc les mémoires indexées lors
// d'un précédent démarrage, dans une collection différente, restent
// pleinement retrouvables.
type AmoxtliStore struct {
	codex          *amoxtli.Codex
	collectionID   amoxtlimodel.CollectionID
	indexTimeout   time.Duration
	reindexTimeout time.Duration
}

// NewAmoxtliStore construit un AmoxtliStore adossé à codex, en créant une
// collection dédiée à la mémoire (label purement descriptif, voir le
// commentaire de AmoxtliStore sur la gestion des collections).
func NewAmoxtliStore(ctx context.Context, codex *amoxtli.Codex, collectionLabel string) (*AmoxtliStore, error) {
	if codex == nil {
		return nil, fmt.Errorf("memory: codex amoxtli requis")
	}

	collectionID, err := codex.CreateCollection(ctx, collectionLabel)
	if err != nil {
		return nil, fmt.Errorf("memory: création de la collection %q: %w", collectionLabel, err)
	}

	return &AmoxtliStore{
		codex:          codex,
		collectionID:   collectionID,
		indexTimeout:   defaultIndexTimeout,
		reindexTimeout: defaultReindexTimeout,
	}, nil
}

// memorySource construit l'URL source synthétique associée à id.
func memorySource(id string) (*url.URL, error) {
	u := &url.URL{Scheme: sourceScheme, Host: sourceHost, Path: "/" + id}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("memory: identifiant vide")
	}
	return u, nil
}

// memoryIDFromSource extrait l'identifiant de mémoire d'une URL source
// synthétique. ok vaut false si source ne correspond pas au schéma attendu
// (ex: un document indexé par un autre mécanisme, hors du périmètre de ce
// store).
func memoryIDFromSource(source *url.URL) (string, bool) {
	if source == nil || source.Scheme != sourceScheme || source.Host != sourceHost {
		return "", false
	}
	id := strings.TrimPrefix(source.Path, "/")
	if id == "" {
		return "", false
	}
	return id, true
}

// Remember implémente Store. Voir le commentaire de package pour la
// politique de cohérence store/index : en cas d'échec de la tâche
// d'indexation, amoxtli effectue lui-même le rollback (compensation de la
// saga d'ingestion, voir docs/integration-inventory.md §3, "Garanties
// transactionnelles") ; il n'y a donc pas de mémoire fantôme à nettoyer ici,
// juste une erreur claire à remonter.
func (s *AmoxtliStore) Remember(ctx context.Context, mem NewMemory) (Memory, error) {
	content := strings.TrimSpace(mem.Content)
	if content == "" {
		return Memory{}, fmt.Errorf("memory: contenu vide")
	}

	id := uuid.NewString()

	source, err := memorySource(id)
	if err != nil {
		return Memory{}, err
	}

	now := time.Now().UTC()

	metadata := map[string]any{
		"org_id":                 string(mem.OrgID),
		"scope":                  string(mem.Scope),
		"scope_id":               string(mem.ScopeID),
		"owner_principal_id":     string(mem.OwnerPrincipalID),
		"created_by":             string(mem.CreatedBy),
		"created_at":             now.Format(time.RFC3339),
		"source_conversation_id": string(mem.SourceConversationID),
		"content_type":           "text/plain",
		memoryIDMetadataKey:      id,
	}

	if origin := strings.TrimSpace(mem.Origin); origin != "" {
		metadata["origin"] = origin
	}

	// Le contenu réellement indexé est préfixé par l'identifiant, sur sa
	// propre ligne : c'est ce qui permet à GetByID de retrouver une mémoire
	// de façon fiable par une recherche plein texte filtrée sur l'identifiant
	// (Codex.Search exige toujours un texte de requête ; un identifiant
	// aléatoire n'apparaîtrait sinon dans aucun contenu). Le préfixe est
	// retiré avant restitution (voir stripIDPrefix), il n'est jamais visible
	// de l'appelant.
	indexedContent := id + "\n" + content

	filename := id + ".md"

	taskID, err := s.codex.IndexFile(ctx, s.collectionID, filename, strings.NewReader(indexedContent),
		amoxtli.WithIndexFileSource(source),
		amoxtli.WithIndexFileMetadata(metadata),
	)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: indexation de la mémoire: %w", err)
	}

	if err := s.waitForTask(ctx, taskID, s.indexTimeout); err != nil {
		return Memory{}, fmt.Errorf("memory: échec de l'indexation de la mémoire: %w", err)
	}

	return Memory{
		ID:        id,
		Content:   content,
		Metadata:  stringMetadata(metadata),
		CreatedAt: now,
	}, nil
}

// Search implémente Store.
func (s *AmoxtliStore) Search(ctx context.Context, query Query) ([]Memory, error) {
	conditions := scopeConditions(query.OrgID, query.Scope, query.ScopeID)

	maxResults := query.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}

	// Codex.Search interroge toujours l'index plein texte avec ce texte :
	// amoxtli n'expose pas de "match all" via cette façade (voir
	// docs/integration-inventory.md §3), donc une requête vide ne
	// correspondrait à aucun contenu plutôt qu'à "toutes les mémoires de
	// cette portée". On échoue explicitement plutôt que de retourner une
	// liste vide silencieuse qui serait mal interprétée comme "aucune
	// mémoire dans cette portée".
	if strings.TrimSpace(query.Text) == "" {
		return nil, fmt.Errorf("memory: texte de requête vide")
	}

	results, err := s.codex.Search(ctx, query.Text,
		amoxtli.WithSearchFilter(conditions...),
		amoxtli.WithSearchMaxResults(maxResults),
	)
	if err != nil {
		return nil, fmt.Errorf("memory: recherche: %w", err)
	}

	return s.resolveResults(ctx, results)
}

// GetByID implémente Store. Voir le commentaire de package sur
// memoryIDMetadataKey pour le mécanisme utilisé afin de retrouver une
// mémoire par son identifiant exact.
func (s *AmoxtliStore) GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (Memory, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Memory{}, false, fmt.Errorf("memory: identifiant vide")
	}

	conditions := scopeConditions(orgID, scope, scopeID)
	conditions = append(conditions, amoxtliindex.Eq(memoryIDMetadataKey, id))

	results, err := s.codex.Search(ctx, id,
		amoxtli.WithSearchFilter(conditions...),
		amoxtli.WithSearchMaxResults(5),
	)
	if err != nil {
		return Memory{}, false, fmt.Errorf("memory: recherche par identifiant: %w", err)
	}

	memories, err := s.resolveResults(ctx, results)
	if err != nil {
		return Memory{}, false, err
	}

	for _, m := range memories {
		if m.ID == id {
			return m, true, nil
		}
	}

	return Memory{}, false, nil
}

// Forget implémente Store.
func (s *AmoxtliStore) Forget(ctx context.Context, id string) error {
	source, err := memorySource(id)
	if err != nil {
		return err
	}

	if err := s.codex.DeleteBySource(ctx, source); err != nil {
		return fmt.Errorf("memory: suppression de la mémoire %q: %w", id, err)
	}

	return nil
}

// listPageSize est la taille de page utilisée par List pour parcourir le
// store document par document sans jamais tout charger d'un bloc.
const listPageSize = 200

// List implémente Store. Contrairement à Search, qui passe par l'index
// plein texte (et exige donc un texte de requête), List interroge
// directement le store documentaire d'amoxtli (Codex.Manager) en filtrant
// sur le préfixe des URL sources synthétiques : c'est le seul moyen exposé
// par amoxtli d'énumérer exhaustivement les documents (voir
// ingest.Store.QueryDocuments), et c'est exactement ce dont la
// consolidation périodique a besoin. Voir l'avertissement de l'interface :
// aucune restriction de portée n'est appliquée ici.
func (s *AmoxtliStore) List(ctx context.Context) ([]Memory, error) {
	manager := s.codex.Manager()

	// SourcePattern est un motif LIKE %pattern% côté store : le préfixe des
	// URL synthétiques suffit à exclure tout document indexé par un autre
	// mécanisme dans le même codex.
	pattern := sourceScheme + "://" + sourceHost + "/"

	var memories []Memory

	for page := 0; ; page++ {
		p := page
		limit := listPageSize
		documents, _, err := manager.QueryDocuments(ctx, amoxtliingest.QueryDocumentsOptions{
			Page:          &p,
			Limit:         &limit,
			SourcePattern: &pattern,
		})
		if err != nil {
			return nil, fmt.Errorf("memory: énumération des mémoires (page %d): %w", page, err)
		}

		for _, doc := range documents {
			id, ok := memoryIDFromSource(doc.Source())
			if !ok {
				continue
			}

			raw, err := doc.Content()
			if err != nil {
				return nil, fmt.Errorf("memory: lecture du contenu de %q: %w", id, err)
			}

			var (
				metadata  map[string]string
				createdAt time.Time
			)
			if rawMeta := amoxtlimodel.Metadata(doc); rawMeta != nil {
				metadata = stringMetadata(rawMeta)
				if createdAtStr, ok := metadata["created_at"]; ok {
					if parsed, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
						createdAt = parsed
					}
				}
			}

			memories = append(memories, Memory{
				ID:        id,
				Content:   stripIDPrefix(string(raw), id),
				Metadata:  metadata,
				CreatedAt: createdAt,
			})
		}

		if len(documents) < listPageSize {
			return memories, nil
		}
	}
}

// Reindex implémente Store.
func (s *AmoxtliStore) Reindex(ctx context.Context) error {
	taskID, err := s.codex.Reindex(ctx)
	if err != nil {
		return fmt.Errorf("memory: déclenchement de la réindexation: %w", err)
	}

	if err := s.waitForTask(ctx, taskID, s.reindexTimeout); err != nil {
		return fmt.Errorf("memory: échec de la réindexation: %w", err)
	}

	return nil
}

// resolveResults résout des *index.SearchResult amoxtli en Memory
// exploitables par l'application : contenu texte (via les sections du
// document) et identifiant stable (extrait de l'URL source synthétique).
// Un résultat dont la source ne correspond pas au schéma attendu est ignoré
// plutôt que de faire échouer toute la recherche (défensif : d'autres
// mécanismes pourraient en théorie indexer dans le même codex).
func (s *AmoxtliStore) resolveResults(ctx context.Context, results []*amoxtliindex.SearchResult) ([]Memory, error) {
	memories := make([]Memory, 0, len(results))

	for _, r := range results {
		id, ok := memoryIDFromSource(r.Source)
		if !ok {
			continue
		}

		sections, err := s.codex.GetSectionsByIDs(ctx, r.Sections)
		if err != nil {
			return nil, fmt.Errorf("memory: récupération du contenu de %q: %w", id, err)
		}

		content, createdAt, metadata, err := contentFromSections(id, r.Sections, sections)
		if err != nil {
			return nil, err
		}

		memories = append(memories, Memory{
			ID:        id,
			Content:   content,
			Metadata:  metadata,
			CreatedAt: createdAt,
		})
	}

	return memories, nil
}

// contentFromSections concatène le contenu des sections d'un document dans
// l'ordre fourni par le résultat de recherche, retire le préfixe
// d'identifiant (voir Remember) et extrait created_at / les métadonnées
// depuis le document porté par la première section, s'il en implémente
// model.WithMetadata.
func contentFromSections(id string, sectionIDs []amoxtlimodel.SectionID, sections map[amoxtlimodel.SectionID]amoxtlimodel.Section) (string, time.Time, map[string]string, error) {
	var parts []string
	var metadata map[string]string
	var createdAt time.Time

	for i, sectionID := range sectionIDs {
		section, ok := sections[sectionID]
		if !ok {
			continue
		}

		raw, err := section.Content()
		if err != nil {
			return "", time.Time{}, nil, fmt.Errorf("memory: lecture du contenu de %q: %w", id, err)
		}

		parts = append(parts, string(raw))

		if i == 0 {
			if doc := section.Document(); doc != nil {
				if raw := amoxtlimodel.Metadata(doc); raw != nil {
					metadata = stringMetadata(raw)
					if createdAtStr, ok := metadata["created_at"]; ok {
						if parsed, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
							createdAt = parsed
						}
					}
				}
			}
		}
	}

	content := strings.Join(parts, "\n\n")
	content = stripIDPrefix(content, id)

	return content, createdAt, metadata, nil
}

// stripIDPrefix retire le préfixe "<id>\n" ajouté par Remember au contenu
// indexé, pour ne jamais exposer ce détail d'implémentation à l'appelant.
func stripIDPrefix(content, id string) string {
	prefix := id + "\n"
	return strings.TrimPrefix(content, prefix)
}

// scopeConditions construit les filtres de cloisonnement obligatoires
// (PLAN.md §8.3) pour orgID/scope/scopeID. Ce package ne décide jamais de la
// portée : il applique celle qui lui est donnée par l'appelant (voir le
// commentaire de package).
func scopeConditions(orgID model.OrgID, scope model.Scope, scopeID model.ScopeID) []amoxtliindex.Condition {
	conditions := []amoxtliindex.Condition{
		amoxtliindex.Eq("org_id", string(orgID)),
		amoxtliindex.Eq("scope", string(scope)),
		// Les souvenirs sémantiques ne portent aucune métadonnée "kind" ;
		// exclure toute valeur présente écarte les épisodes
		// (episode_store.go) dès l'index, sans quoi ils consommeraient le
		// quota de résultats avant d'être écartés par resolveResults.
		amoxtliindex.NotExists("kind"),
	}
	if scopeID != "" {
		conditions = append(conditions, amoxtliindex.Eq("scope_id", string(scopeID)))
	}
	return conditions
}

// stringMetadata convertit une métadonnée map[string]any (format attendu par
// amoxtli.WithIndexFileMetadata / model.WithMetadata) en map[string]string
// (format applicatif, voir Memory.Metadata).
func stringMetadata(metadata map[string]any) map[string]string {
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// waitForTask scrute l'état de la tâche asynchrone id jusqu'à ce qu'elle
// atteigne un état terminal (succès ou échec), jusqu'à expiration de
// timeout ou annulation de ctx. C'est le pattern documenté par amoxtli
// lui-même (voir example/sqlite/main.go du dépôt amoxtli) : la bibliothèque
// n'expose pas d'attente bloquante, seulement Codex.TaskState en
// scrutation.
func (s *AmoxtliStore) waitForTask(ctx context.Context, id amoxtlitask.ID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		state, err := s.codex.TaskState(ctx, id)
		if err != nil {
			return fmt.Errorf("consultation de l'état de la tâche %q: %w", id, err)
		}

		switch state.Status {
		case amoxtlitask.StatusSucceeded:
			return nil
		case amoxtlitask.StatusFailed:
			return fmt.Errorf("tâche %q en échec: %v", id, state.Error)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("tâche %q non terminée après %s (statut: %s)", id, timeout, state.Status)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

var _ Store = (*AmoxtliStore)(nil)
