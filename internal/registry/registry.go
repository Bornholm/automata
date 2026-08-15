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

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/mistral"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"

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

	handler, err := buildConversationHandler(ctx, cfg, db)
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
// généraliste (PLAN.md Phase 6) : un GenAIAgent adossé au client LLM et au
// system prompt de l'agent "main", orchestré par un conversation.Handler qui
// persiste l'historique dans db.
func buildConversationHandler(ctx context.Context, cfg *config.Config, db *persistence.DB) (ingress.Handler, error) {
	mainAgent, ok := cfg.Agents[mainAgentName]
	if !ok {
		return nil, fmt.Errorf("agent %q introuvable dans la configuration", mainAgentName)
	}

	llmClientCfg, ok := cfg.LLMClients[mainAgent.Client]
	if !ok {
		return nil, fmt.Errorf("client llm %q (référencé par agents.%s.client) introuvable dans la configuration", mainAgent.Client, mainAgentName)
	}

	client, err := buildLLMClient(ctx, llmClientCfg)
	if err != nil {
		return nil, fmt.Errorf("construction du client llm %q: %w", mainAgent.Client, err)
	}

	genAIAgent := agent.NewGenAIAgent(client, mainAgent.SystemPrompt.Content)

	return conversation.NewHandler(db, genAIAgent, 0), nil
}

// buildLLMClient construit un llm.Client GenAI à partir d'un config.LLMClient
// applicatif. Seuls les providers effectivement enregistrés côté GenAI pour
// la complétion de chat sont supportés (openai, mistral, openrouter) ; voir
// docs/integration-inventory.md §2.
func buildLLMClient(ctx context.Context, cfg config.LLMClient) (llm.Client, error) {
	common := provider.CommonOptions{
		Model:   cfg.Model,
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
	}

	var optFunc provider.OptionFunc

	switch cfg.Provider {
	case "openai":
		optFunc = provider.WithChatCompletion(openai.Name, openai.Options{CommonOptions: common})
	case "mistral":
		optFunc = provider.WithChatCompletion(mistral.Name, mistral.Options{CommonOptions: common})
	case "openrouter":
		optFunc = provider.WithChatCompletion(openrouter.Name, openrouter.Options{CommonOptions: common})
	default:
		return nil, fmt.Errorf("provider llm %q non supporté", cfg.Provider)
	}

	client, err := provider.Create(ctx, optFunc)
	if err != nil {
		return nil, fmt.Errorf("création du client llm (provider %q): %w", cfg.Provider, err)
	}

	return client, nil
}
