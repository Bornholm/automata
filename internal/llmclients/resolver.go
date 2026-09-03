package llmclients

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

// Resolver choisit le client d'un rôle : la surcharge de l'organisation si
// elle en a posé une, le défaut de l'instance sinon — les deux vivent dans
// la MÊME table (org_agent_clients), le défaut d'instance étant la ligne
// dont org_id est vide (migration 0023). Le YAML ne participe plus à la
// résolution : la base fait foi, et un rôle sans défaut est une erreur
// nommée, pas un repli silencieux.
//
// Dans tous les cas le client vient du pool : une modification faite dans
// l'administration prend effet au tour suivant, sans redémarrage.
type Resolver struct {
	pool   *Pool
	store  *Store
	logger *slog.Logger
	// images sert les rôles de génération d'images ; nil = aucune
	// résolution d'images.
	images *ImagePool
}

// NewResolver construit un résolveur.
func NewResolver(pool *Pool, store *Store, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}

	return &Resolver{pool: pool, store: store, logger: logger}
}

// WithImagePool branche le pool des générateurs d'images. Retourne r pour
// permettre le chaînage.
func (r *Resolver) WithImagePool(pool *ImagePool) *Resolver {
	r.images = pool
	return r
}

// ErrNoDefault signale un rôle sans défaut d'instance : la fonctionnalité
// qui en dépend doit se désactiver avec un message actionnable, jamais
// paniquer. errors.Is le distingue d'une vraie panne de base.
var ErrNoDefault = fmt.Errorf("llmclients: no client configured for this role")

// Rôles système, distincts des noms d'agents déclarés dans la
// configuration.
const (
	RolePlugins       = "plugins"
	RolePluginsVision = "plugins.vision"
	RoleCompaction    = "compaction"
	RoleTranscription = "transcription"
	RoleConsolidation = "consolidation"
	// RoleRetrieval sert la reformulation HyDE de la recherche mémoire.
	RoleRetrieval = "retrieval"
	// RoleIntrospection sert la passe hebdomadaire de suggestions
	// (internal/introspection).
	RoleIntrospection = "introspection"
	// RoleJudge relit les réponses produites sans aucun appel d'outil et
	// dit si elles affirment un fait que rien n'appuie (voir
	// internal/agent/judge.go). Sans modèle affecté, la vérification est
	// simplement absente : aucune réponse n'en dépend.
	RoleJudge = "judge"
	// RoleImagePrefix préfixe le rôle de génération d'images d'un agent :
	// « image:imagine ». Un agent a deux modèles distincts — celui qui
	// converse et celui qui dessine.
	RoleImagePrefix = "image:"
	// RoleEmbeddingsPrefix préfixe le rôle d'embeddings d'un index
	// sémantique : « embeddings:semantic ». VERROUILLÉ après le premier
	// démarrage réussi (voir la sentinelle dans internal/registry/memory.go) :
	// changer le modèle rendrait les vecteurs déjà écrits incomparables.
	RoleEmbeddingsPrefix = "embeddings:"
)

// ImageRole compose le rôle de génération d'images d'un agent.
func ImageRole(agentName string) string {
	return RoleImagePrefix + agentName
}

// EmbeddingsRole compose le rôle d'embeddings d'un index sémantique.
func EmbeddingsRole(indexID string) string {
	return RoleEmbeddingsPrefix + indexID
}

// Roles dresse la liste des rôles que cette instance peut configurer,
// d'après ce que la configuration DÉCLARE (agents, index, fonctionnalités
// actives) — jamais d'après la base : un rôle orphelin en base (agent
// renommé) est simplement ignoré.
//
// Les noms de clients, eux, ne viennent plus jamais du YAML.
func Roles(cfg *config.Config) []string {
	var roles []string

	for name, agentCfg := range cfg.Agents {
		roles = append(roles, name)
		if agentCfg.ImageGeneration {
			roles = append(roles, ImageRole(name))
		}
	}

	// Toujours proposé : la vérification des réponses sans appel d'outil
	// n'a pas de drapeau de configuration, elle s'active en affectant un
	// modèle au rôle et se désactive en le retirant.
	roles = append(roles, RoleJudge)

	if cfg.Plugins.Enabled {
		roles = append(roles, RolePlugins, RolePluginsVision)
	}
	if cfg.Conversation.Compaction.Enabled {
		roles = append(roles, RoleCompaction)
	}
	if cfg.Audio.Enabled {
		roles = append(roles, RoleTranscription)
	}
	if cfg.Memory.Consolidation.Enabled {
		roles = append(roles, RoleConsolidation)
	}
	if cfg.Introspection.Enabled {
		roles = append(roles, RoleIntrospection)
	}
	if cfg.Memory.Retrieval.Profile == "balanced" {
		roles = append(roles, RoleRetrieval)
	}
	for _, index := range cfg.Memory.Indexes {
		if index.Type == "sqlitevec" {
			roles = append(roles, EmbeddingsRole(index.ID))
		}
	}

	sort.Strings(roles)

	return roles
}

// ResolveClient retourne le client à utiliser pour ce rôle et cette
// organisation.
//
// Une organisation vide (exécution hors contexte d'organisation) ou sans
// surcharge obtient le défaut de l'instance. Aucun défaut : ErrNoDefault,
// avec le rôle dans le message — l'appelant décide s'il dégrade ou s'il
// fait échouer le tour, jamais en silence.
func (r *Resolver) ResolveClient(ctx context.Context, role string, orgID model.OrgID) (Resolved, error) {
	name, err := r.clientNameFor(ctx, role, orgID)
	if err != nil {
		return Resolved{}, err
	}

	return r.pool.Get(ctx, name)
}

// ResolveImageClient retourne le générateur d'images du rôle pour cette
// organisation, selon la même préséance que ResolveClient.
func (r *Resolver) ResolveImageClient(ctx context.Context, role string, orgID model.OrgID) (llm.ImageGenerationClient, error) {
	if r.images == nil {
		return nil, fmt.Errorf("%w: %q (image pool not wired)", ErrNoDefault, role)
	}

	name, err := r.clientNameFor(ctx, role, orgID)
	if err != nil {
		return nil, err
	}

	return r.images.Get(ctx, name)
}

// clientNameFor applique la règle de préséance : surcharge de
// l'organisation, puis défaut de l'instance (org_id vide).
func (r *Resolver) clientNameFor(ctx context.Context, role string, orgID model.OrgID) (string, error) {
	if orgID != "" {
		name, found, err := r.store.OrgChoice(ctx, string(orgID), role)
		if err != nil {
			// On journalise et on retombe sur le défaut d'instance :
			// perdre la surcharge est préférable à perdre la conversation.
			r.logger.WarnContext(ctx, "llmclients: lecture du modèle de l'organisation en échec",
				"org", string(orgID), "role", role, "error", err)
		} else if found && name != "" {
			return name, nil
		}
	}

	name, found, err := r.store.OrgChoice(ctx, "", role)
	if err != nil {
		return "", fmt.Errorf("llmclients: lecture du défaut d'instance du rôle %q: %w", role, err)
	}
	if !found || name == "" {
		return "", fmt.Errorf("%w: %q — set an instance default in the administration (Modèles)", ErrNoDefault, role)
	}

	return name, nil
}

// InstanceDefault retourne le client par défaut de l'instance pour un rôle,
// ou ("", nil) s'il n'est pas configuré. Sert aux écrans d'administration
// et aux composants construits au démarrage.
func (r *Resolver) InstanceDefault(ctx context.Context, role string) (string, error) {
	name, _, err := r.store.OrgChoice(ctx, "", role)
	return name, err
}
