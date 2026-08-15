package agent

import (
	"context"
	"fmt"

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/mistral"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"

	"github.com/bornholm/automata/internal/config"
)

// Registry construit et détient un Agent isolé pour chaque agent déclaré
// dans la configuration (PLAN.md §6.2, §7.2, Phase 7). Chaque Agent obtenu
// via Get a son propre client LLM et son propre system prompt composé :
// deux agents construits depuis des configurations différentes ne
// partagent jamais d'état mutable (voir TestNewRegistry_Isolation).
type Registry struct {
	agents map[string]Agent
}

// NewRegistry construit un Registry contenant un GenAIAgent pour CHAQUE
// agent déclaré dans cfg.Agents (pas seulement "main"). cfg doit avoir été
// validée au préalable (config.Validate) : NewRegistry ne revalide pas les
// références croisées (client llm existant, etc.), il les suppose déjà
// correctes et retourne une erreur claire si ce n'est pas le cas.
//
// Les limites par agent (agentCfg.Limits) ne sont pas encore exploitées
// ici : MaxSequentialToolCalls et MaxActionsPerTurn ne prennent sens qu'à
// partir du tool-calling et de la délégation (Phase 8+, PLAN.md §6.3).
// ToolTimeout, MaxToolResultBytes et MaxToolContextBytes concernent eux
// aussi exclusivement les appels d'outils, absents de cette phase (voir
// GenAIAgent.Execute). Elles sont en revanche déjà validées comme
// strictement positives par config.Validate (internal/config/validate.go),
// pour ne pas laisser passer une configuration inutilisable jusqu'à la
// phase qui les consommera.
func NewRegistry(cfg *config.Config) (*Registry, error) {
	agents := make(map[string]Agent, len(cfg.Agents))

	for name, agentCfg := range cfg.Agents {
		llmClientCfg, ok := cfg.LLMClients[agentCfg.Client]
		if !ok {
			return nil, fmt.Errorf("agent: client llm %q (référencé par agents.%s.client) introuvable dans la configuration", agentCfg.Client, name)
		}

		client, err := BuildLLMClient(context.Background(), llmClientCfg)
		if err != nil {
			return nil, fmt.Errorf("agent: construction du client llm %q pour l'agent %q: %w", agentCfg.Client, name, err)
		}

		systemPrompt := BuildSystemPrompt(name, agentCfg)

		// name et agentCfg sont capturés par valeur à chaque itération de
		// boucle (variables déclarées dans le corps du for range), donc
		// chaque GenAIAgent obtient bien sa propre chaîne systemPrompt et
		// son propre client : aucune contamination croisée possible entre
		// agents.
		agents[name] = NewGenAIAgent(client, systemPrompt, name, cfg.Organization.DisplayName)
	}

	return &Registry{agents: agents}, nil
}

// Get retourne l'Agent nommé name. Si aucun agent de ce nom n'a été
// construit par NewRegistry, une erreur explicite est retournée plutôt que
// de paniquer ou de retourner un Agent nil silencieux.
func (r *Registry) Get(name string) (Agent, error) {
	a, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent: agent %q inconnu du registre", name)
	}

	return a, nil
}

// BuildLLMClient construit un llm.Client GenAI à partir d'un
// config.LLMClient applicatif. Seuls les providers effectivement
// enregistrés côté GenAI pour la complétion de chat sont supportés
// (openai, mistral, openrouter) ; voir docs/integration-inventory.md §2.
// Factorisée depuis internal/registry (Phase 6) pour être réutilisée par
// chaque agent construit par NewRegistry, pas seulement "main".
func BuildLLMClient(ctx context.Context, cfg config.LLMClient) (llm.Client, error) {
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
