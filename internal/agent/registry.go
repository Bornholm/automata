package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/mistral"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mcp"
)

// calendarMCPServerName est le nom conventionnel du serveur MCP Google
// Calendar déclaré par agents.<nom>.mcp_servers (PLAN.md §9.1, Phase 13) :
// c'est sa présence dans agentCfg.MCPServers qui identifie le spécialiste
// agenda (voir NewRegistryWithMemory ci-dessous).
const calendarMCPServerName = "google-calendar"

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
// ToolTimeout, MaxToolResultBytes et MaxToolContextBytes concernent
// exclusivement les appels d'outils MCP, absents de cette phase. Elles sont
// en revanche déjà validées comme strictement positives par config.Validate
// (internal/config/validate.go), pour ne pas laisser passer une
// configuration inutilisable jusqu'à la phase qui les consommera.
//
// Depuis la Phase 8 (PLAN.md §6.3, §6.4), tout agent déclarant des
// Delegates non vides est construit en deux passes : d'abord un GenAIAgent
// "brut" comme tous les autres (y compris les spécialistes eux-mêmes,
// puisqu'un délégué peut en théorie déléguer à son tour), puis, dans une
// seconde passe, remplacé par un OrchestratorAgent qui expose chaque
// délégué comme un outil LLM ("delegate_to_<nom>") adossé à un
// AgentSpecialist enveloppant le GenAIAgent déjà construit du délégué. Les
// deux passes sont nécessaires : un OrchestratorAgent a besoin que ses
// délégués existent déjà dans le registre pour les envelopper.
func NewRegistry(cfg *config.Config, mcpManager *mcp.Manager) (*Registry, error) {
	return NewRegistryWithMemory(cfg, MemoryTools{}, mcpManager)
}

// NewRegistryWithMemory se comporte comme NewRegistry, mais attache
// memoryTools (search_memory/remember/forget_memory, PLAN.md §8, Phase 10) à
// chaque agent orchestrateur construit, selon les booléens
// agentCfg.Memory.{Search,Remember,Forget} propres à chaque agent déclaré
// dans cfg.Agents. memoryTools.Store et memoryTools.Authorizer sont partagés
// par tous les agents (une seule mémoire, un seul Authorizer par instance,
// voir internal/registry.Run) ; seuls les trois booléens sont recalculés par
// agent.
//
// La valeur zéro de MemoryTools (Store nil) n'expose aucun outil mémoire :
// NewRegistry s'appuie sur ce comportement pour rester utilisable sans
// mémoire câblée (tests, phases antérieures).
//
// Depuis la Phase 12 (PLAN.md §9, §9.3), mcpManager est utilisé pour
// construire un MCPToolAgent (mcp_tool_agent.go) pour chaque agent déclarant
// des MCPServers non vides et aucun Delegates : les Delegates ont priorité
// sur MCPServers si un agent déclarait les deux (choix documenté ci-dessous,
// dans la seconde passe) — un OrchestratorAgent expose des outils de
// délégation, pas des outils MCP, conformément à PLAN.md Phase 12 "le
// généraliste peut déléguer une recherche sans charger les outils
// Internet". mcpManager peut être nil si aucun agent de cfg ne déclare de
// MCPServers (utilisable tel quel par les tests n'exerçant pas cette
// fonctionnalité).
func NewRegistryWithMemory(cfg *config.Config, memoryTools MemoryTools, mcpManager *mcp.Manager) (*Registry, error) {
	agents := make(map[string]Agent, len(cfg.Agents))
	clients := make(map[string]llm.Client, len(cfg.Agents))
	prompts := make(map[string]string, len(cfg.Agents))

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
		clients[name] = client
		prompts[name] = systemPrompt
	}

	for name, agentCfg := range cfg.Agents {
		if len(agentCfg.Delegates) == 0 {
			if len(agentCfg.MCPServers) > 0 {
				limits := mcp.Limits{
					ToolTimeout:        agentCfg.Limits.ToolTimeout.Duration(),
					MaxToolResultBytes: int64(agentCfg.Limits.MaxToolResultBytes.Bytes()),
				}

				// Le spécialiste agenda est identifié par la présence du
				// serveur MCP "google-calendar" parmi ses MCPServers, plutôt
				// que par son nom d'agent : un nom (ex: "agenda") est un
				// choix arbitraire de l'opérateur qui configure agents.yaml,
				// alors que le serveur MCP déclaré décrit sans ambiguïté ce
				// que l'agent peut effectivement faire (PLAN.md Phase 13).
				// Tout autre spécialiste MCP (ex: "research", Phase 12) qui
				// ne déclare pas ce serveur reste un MCPToolAgent nu, sans
				// aucune résolution de ressource ni confirmation.
				if slices.Contains(agentCfg.MCPServers, calendarMCPServerName) {
					agents[name] = NewAgendaToolAgent(
						clients[name],
						prompts[name],
						name,
						cfg.Organization.DisplayName,
						mcpManager,
						agentCfg.MCPServers,
						limits,
						agentCfg.Limits.MaxSequentialToolCalls,
						cfg,
						nil,
					)
				} else {
					agents[name] = NewMCPToolAgent(
						clients[name],
						prompts[name],
						name,
						cfg.Organization.DisplayName,
						mcpManager,
						agentCfg.MCPServers,
						limits,
						agentCfg.Limits.MaxSequentialToolCalls,
					)
				}
			}

			continue
		}

		specialists := make(map[string]delegation.Specialist, len(agentCfg.Delegates))
		for _, delegateName := range agentCfg.Delegates {
			delegateAgent, ok := agents[delegateName]
			if !ok {
				// config.Validate rejette déjà les délégués inconnus avant
				// d'atteindre NewRegistry ; ce cas ne devrait donc jamais se
				// produire en usage normal (cfg validée). On le refuse tout
				// de même explicitement plutôt que de paniquer sur un accès
				// map invalide silencieux, cohérent avec le principe de ne
				// jamais faire confiance implicitement à cfg (voir le
				// commentaire de package).
				return nil, fmt.Errorf("agent: délégué %q (référencé par agents.%s.delegates) introuvable dans le registre", delegateName, name)
			}

			specialists[delegateName] = NewAgentSpecialist(delegateName, delegateAgent)
		}

		orchestrator := NewOrchestratorAgent(clients[name], prompts[name], name, cfg.Organization.DisplayName, specialists, agentCfg.Limits.MaxSequentialToolCalls)

		agentMemoryTools := memoryTools
		agentMemoryTools.Search = memoryTools.Search && agentCfg.Memory.Search
		agentMemoryTools.Remember = memoryTools.Remember && agentCfg.Memory.Remember
		agentMemoryTools.Forget = memoryTools.Forget && agentCfg.Memory.Forget

		agents[name] = orchestrator.WithMemoryTools(agentMemoryTools)
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

// BuildTranscriptionClient construit un llm.TranscriptionClient GenAI à
// partir d'un config.LLMClient applicatif, pour le traitement audio (PLAN.md
// §3.4, Phase 9). Mêmes providers supportés que BuildLLMClient (openai,
// mistral, openrouter) ; voir docs/integration-inventory.md §2.
func BuildTranscriptionClient(ctx context.Context, cfg config.LLMClient) (llm.TranscriptionClient, error) {
	common := provider.CommonOptions{
		Model:   cfg.Model,
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
	}

	var optFunc provider.OptionFunc

	switch cfg.Provider {
	case "openai":
		optFunc = provider.WithTranscription(openai.Name, openai.Options{CommonOptions: common})
	case "mistral":
		optFunc = provider.WithTranscription(mistral.Name, mistral.Options{CommonOptions: common})
	case "openrouter":
		optFunc = provider.WithTranscription(openrouter.Name, openrouter.Options{CommonOptions: common})
	default:
		return nil, fmt.Errorf("provider llm %q non supporté pour la transcription", cfg.Provider)
	}

	client, err := provider.Create(ctx, optFunc)
	if err != nil {
		return nil, fmt.Errorf("création du client de transcription (provider %q): %w", cfg.Provider, err)
	}

	return client, nil
}
