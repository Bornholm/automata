package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/media"
)

// ErrMaxToolCallsReached est retournée par MCPToolAgent.Execute lorsque le
// plafond d'itérations de la boucle d'appel d'outils
// (agentCfg.Limits.MaxSequentialToolCalls) est atteint sans que le modèle
// n'ait produit de réponse finale sans tool-call. Sentinel distinct de
// ErrMaxDelegationsReached (OrchestratorAgent) : les deux agents partagent
// la même mécanique de boucle (runToolLoop, toolloop.go) mais pas la même
// origine fonctionnelle, et errors.Is doit pouvoir distinguer "boucle de
// délégation" de "boucle d'appel d'outils MCP" côté appelant.
var ErrMaxToolCallsReached = errors.New("agent: plafond d'appels d'outils mcp atteint sans réponse finale du modèle")

// urlPattern extrait des URLs depuis un résultat d'outil MCP quelconque. Une
// regex simple suffit (PLAN.md Phase 12, "normaliser les références
// retournées") : aucun format de réponse MCP spécifique n'est supposé
// puisque le serveur réel appelé par un agent configuré est inconnu du
// code. La ponctuation de fin de phrase usuelle (., ; ) ] etc.) est retirée
// après capture, sans quoi elle serait incluse dans l'URL.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// MCPToolAgent est un agent spécialiste dont les outils sont exclusivement
// ceux des serveurs MCP qu'il déclare (PLAN.md §9, §9.3, Phase 12), par
// opposition à OrchestratorAgent dont les outils sont des délégations vers
// d'autres agents. Les deux réutilisent la même mécanique de boucle de
// tool-calling (runToolLoop, toolloop.go).
//
// La récupération des outils MCP se fait à CHAQUE Execute, jamais à la
// construction (PLAN.md §9.3, "une session MCP distincte par conversation
// active") : agent.Registry construit les agents une seule fois au
// démarrage, avant de connaître aucune conversation, donc mcpManager.
// GetTools doit être appelé par requête, avec une SessionKey dérivée de
// req.Conversation.ID (voir Execute).
type MCPToolAgent struct {
	client                 llm.ChatCompletionClient
	systemPrompt           string
	orgPrompts             map[string]string
	agentName              string
	cfg                    *config.Config
	mcpManager             *mcp.Manager
	mcpServerNames         []string
	mcpLimits              mcp.Limits
	maxSequentialToolCalls int
	maxToolContextBytes    int64
	// extraTools sont des outils applicatifs natifs ajoutés aux outils MCP
	// (generate_image). Fixes à la construction : contrairement aux outils
	// MCP, ils ne dépendent ni de la session ni du principal.
	extraTools []llm.Tool
	logger     *slog.Logger
}

// NewMCPToolAgent construit un MCPToolAgent. mcpServerNames est la liste des
// NOMS de serveurs MCP déclarés par l'agent (agentCfg.MCPServers) : Execute
// n'appellera jamais mcpManager.GetTools pour un autre serveur, ce qui
// garantit qu'un spécialiste n'a jamais accès aux MCP d'un autre spécialiste
// (PLAN.md Phase 11, critère de sortie "un spécialiste peut utiliser
// uniquement ses MCP déclarés").
//
// cfg fournit la politique de chaque serveur : ressource à injecter,
// classification lecture/écriture, domaine de permission. Un seul type
// d'agent MCP suffit donc à couvrir agenda, tâches, météo ou n'importe quel
// autre domaine, sans code dédié (voir applyServerPolicy).
func NewMCPToolAgent(client llm.ChatCompletionClient, systemPrompt, agentName string, cfg *config.Config, mcpManager *mcp.Manager, mcpServerNames []string, mcpLimits mcp.Limits, maxSequentialToolCalls int) *MCPToolAgent {
	return &MCPToolAgent{
		client:                 client,
		systemPrompt:           systemPrompt,
		agentName:              agentName,
		cfg:                    cfg,
		mcpManager:             mcpManager,
		mcpServerNames:         mcpServerNames,
		mcpLimits:              mcpLimits,
		maxSequentialToolCalls: maxSequentialToolCalls,
	}
}

// Execute implémente Agent.
//
// Décision de conception (échec partiel d'un serveur MCP) : si l'un des
// serveurs déclarés échoue à fournir ses outils (serveur indisponible,
// timeout de connexion, transport non supporté...), Execute retourne une
// erreur immédiatement plutôt que de continuer avec les serveurs restants.
// Ce choix est cohérent avec le principe déjà appliqué ailleurs dans ce
// package (ErrEmptyReply, ErrMaxDelegationsReached) : ne jamais deviner une
// réponse à partir d'un état partiel sans le signaler explicitement à
// l'appelant. Un spécialiste qui ne déclare qu'un seul serveur (cas le plus
// courant, ex: "research" avec seulement "internet-search") ne perd de
// toute façon rien à ce choix ; un spécialiste multi-serveurs futur pourra
// nuancer ce comportement si un besoin réel apparaît.
func (a *MCPToolAgent) Execute(ctx context.Context, req Request) (Result, error) {
	sessionKey := mcp.SessionKey(req.Conversation.ID)

	// Les outils d'écriture peuvent produire des actions à confirmer plutôt
	// que de s'exécuter : elles sont collectées ici puis remontées à
	// l'orchestrateur via Result, qui les transforme en plan persisté
	// (internal/action).
	collector := newProposalCollector()
	dedupe := newWriteDedupeSet()

	var tools []llm.Tool

	for _, serverName := range a.mcpServerNames {
		// L'identité vient de l'application, jamais du modèle : elle
		// sélectionne la connexion propre au principal lorsque la
		// configuration en déclare une (identities.principals[].mcp).
		serverTools, err := a.mcpManager.GetToolsFor(ctx, sessionKey, req.Identity.PrincipalID, serverName, a.mcpLimits)
		if err != nil {
			return Result{}, fmt.Errorf("agent: récupération des outils du serveur mcp %q: %w", serverName, err)
		}

		// La politique est appliquée serveur par serveur : un même agent peut
		// déclarer un service en lecture seule et un autre exigeant
		// confirmation, chacun avec sa propre ressource.
		serverCfg, ok := a.cfg.MCPServers[serverName]
		if !ok {
			return Result{}, fmt.Errorf("agent: serveur mcp %q absent de la configuration", serverName)
		}

		serverTools, err = applyServerPolicy(a.cfg, serverName, serverCfg, serverTools, req, collector, a.agentName, dedupe)
		if err != nil {
			return Result{}, err
		}

		tools = append(tools, serverTools...)
	}

	tools = append(tools, a.extraTools...)

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	messages := buildChatMessages(resolveSystemPrompt(a.systemPrompt, a.orgPrompts, req.Identity.OrgID), a.agentName, req)

	maxIterations := a.maxSequentialToolCalls
	if maxIterations <= 0 {
		maxIterations = 1
	}

	// Les outils qui produisent un média destiné à l'utilisateur, et non au
	// modèle, le déposent ici (voir newGenerateImageTool). Le collecteur est
	// créé par tour : les outils, eux, sont partagés par toutes les requêtes.
	mediaCollector := newMediaCollector()

	loopResult, err := runToolLoop(withMediaCollector(ctx, mediaCollector), a.client, messages, tools, maxIterations, a.maxToolContextBytes, ErrMaxToolCallsReached, a.logger, a.agentName)
	if err != nil {
		return Result{}, err
	}

	return Result{
		Reply:           loopResult.Text,
		References:      extractReferences(loopResult.ToolResults),
		ProposedActions: collector.take(),
		Attachments:     append(append([]media.Media(nil), loopResult.Attachments...), mediaCollector.take()...),
	}, nil
}

// WithOrgSystemPrompts remplace le prompt système par organisation : la clé
// est un organizations[].id, la valeur un prompt complet déjà composé (voir
// BuildOrgSystemPrompts). Le prompt du constructeur reste le défaut pour
// toute organisation absente de la map. Retourne a pour permettre le
// chaînage.
func (a *MCPToolAgent) WithOrgSystemPrompts(prompts map[string]string) *MCPToolAgent {
	a.orgPrompts = prompts
	return a
}

// WithMaxToolContextBytes borne le cumul des résultats d'outils réinjectés
// dans la conversation durant un tour (PLAN.md §9.4). Une valeur <= 0
// (défaut) laisse ce budget illimité. Retourne a pour permettre le chaînage.
// WithExtraTools ajoute des outils applicatifs natifs aux outils MCP de cet
// agent. Retourne a pour permettre le chaînage.
func (a *MCPToolAgent) WithExtraTools(tools ...llm.Tool) *MCPToolAgent {
	a.extraTools = append(a.extraTools, tools...)
	return a
}

// WithLogger attache logger à a : même introspection de tour que pour
// OrchestratorAgent.WithLogger (outils, durées, jamais les contenus). nil
// (défaut) désactive.
func (a *MCPToolAgent) WithLogger(logger *slog.Logger) *MCPToolAgent {
	a.logger = logger
	return a
}

func (a *MCPToolAgent) WithMaxToolContextBytes(max int64) *MCPToolAgent {
	a.maxToolContextBytes = max
	return a
}

// extractReferences extrait, déduplique (en conservant l'ordre de première
// apparition) toutes les URLs présentes dans toolResults.
func extractReferences(toolResults []string) []string {
	var refs []string
	seen := make(map[string]bool)

	for _, tr := range toolResults {
		for _, raw := range urlPattern.FindAllString(tr, -1) {
			url := strings.TrimRight(raw, ".,;:!?)]}\"'")
			if url == "" || seen[url] {
				continue
			}
			seen[url] = true
			refs = append(refs, url)
		}
	}

	return refs
}

var _ Agent = &MCPToolAgent{}
