package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/circuitbreaker"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/mistral"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"
	"github.com/bornholm/genai/llm/retry"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/observability"
	"github.com/bornholm/automata/internal/usage"
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
	return NewRegistryWithMemory(cfg, MemoryTools{}, ReminderTools{}, ProfileTools{}, nil, mcpManager, nil, nil, nil)
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
// reminderTools (create_reminder/list_reminders/cancel_reminder) suit le
// même schéma que memoryTools : partagé par tous les orchestrateurs, mais
// attaché seulement aux agents déclarant reminders: true. Sa valeur zéro
// (DB nil) n'expose aucun outil.
// logger, s'il n'est pas nil, active l'introspection des tours sur chaque
// agent construit (voir OrchestratorAgent.WithLogger) : outils exposés,
// appels d'outils et délégations, durées — jamais les contenus.
func NewRegistryWithMemory(cfg *config.Config, memoryTools MemoryTools, reminderTools ReminderTools, profileTools ProfileTools, customizer OrgCustomizer, mcpManager *mcp.Manager, pluginProvider PluginSpecialistProvider, metrics *observability.Metrics, logger *slog.Logger) (*Registry, error) {
	agents := make(map[string]Agent, len(cfg.Agents))
	clients := make(map[string]llm.Client, len(cfg.Agents))
	prompts := make(map[string]string, len(cfg.Agents))
	// orgPrompts porte, par agent, ses variantes de prompt par organisation
	// (system_prompt.org_overrides) ; nil pour un agent sans surcharge.
	orgPrompts := make(map[string]map[string]string, len(cfg.Agents))

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
		orgPrompts[name] = BuildOrgSystemPrompts(name, agentCfg)

		// name et agentCfg sont capturés par valeur à chaque itération de
		// boucle (variables déclarées dans le corps du for range), donc
		// chaque GenAIAgent obtient bien sa propre chaîne systemPrompt et
		// son propre client : aucune contamination croisée possible entre
		// agents.
		agents[name] = NewGenAIAgent(client, systemPrompt, name).
			WithOrgSystemPrompts(orgPrompts[name]).
			WithVision(llmClientCfg.SupportsVision())
		clients[name] = client
		prompts[name] = systemPrompt
	}

	// Les spécialistes sont construits AVANT les orchestrateurs, en deux
	// passes distinctes, parce qu'un orchestrateur capture ses délégués tels
	// qu'ils sont dans agents au moment où il est construit. Les traiter
	// dans une seule boucle sur cfg.Agents rendrait le résultat dépendant de
	// l'ordre d'itération de la map, qui est aléatoire en Go : selon les
	// démarrages, l'orchestrateur déléguerait tantôt au spécialiste équipé
	// de ses outils MCP, tantôt à l'agent nu de la première passe — qui
	// répond de mémoire au lieu de chercher, sans qu'aucune erreur ne le
	// signale.
	for name, agentCfg := range cfg.Agents {
		if agentCfg.Type == config.AgentTypeOrchestrator {
			continue
		}

		if len(agentCfg.MCPServers) > 0 || agentCfg.ImageGeneration.Client != "" {
			limits := mcp.Limits{
				ToolTimeout:        agentCfg.Limits.ToolTimeout.Duration(),
				MaxToolResultBytes: int64(agentCfg.Limits.MaxToolResultBytes.Bytes()),
			}

			// Outils applicatifs natifs du spécialiste, hors MCP.
			var extraTools []llm.Tool
			if clientName := agentCfg.ImageGeneration.Client; clientName != "" {
				generator, err := BuildImageGenerationClient(context.Background(), cfg.ImageClients[clientName])
				if err != nil {
					return nil, fmt.Errorf("agent: construction du client d'images %q pour l'agent %q: %w", clientName, name, err)
				}
				extraTools = append(extraTools, newGenerateImageTool(generator))
			}

			// Un seul type de spécialiste MCP, quel que soit le domaine.
			// Ce qui distingue un agenda d'une recherche web est déclaré
			// sous mcp_servers.<nom> : ressource à injecter, outils
			// exigeant confirmation, domaine de permission. Le registre
			// n'a donc aucun nom de service à connaître.
			specialist := NewMCPToolAgent(
				clients[name],
				prompts[name],
				name,
				cfg,
				mcpManager,
				agentCfg.MCPServers,
				limits,
				agentCfg.Limits.MaxSequentialToolCalls,
			)

			agents[name] = specialist.
				WithOrgSystemPrompts(orgPrompts[name]).
				WithVision(cfg.LLMClients[agentCfg.Client].SupportsVision()).
				WithExtraTools(extraTools...).
				WithMaxToolContextBytes(int64(agentCfg.Limits.MaxToolContextBytes.Bytes())).
				WithLogger(logger)
		}
	}

	for name, agentCfg := range cfg.Agents {
		// Le branchement se fait sur le TYPE déclaré, jamais sur la présence
		// de délégués : un orchestrateur sans aucun délégué (configuration
		// minimale, spécialistes commentés) doit tout de même recevoir ses
		// outils propres — mémoire, rappels — sinon les drapeaux memory/
		// reminders de sa configuration seraient silencieusement ignorés.
		if agentCfg.Type != config.AgentTypeOrchestrator {
			continue
		}

		specialists := make(map[string]delegation.Specialist, len(agentCfg.Delegates))
		specialistDescriptions := make(map[string]string, len(agentCfg.Delegates))
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
			// Ce que sait faire le délégué, tel qu'il le déclare : c'est
			// cette phrase que lira le modèle sur l'outil de délégation.
			if delegateCfg, ok := cfg.Agents[delegateName]; ok && delegateCfg.Description != "" {
				specialistDescriptions[delegateName] = delegateCfg.Description
			}
		}

		orchestrator := NewOrchestratorAgent(clients[name], prompts[name], name, specialists, agentCfg.Limits.MaxSequentialToolCalls).
			WithOrgSystemPrompts(orgPrompts[name]).
			WithVision(cfg.LLMClients[agentCfg.Client].SupportsVision()).
			WithSpecialistDescriptions(specialistDescriptions)

		agentMemoryTools := memoryTools
		agentMemoryTools.Search = memoryTools.Search && agentCfg.Memory.Search
		agentMemoryTools.Remember = memoryTools.Remember && agentCfg.Memory.Remember
		agentMemoryTools.Forget = memoryTools.Forget && agentCfg.Memory.Forget
		agentMemoryTools.History = memoryTools.History && agentCfg.Memory.History
		agentMemoryTools.Recall = memoryTools.Recall && agentCfg.Memory.Recall

		agentReminderTools := ReminderTools{}
		if agentCfg.Reminders {
			agentReminderTools = reminderTools
			agentReminderTools.AgentName = name
			// Les outils de tâches ne s'ajoutent que si l'agent les déclare
			// ET qu'un exécuteur est câblé (reminderTools.Tasks) : promettre
			// un travail que rien n'exécutera serait pire que ne pas le
			// proposer.
			agentReminderTools.Tasks = reminderTools.Tasks && agentCfg.ScheduledTasks
		}

		// L'outil de profil suit le même schéma : capacité globale de
		// l'instance recroisée avec le drapeau de chaque agent.
		agentProfileTools := profileTools
		agentProfileTools.Enabled = profileTools.Enabled && agentCfg.ProfileLink

		agents[name] = orchestrator.
			WithPluginSpecialists(pluginProvider).
			WithMemoryTools(agentMemoryTools).
			WithReminderTools(agentReminderTools).
			WithProfileTools(agentProfileTools).
			WithOrgCustomizer(customizer).
			WithMaxActionsPerTurn(agentCfg.Limits.MaxActionsPerTurn).
			WithMaxToolContextBytes(int64(agentCfg.Limits.MaxToolContextBytes.Bytes())).
			WithMetrics(metrics).
			WithLogger(logger)
	}

	return &Registry{agents: agents}, nil
}

// NewRegistryFromAgents construit un Registry directement à partir d'agents
// déjà construits, sans passer par la configuration YAML ni construire de
// client LLM réel. Utilisé par les tests d'autres packages (ex :
// internal/scheduler) pour injecter un Agent factice via Get.
func NewRegistryFromAgents(agents map[string]Agent) *Registry {
	return &Registry{agents: agents}
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

// Paramètres de résilience appliqués à chaque client LLM construit. Un 429
// ou un 5xx ponctuel du fournisseur ne doit jamais faire échouer un tour :
// llm.IsRetryable classifie déjà ces statuts, et retry.Client réessaie avec
// backoff exponentiel (1 s, 2 s, 4 s), y compris à l'ouverture d'un stream.
// Le circuit breaker, posé PAR-DESSUS le retry (comme dans
// genai/examples/resilient), compte donc un échec après épuisement des
// tentatives : au-delà de breakerMaxFailures échecs consécutifs, il coupe
// court pendant breakerResetTimeout au lieu de faire attendre chaque message
// suivant tout le cycle de retries contre un fournisseur manifestement en
// panne. Le total retryBaseDelay×(2⁰+2¹+2²) = 7 s reste très en deçà du
// handleTimeout de 5 min du pipeline d'ingress.
const (
	retryBaseDelay      = 1 * time.Second
	retryMaxAttempts    = 3
	breakerMaxFailures  = 5
	breakerResetTimeout = 30 * time.Second
)

// wrapResilience enveloppe client dans les middlewares genai de retry puis de
// circuit breaker. Chaque client construit reçoit son propre breaker : la
// panne d'un fournisseur n'ouvre jamais le circuit d'un autre.
//
// Limite connue du retry.Client : une erreur retryable survenant EN MILIEU de
// stream rouvre le stream en réémettant tout depuis le début, dupliquant les
// chunks déjà transmis. En pratique les erreurs mi-stream sont des erreurs
// d'entrée/sortie, non retryables selon llm.IsRetryable (429/5xx uniquement) ;
// seul l'échec d'OUVERTURE est réessayé, sans réémission possible.
func wrapResilience(client llm.Client) llm.Client {
	retried := retry.NewClient(client, retryBaseDelay, retryMaxAttempts)
	return circuitbreaker.NewClient(retried, breakerMaxFailures, breakerResetTimeout)
}

// BuildLLMClient construit un llm.Client GenAI à partir d'un
// config.LLMClient applicatif. Seuls les providers effectivement
// enregistrés côté GenAI pour la complétion de chat sont supportés
// (openai, mistral, openrouter) ; voir docs/integration-inventory.md §2.
// Factorisée depuis internal/registry (Phase 6) pour être réutilisée par
// chaque agent construit par NewRegistry, pas seulement "main". Le client
// retourné est systématiquement enveloppé par wrapResilience.
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

	reasoning, err := reasoningOptions(cfg.Reasoning)
	if err != nil {
		return nil, fmt.Errorf("client llm (provider %q): %w", cfg.Provider, err)
	}

	// Le décorateur d'usage est le plus externe : il enregistre la réponse
	// effectivement obtenue, après retry — une trace par appel réussi, pas
	// une par tentative (voir internal/usage).
	return usage.WrapClient(wrapResilience(withExtraFields(withReasoning(client, reasoning), cfg.ExtraFields)), cfg.Provider, cfg.Model), nil
}

// BuildEmbeddingsClient construit un llm.Client GenAI limité aux
// embeddings, à partir d'un config.LLMClient applicatif (ex: provider
// "mistral", model "mistral-embed"). Retourne un llm.Client complet plutôt
// qu'un llm.EmbeddingsClient car l'index sémantique amoxtli
// (index/sqlitevec.NewIndexAtPath) attend cette interface — seule la
// méthode Embeddings est effectivement câblée.
func BuildEmbeddingsClient(ctx context.Context, cfg config.LLMClient) (llm.Client, error) {
	common := provider.CommonOptions{
		Model:   cfg.Model,
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
	}

	var optFunc provider.OptionFunc

	switch cfg.Provider {
	case "openai":
		optFunc = provider.WithEmbeddings(openai.Name, openai.Options{CommonOptions: common})
	case "mistral":
		optFunc = provider.WithEmbeddings(mistral.Name, mistral.Options{CommonOptions: common})
	case "openrouter":
		optFunc = provider.WithEmbeddings(openrouter.Name, openrouter.Options{CommonOptions: common})
	default:
		return nil, fmt.Errorf("provider d'embeddings %q non supporté", cfg.Provider)
	}

	client, err := provider.Create(ctx, optFunc)
	if err != nil {
		return nil, fmt.Errorf("création du client d'embeddings (provider %q): %w", cfg.Provider, err)
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

	// Même résilience que la complétion de chat : retry.Client et
	// circuitbreaker.Client implémentent llm.Client entier, Transcription
	// comprise. Même comptabilité d'usage que la complétion, aussi.
	return usage.WrapClient(wrapResilience(client), cfg.Provider, cfg.Model), nil
}
