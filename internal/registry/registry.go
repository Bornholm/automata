// Package registry assemble les services applicatifs d'Automata
// (configuration, ingress, scheduler, agents, mémoire, persistance) et
// pilote leur cycle de vie.
//
// À ce stade (Phase 5), seul le transport texte est câblé : persistance,
// résolution d'identité et un pipeline ingress par fournisseur Courier
// déclaré. Les phases suivantes y brancheront progressivement les autres
// composants (agent, mémoire, scheduler...).
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/persistence"
)

// fixedReply est la réponse envoyée par le handler de la Phase 5 : aucun
// appel LLM n'est encore branché (voir PLAN.md, Phase 6).
const fixedReply = "Message bien reçu."

// Run démarre les services applicatifs et bloque jusqu'à l'annulation du
// contexte fourni, puis s'arrête proprement. cfg doit avoir été chargée et
// validée (voir config.Load).
func Run(ctx context.Context, logger *slog.Logger, cfg *config.Config) error {
	logger.InfoContext(ctx, "automata starting")

	db, err := persistence.Open(ctx, cfg.Storage.Application)
	if err != nil {
		return fmt.Errorf("registry: ouverture de la persistance: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.ErrorContext(ctx, "registry: échec de la fermeture de la persistance", "error", err)
		}
	}()

	resolver, err := identity.NewResolver(cfg)
	if err != nil {
		return fmt.Errorf("registry: construction du résolveur d'identité: %w", err)
	}

	providers, err := buildCourierProviders(cfg)
	if err != nil {
		return fmt.Errorf("registry: construction des fournisseurs courier: %w", err)
	}

	handler := ingress.FixedReplyHandler{Reply: fixedReply}

	var wg sync.WaitGroup

	for name, provider := range providers {
		pipeline := ingress.NewPipeline(name, provider, resolver, db, handler, logger)

		wg.Add(1)
		go func(name string, pipeline *ingress.Pipeline) {
			defer wg.Done()

			if err := pipeline.Run(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "registry: pipeline ingress arrêté en erreur", "provider", name, "error", err)
			}
		}(name, pipeline)
	}

	<-ctx.Done()

	logger.InfoContext(ctx, "automata stopping")

	wg.Wait()

	return nil
}

// buildCourierProviders construit un courier.Provider réel pour chaque
// fournisseur déclaré dans cfg.Courier.Providers. Seul le type "whatsapp"
// est supporté à ce stade (voir PLAN.md, Phase 5).
func buildCourierProviders(cfg *config.Config) (map[string]courier.Provider, error) {
	providers := make(map[string]courier.Provider, len(cfg.Courier.Providers))

	for name, cp := range cfg.Courier.Providers {
		switch cp.Type {
		case "whatsapp":
			sessionPath, ok := cp.Extra["session_path"].(string)
			if !ok || sessionPath == "" {
				return nil, fmt.Errorf("fournisseur courier %q: champ session_path requis et non vide", name)
			}

			providers[name] = whatsapp.NewProvider(whatsapp.WithDBPath(sessionPath))
		default:
			return nil, fmt.Errorf("fournisseur courier %q: type %q non supporté", name, cp.Type)
		}
	}

	return providers, nil
}
