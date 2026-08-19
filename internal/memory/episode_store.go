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

	"github.com/bornholm/automata/internal/model"
)

// La mémoire épisodique conserve des fragments VERBATIM de conversations,
// juste avant que la compaction les dilue dans le résumé roulant : c'est ce
// qui permet de répondre à « qu'avait-on décidé le mois dernier ? » quand
// le résumé et les faits extraits n'ont pas retenu le détail.
//
// Les épisodes partagent le codex amoxtli de la mémoire sémantique mais
// vivent sous leur propre hôte d'URL source (amoxtli://episode/<id>) : ils
// sont ainsi invisibles de Store.List — donc de la consolidation périodique,
// qui ne doit jamais fusionner ni réécrire du verbatim — et écartés des
// résultats de Store.Search par resolveResults. La séparation est doublée
// au niveau de l'index par la métadonnée "kind" (voir episodeConditions et
// scopeConditions), pour que les épisodes ne consomment pas le quota de
// résultats des recherches de faits, et réciproquement.

// episodeHost est l'hôte conventionnel des URL sources synthétiques des
// épisodes ("amoxtli://episode/<id>").
const episodeHost = "episode"

// episodeIDMetadataKey joue pour les épisodes le rôle de
// memoryIDMetadataKey pour les souvenirs : retrouver un document par son
// identifiant exact sans recherche plein texte hasardeuse.
const episodeIDMetadataKey = "episode_id"

// episodeKind est la valeur de la métadonnée "kind" marquant un épisode.
// Les souvenirs sémantiques ne portent aucune métadonnée "kind" : voir
// scopeConditions, qui exclut toute valeur présente.
const episodeKind = "episode"

// Episode est un fragment de conversation mémorisé verbatim, restitué par
// SearchEpisodes.
type Episode struct {
	ID             string
	Content        string
	ConversationID model.ConversationID
	// From et To bornent la période couverte par le fragment (horodatage du
	// premier et du dernier message).
	From time.Time
	To   time.Time
}

// NewEpisode décrit l'enregistrement d'un fragment de conversation. Comme
// NewMemory, tous les champs de portée sont déterminés par l'application,
// jamais par le LLM — l'enregistrement est d'ailleurs entièrement
// automatique (internal/conversation.Compactor), aucun outil n'écrit ici.
type NewEpisode struct {
	Content        string
	OrgID          model.OrgID
	Scope          model.Scope
	ScopeID        model.ScopeID
	ConversationID model.ConversationID
	From           time.Time
	To             time.Time
}

// EpisodeQuery décrit une recherche d'épisodes déjà cloisonnée, sur le même
// contrat que Query : la portée est fournie par l'appelant, jamais déduite.
type EpisodeQuery struct {
	Text       string
	OrgID      model.OrgID
	Scope      model.Scope
	ScopeID    model.ScopeID
	MaxResults int
}

// EpisodeStore est l'interface applicative de la mémoire épisodique,
// implémentée par AmoxtliStore. Volontairement minimale : pas de
// suppression exposée — les épisodes ne sont réorganisés par aucun
// mécanisme, seul un nettoyage de rétention pourra un jour en supprimer.
type EpisodeStore interface {
	// RecordEpisode enregistre un fragment et attend la fin de son
	// indexation avant de retourner.
	RecordEpisode(ctx context.Context, ep NewEpisode) (Episode, error)
	// SearchEpisodes retourne les épisodes correspondant à query.Text dans
	// la portée exacte décrite par query.
	SearchEpisodes(ctx context.Context, query EpisodeQuery) ([]Episode, error)
}

// episodeSource construit l'URL source synthétique associée à id.
func episodeSource(id string) (*url.URL, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("memory: identifiant d'épisode vide")
	}
	return &url.URL{Scheme: sourceScheme, Host: episodeHost, Path: "/" + id}, nil
}

// episodeIDFromSource extrait l'identifiant d'épisode d'une URL source
// synthétique. ok vaut false si source ne relève pas des épisodes.
func episodeIDFromSource(source *url.URL) (string, bool) {
	if source == nil || source.Scheme != sourceScheme || source.Host != episodeHost {
		return "", false
	}
	id := strings.TrimPrefix(source.Path, "/")
	if id == "" {
		return "", false
	}
	return id, true
}

// episodeConditions construit les filtres de cloisonnement d'une recherche
// d'épisodes : la portée obligatoire, plus la restriction aux documents de
// type épisode.
func episodeConditions(orgID model.OrgID, scope model.Scope, scopeID model.ScopeID) []amoxtliindex.Condition {
	conditions := []amoxtliindex.Condition{
		amoxtliindex.Eq("org_id", string(orgID)),
		amoxtliindex.Eq("scope", string(scope)),
		amoxtliindex.Eq("kind", episodeKind),
	}
	if scopeID != "" {
		conditions = append(conditions, amoxtliindex.Eq("scope_id", string(scopeID)))
	}
	return conditions
}

// RecordEpisode implémente EpisodeStore. Même politique de cohérence
// store/index que Remember : l'indexation est attendue, amoxtli compense
// lui-même un échec.
func (s *AmoxtliStore) RecordEpisode(ctx context.Context, ep NewEpisode) (Episode, error) {
	content := strings.TrimSpace(ep.Content)
	if content == "" {
		return Episode{}, fmt.Errorf("memory: contenu d'épisode vide")
	}

	id := uuid.NewString()

	source, err := episodeSource(id)
	if err != nil {
		return Episode{}, err
	}

	now := time.Now().UTC()

	metadata := map[string]any{
		"org_id":             string(ep.OrgID),
		"scope":              string(ep.Scope),
		"scope_id":           string(ep.ScopeID),
		"conversation_id":    string(ep.ConversationID),
		"created_at":         now.Format(time.RFC3339),
		"from":               ep.From.UTC().Format(time.RFC3339),
		"to":                 ep.To.UTC().Format(time.RFC3339),
		"content_type":       "text/plain",
		"kind":               episodeKind,
		episodeIDMetadataKey: id,
	}

	// Même convention de préfixe que les souvenirs : contentFromSections le
	// retire à la restitution.
	indexedContent := id + "\n" + content

	taskID, err := s.codex.IndexFile(ctx, s.collectionID, id+".md", strings.NewReader(indexedContent),
		amoxtli.WithIndexFileSource(source),
		amoxtli.WithIndexFileMetadata(metadata),
	)
	if err != nil {
		return Episode{}, fmt.Errorf("memory: indexation de l'épisode: %w", err)
	}

	if err := s.waitForTask(ctx, taskID, s.indexTimeout); err != nil {
		return Episode{}, fmt.Errorf("memory: échec de l'indexation de l'épisode: %w", err)
	}

	return Episode{
		ID:             id,
		Content:        content,
		ConversationID: ep.ConversationID,
		From:           ep.From,
		To:             ep.To,
	}, nil
}

// SearchEpisodes implémente EpisodeStore.
func (s *AmoxtliStore) SearchEpisodes(ctx context.Context, query EpisodeQuery) ([]Episode, error) {
	if strings.TrimSpace(query.Text) == "" {
		// Même contrat que Search : voir le commentaire sur l'absence de
		// "match all" dans la façade amoxtli.
		return nil, fmt.Errorf("memory: texte de requête vide")
	}

	maxResults := query.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}

	results, err := s.codex.Search(ctx, query.Text,
		amoxtli.WithSearchFilter(episodeConditions(query.OrgID, query.Scope, query.ScopeID)...),
		amoxtli.WithSearchMaxResults(maxResults),
	)
	if err != nil {
		return nil, fmt.Errorf("memory: recherche d'épisodes: %w", err)
	}

	episodes := make([]Episode, 0, len(results))

	for _, r := range results {
		id, ok := episodeIDFromSource(r.Source)
		if !ok {
			continue
		}

		sections, err := s.codex.GetSectionsByIDs(ctx, r.Sections)
		if err != nil {
			return nil, fmt.Errorf("memory: récupération du contenu de l'épisode %q: %w", id, err)
		}

		content, _, metadata, err := contentFromSections(id, r.Sections, sections)
		if err != nil {
			return nil, err
		}

		episodes = append(episodes, Episode{
			ID:             id,
			Content:        content,
			ConversationID: model.ConversationID(metadata["conversation_id"]),
			From:           parseMetadataTime(metadata["from"]),
			To:             parseMetadataTime(metadata["to"]),
		})
	}

	return episodes, nil
}

// parseMetadataTime décode un horodatage RFC 3339 de métadonnée, en
// tolérant l'absence (zéro) : une borne manquante ne doit pas faire échouer
// la restitution d'un épisode.
func parseMetadataTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
