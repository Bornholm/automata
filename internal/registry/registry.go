// Package registry assemble les services applicatifs d'Automata
// (configuration, ingress, scheduler, agents, mémoire, persistance) et
// pilote leur cycle de vie.
//
// Depuis la Phase 6, le transport texte est branché sur un agent
// généraliste minimal adossé à GenAI (voir internal/agent,
// internal/conversation) : persistance, résolution d'identité et un
// pipeline ingress par fournisseur Courier déclaré. Les phases suivantes y
// brancheront progressivement les autres composants (délégation, mémoire,
// MCP, scheduler...).
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/persistence"
)

// mainAgentName est l'identifiant conventionnel de l'agent généraliste dans
// cfg.Agents (voir PLAN.md §12 et internal/config/testdata/valid/config.yaml).
const mainAgentName = "main"

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

	handler, err := buildConversationHandler(cfg, db)
	if err != nil {
		return fmt.Errorf("registry: construction de l'agent généraliste: %w", err)
	}

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

// buildConversationHandler construit le ingress.Handler de l'agent
// généraliste "main" (PLAN.md Phase 6, Phase 7) : depuis la Phase 7, la
// construction du client LLM et la composition du system prompt (règles
// invariantes + personnalité configurée + capacités) de CHAQUE agent
// déclaré dans la configuration sont déléguées à agent.NewRegistry
// (internal/agent/registry.go), pas seulement celles de "main". Cette
// fonction se contente d'en extraire l'agent "main" pour le brancher sur un
// conversation.Handler qui persiste l'historique dans db.
func buildConversationHandler(cfg *config.Config, db *persistence.DB) (ingress.Handler, error) {
	agents, err := agent.NewRegistry(cfg)
	if err != nil {
		return nil, fmt.Errorf("construction du registre d'agents: %w", err)
	}

	mainAgent, err := agents.Get(mainAgentName)
	if err != nil {
		return nil, fmt.Errorf("récupération de l'agent %q: %w", mainAgentName, err)
	}

	return conversation.NewHandler(db, mainAgent, 0), nil
}
