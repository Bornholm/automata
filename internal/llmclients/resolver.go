package llmclients

import (
	"context"
	"log/slog"
	"maps"
	"sync"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

// Resolver choisit le client d'un rôle pour une organisation donnée : la
// surcharge de l'organisation si elle en a posé une, le défaut de
// l'instance sinon. Dans les deux cas le client vient du pool, donc une
// modification faite dans l'interface d'administration prend effet au tour
// suivant, sans redémarrage.
//
// Les rôles sont les noms d'agents déclarés, plus les rôles système
// RolePlugins, RolePluginsVision et RoleCompaction.
type Resolver struct {
	pool  *Pool
	store *Store
	// defaults associe un rôle au client que la configuration lui donne :
	// c'est le YAML qui dit encore quel client sert quel rôle par défaut,
	// seul le CONTENU des clients ayant migré en base.
	mu       sync.RWMutex
	defaults map[string]string
	logger   *slog.Logger
	// images sert les rôles de génération d'images ; nil = aucune
	// résolution d'images (l'appelant garde son générateur de démarrage).
	images *ImagePool
}

// WithImagePool branche le pool des générateurs d'images. Retourne r pour
// permettre le chaînage.
func (r *Resolver) WithImagePool(pool *ImagePool) *Resolver {
	r.images = pool
	return r
}

// ResolveImageClient retourne le générateur d'images du rôle pour cette
// organisation, selon la même préséance que ResolveClient.
func (r *Resolver) ResolveImageClient(ctx context.Context, role string, orgID model.OrgID) (llm.ImageGenerationClient, error) {
	if r.images == nil {
		return nil, ErrUnknownClient
	}

	name := r.clientNameFor(ctx, role, orgID)
	if name == "" {
		return nil, ErrUnknownClient
	}

	return r.images.Get(ctx, name)
}

// Rôles système, distincts des noms d'agents déclarés dans la
// configuration.
const (
	RolePlugins       = "plugins"
	RolePluginsVision = "plugins.vision"
	RoleCompaction    = "compaction"
	// RoleImagePrefix préfixe le rôle de génération d'images d'un agent :
	// « image:imagine ». Un agent a deux modèles distincts — celui qui
	// converse et celui qui dessine.
	RoleImagePrefix = "image:"
)

// ImageRole compose le rôle de génération d'images d'un agent.
func ImageRole(agentName string) string {
	return RoleImagePrefix + agentName
}

// DefaultRoles dresse la table « rôle → client par défaut » à partir de la
// configuration : un rôle par agent déclaré, plus les rôles système. C'est
// le YAML qui garde ce câblage ; seul le contenu des clients a migré en
// base.
//
// Les clients de la mémoire n'y figurent pas volontairement : le modèle
// d'embeddings d'un index ne peut pas changer sans rendre incomparables les
// vecteurs déjà écrits, et la consolidation comme la reformulation HyDE
// n'ont pas de sens à faire varier par organisation.
func DefaultRoles(cfg *config.Config) map[string]string {
	roles := make(map[string]string, len(cfg.Agents)+3)

	for name, agentCfg := range cfg.Agents {
		if agentCfg.Client != "" {
			roles[name] = agentCfg.Client
		}
		if client := agentCfg.ImageGeneration.Client; client != "" {
			roles[ImageRole(name)] = client
		}
	}

	if name := cfg.Plugins.Client; name != "" {
		roles[RolePlugins] = name
	}
	if name := cfg.Plugins.VisionClient; name != "" {
		roles[RolePluginsVision] = name
	}
	if name := cfg.Conversation.Compaction.Client; name != "" {
		roles[RoleCompaction] = name
	}

	return roles
}

// NewResolver construit un résolveur. defaults associe chaque rôle au nom
// du client que la configuration lui attribue.
func NewResolver(pool *Pool, store *Store, defaults map[string]string, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}

	copied := make(map[string]string, len(defaults))
	maps.Copy(copied, defaults)

	return &Resolver{pool: pool, store: store, defaults: copied, logger: logger}
}

// ResolveClient retourne le client à utiliser pour ce rôle et cette
// organisation.
//
// Une organisation vide (exécution hors contexte d'organisation) ou sans
// surcharge obtient le client par défaut du rôle. L'erreur retournée n'est
// jamais fatale pour l'appelant : un agent qui ne peut pas résoudre son
// client garde celui construit au démarrage, car une base illisible ne doit
// pas rendre l'assistant muet.
func (r *Resolver) ResolveClient(ctx context.Context, role string, orgID model.OrgID) (Resolved, error) {
	name := r.clientNameFor(ctx, role, orgID)
	if name == "" {
		return Resolved{}, ErrUnknownClient
	}

	return r.pool.Get(ctx, name)
}

// clientNameFor applique la règle de préséance : surcharge de
// l'organisation, puis défaut de l'instance.
func (r *Resolver) clientNameFor(ctx context.Context, role string, orgID model.OrgID) string {
	if orgID != "" {
		name, found, err := r.store.OrgChoice(ctx, string(orgID), role)
		if err != nil {
			// On journalise et on retombe sur le défaut : perdre la
			// surcharge est préférable à perdre la conversation.
			r.logger.WarnContext(ctx, "llmclients: lecture du modèle de l'organisation en échec",
				"org", string(orgID), "role", role, "error", err)
		} else if found && name != "" {
			return name
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.defaults[role]
}

// DefaultFor retourne le client que la configuration attribue à ce rôle,
// sans considérer d'organisation. Sert aux écrans d'administration, pour
// dire à quoi revient l'option « défaut de l'instance ».
func (r *Resolver) DefaultFor(role string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.defaults[role]
}

// Roles retourne les rôles connus du résolveur, c'est-à-dire ceux qu'une
// organisation peut surcharger.
func (r *Resolver) Roles() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	roles := make([]string, 0, len(r.defaults))
	for role := range r.defaults {
		roles = append(roles, role)
	}

	return roles
}
