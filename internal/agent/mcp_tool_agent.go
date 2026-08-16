package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/mcp"
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
	agentName              string
	orgDisplayName         string
	mcpManager             *mcp.Manager
	mcpServerNames         []string
	mcpLimits              mcp.Limits
	maxSequentialToolCalls int
	maxToolContextBytes    int64
	toolsRewriter          ToolsRewriterFunc
}

// ToolsRewriterFunc transforme les outils MCP bruts d'un spécialiste avant
// qu'ils ne soient exposés au modèle, pour un Request donné. Introduit par
// la Phase 13 (agenda) : un spécialiste MCP nu (ex: "research", Phase 12)
// n'a besoin d'aucune transformation, mais le spécialiste agenda doit
// réécrire systématiquement calendar_id (PLAN.md §9.2) et transformer les
// outils d'écriture en un flux proposition/confirmation (PLAN.md §10.1) —
// voir agenda.go. Un rewriter reçoit les outils déjà triés par nom et peut
// retourner un ensemble différent (nombre, noms, schémas) : MCPToolAgent ne
// suppose rien de plus que "une liste d'outils exploitable par le modèle".
type ToolsRewriterFunc func(ctx context.Context, req Request, tools []llm.Tool) ([]llm.Tool, error)

// NewMCPToolAgent construit un MCPToolAgent. mcpServerNames est la liste des
// NOMS de serveurs MCP déclarés par l'agent (agentCfg.MCPServers) : Execute
// n'appellera jamais mcpManager.GetTools pour un autre serveur, ce qui
// garantit qu'un spécialiste n'a jamais accès aux MCP d'un autre spécialiste
// (PLAN.md Phase 11, critère de sortie "un spécialiste peut utiliser
// uniquement ses MCP déclarés").
func NewMCPToolAgent(client llm.ChatCompletionClient, systemPrompt, agentName, orgDisplayName string, mcpManager *mcp.Manager, mcpServerNames []string, mcpLimits mcp.Limits, maxSequentialToolCalls int) *MCPToolAgent {
	return &MCPToolAgent{
		client:                 client,
		systemPrompt:           systemPrompt,
		agentName:              agentName,
		orgDisplayName:         orgDisplayName,
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

	var tools []llm.Tool
	for _, serverName := range a.mcpServerNames {
		serverTools, err := a.mcpManager.GetTools(ctx, sessionKey, serverName, a.mcpLimits)
		if err != nil {
			return Result{}, fmt.Errorf("agent: récupération des outils du serveur mcp %q: %w", serverName, err)
		}
		tools = append(tools, serverTools...)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })

	if a.toolsRewriter != nil {
		rewritten, err := a.toolsRewriter(ctx, req, tools)
		if err != nil {
			return Result{}, fmt.Errorf("agent: réécriture des outils mcp: %w", err)
		}
		tools = rewritten
	}

	messages := buildChatMessages(a.systemPrompt, a.agentName, a.orgDisplayName, req)

	maxIterations := a.maxSequentialToolCalls
	if maxIterations <= 0 {
		maxIterations = 1
	}

	loopResult, err := runToolLoop(ctx, a.client, messages, tools, maxIterations, a.maxToolContextBytes, ErrMaxToolCallsReached)
	if err != nil {
		return Result{}, err
	}

	return Result{Reply: loopResult.Text, References: extractReferences(loopResult.ToolResults)}, nil
}

// WithToolsRewriter attache fn à a : les outils mcp bruts récupérés à
// chaque Execute passent par fn avant d'être exposés au modèle. Retourne a
// pour permettre le chaînage à la construction (voir
// NewAgendaToolAgent). fn nil (comportement par défaut) ne modifie jamais
// les outils, exactement le comportement de MCPToolAgent avant la Phase 13.
func (a *MCPToolAgent) WithToolsRewriter(fn ToolsRewriterFunc) *MCPToolAgent {
	a.toolsRewriter = fn
	return a
}

// WithMaxToolContextBytes borne le cumul des résultats d'outils réinjectés
// dans la conversation durant un tour (PLAN.md §9.4). Une valeur <= 0
// (défaut) laisse ce budget illimité. Retourne a pour permettre le chaînage.
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
