// Package mcp fournit le gestionnaire de connexions aux serveurs MCP
// déclarés dans la configuration (PLAN.md §9, Phase 11).
//
// genai (github.com/bornholm/genai/mcp) expose un client de connexion à un
// serveur MCP donné (Start/Stop/GetTools), mais aucune notion de « session
// applicative » identifiée par une clé métier (conversation, tâche
// planifiée) : voir docs/integration-inventory.md, ligne « MCP (client,
// sessions) ». Manager construit cette notion de session entièrement côté
// automata : une connexion MCP est créée au premier appel de GetTools pour
// un couple (SessionKey, serveur), puis réutilisée pour les appels suivants
// de la même session ; deux sessions distinctes n'obtiennent jamais le même
// client, même pour le même serveur.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bornholm/genai/llm"
	genaimcp "github.com/bornholm/genai/mcp"
	genaihttp "github.com/bornholm/genai/mcp/http"

	"github.com/bornholm/automata/internal/config"
)

// Limits décrit les limites appliquées à l'exécution des outils MCP d'une
// session pour un serveur donné (PLAN.md §9.4). Une valeur nulle pour un
// champ désactive la limite correspondante.
type Limits struct {
	// ToolTimeout borne la durée d'exécution d'un appel d'outil. <= 0 :
	// aucun timeout n'est appliqué au-delà de celui déjà porté par ctx.
	ToolTimeout time.Duration
	// MaxToolResultBytes borne la taille (en octets) du texte d'un résultat
	// d'outil. <= 0 : aucune troncature n'est appliquée.
	MaxToolResultBytes int64
}

// SessionKey identifie une session applicative à laquelle une connexion MCP
// est dédiée : "<provider>:<channelID>" pour une conversation active,
// "<schedule_id>:<scheduled_for>" pour une tâche planifiée (Phase 16+, pas
// encore utilisé dans cette phase). Deux SessionKey distinctes n'obtiennent
// jamais le même client MCP, même pour le même serveur.
type SessionKey string

// Manager gère les connexions aux serveurs MCP déclarés dans la
// configuration, isolées par SessionKey. Une valeur zéro n'est pas
// utilisable : construire via NewManager.
type Manager struct {
	cfg    *config.Config
	logger *slog.Logger

	mu sync.Mutex
	// sessions[sessionKey][serverName] : un client connecté au plus par
	// couple (session, serveur). La création (y compris Start, qui effectue
	// des I/O réseau) est sérialisée sous mu : ce n'est pas la stratégie la
	// plus concurrente possible, mais elle garantit qu'aucune session
	// n'obtient jamais deux clients distincts pour le même serveur, et que
	// deux sessions n'obtiennent jamais le même client — ce qui est la seule
	// garantie exigée par PLAN.md §9.3. Les connexions MCP ne sont pas sur
	// un chemin assez chaud pour que la sérialisation de leur établissement
	// coûte quoi que ce soit d'observable.
	sessions map[SessionKey]map[string]genaimcp.Client
}

// NewManager construit un Manager. logger nil retombe sur slog.Default().
func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		cfg:      cfg,
		logger:   logger,
		sessions: make(map[SessionKey]map[string]genaimcp.Client),
	}
}

// GetTools retourne les outils du serveur MCP nommé serverName pour la
// session sessionKey, en créant/réutilisant une connexion dédiée à cette
// session. Les outils retournés sont enveloppés pour appliquer limits
// (timeout par appel, troncature du résultat signalée au modèle).
func (m *Manager) GetTools(ctx context.Context, sessionKey SessionKey, serverName string, limits Limits) ([]llm.Tool, error) {
	client, err := m.getOrCreateClient(ctx, sessionKey, serverName)
	if err != nil {
		return nil, err
	}

	tools, err := client.GetTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: récupération des outils du serveur %q (session %q): %w", serverName, sessionKey, err)
	}

	wrapped := make([]llm.Tool, len(tools))
	for i, tool := range tools {
		wrapped[i] = m.wrapTool(tool, sessionKey, serverName, limits)
	}

	return wrapped, nil
}

// getOrCreateClient retourne le client MCP existant pour (sessionKey,
// serverName), ou en crée et démarre un nouveau.
func (m *Manager) getOrCreateClient(ctx context.Context, sessionKey SessionKey, serverName string) (genaimcp.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	servers := m.sessions[sessionKey]
	if servers == nil {
		servers = make(map[string]genaimcp.Client)
		m.sessions[sessionKey] = servers
	}

	if client, ok := servers[serverName]; ok {
		return client, nil
	}

	client, err := m.buildClient(serverName)
	if err != nil {
		return nil, err
	}

	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("mcp: connexion au serveur %q (session %q): %w", serverName, sessionKey, err)
	}

	servers[serverName] = client

	m.logger.InfoContext(ctx, "mcp: connexion établie",
		slog.String("server", serverName),
		slog.String("session", string(sessionKey)))

	return client, nil
}

// buildClient construit (sans le démarrer) le client MCP réel pour
// serverName, à partir de cfg.MCPServers[serverName].
//
// Seul le transport "http" est représentable par le schéma de configuration
// actuel (config.MCPServer{Transport, URL, Headers}) : aucun champ n'existe
// pour une commande à exécuter, un support "stdio" serait donc spéculatif.
// Toute autre valeur de Transport retourne une erreur claire.
func (m *Manager) buildClient(serverName string) (genaimcp.Client, error) {
	serverCfg, ok := m.cfg.MCPServers[serverName]
	if !ok {
		return nil, fmt.Errorf("mcp: serveur %q introuvable dans la configuration (mcp_servers)", serverName)
	}

	switch serverCfg.Transport {
	case "http":
		httpClient := &http.Client{
			Transport: &headerRoundTripper{
				headers:   serverCfg.Headers,
				transport: http.DefaultTransport,
			},
		}

		return genaihttp.NewClient(serverCfg.URL, genaihttp.WithHTTPClient(httpClient)), nil
	default:
		return nil, fmt.Errorf("mcp: transport %q non supporté pour le serveur %q (seul \"http\" est représentable par la configuration actuelle)", serverCfg.Transport, serverName)
	}
}

// headerRoundTripper injecte des en-têtes HTTP fixes (déjà résolus par
// config.Load, secrets compris) sur chaque requête. Ces en-têtes ne doivent
// jamais être journalisés (AGENTS.md : « ne pas journaliser les contenus
// privés » ; PLAN.md §9.4 : journaliser uniquement les métadonnées sûres).
type headerRoundTripper struct {
	headers   map[string]string
	transport http.RoundTripper
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
	}

	return t.transport.RoundTrip(req)
}

// wrapTool enveloppe tool pour appliquer limits (timeout, troncature) et
// journaliser des métadonnées sûres (nom du serveur, session, nom de
// l'outil, durée, statut, taille du résultat) — jamais les arguments, le
// contenu du résultat, ni les en-têtes de configuration.
//
// Identité filtrée (PLAN.md §9.3) : l'API réelle de genai/mcp/http.Client
// (mcp/http/options.go) n'expose aucun moyen d'injecter des en-têtes ou
// métadonnées dynamiques par appel d'outil — seuls un *http.Client global et
// un jeton porteur global sont configurables à la construction du client, et
// llm.Tool.Execute(ctx, params) ne transmet rien d'autre que ctx et params
// jusqu'au transport HTTP. Il n'existe donc aucun canal natif pour propager
// org_id/conversation_id/scope jusqu'au serveur MCP par cet appel : on se
// contente ici d'utiliser sessionKey comme identifiant de corrélation dans
// les journaux d'audit, comme le permet effectivement l'API.
func (m *Manager) wrapTool(tool llm.Tool, sessionKey SessionKey, serverName string, limits Limits) llm.Tool {
	toolName := tool.Name()

	execute := func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
		callCtx := ctx
		if limits.ToolTimeout > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, limits.ToolTimeout)
			defer cancel()
		}

		start := time.Now()
		result, err := tool.Execute(callCtx, params)
		duration := time.Since(start)

		if err != nil {
			m.logger.ErrorContext(ctx, "mcp: échec de l'appel d'outil",
				slog.String("server", serverName),
				slog.String("session", string(sessionKey)),
				slog.String("tool", toolName),
				slog.Duration("duration", duration),
				slog.String("status", "error"))

			return nil, fmt.Errorf("mcp: appel de l'outil %q (serveur %q, session %q): %w", toolName, serverName, sessionKey, err)
		}

		text := result.Text()
		originalSize := len(text)
		truncated := false

		if limits.MaxToolResultBytes > 0 && int64(originalSize) > limits.MaxToolResultBytes {
			text = truncateText(text, limits.MaxToolResultBytes)
			text += fmt.Sprintf("\n[résultat tronqué à %d octets]", limits.MaxToolResultBytes)
			truncated = true
		}

		m.logger.InfoContext(ctx, "mcp: appel d'outil terminé",
			slog.String("server", serverName),
			slog.String("session", string(sessionKey)),
			slog.String("tool", toolName),
			slog.Duration("duration", duration),
			slog.String("status", "success"),
			slog.Int("result_bytes", originalSize),
			slog.Bool("truncated", truncated))

		return llm.NewToolResult(text, result.Attachments()...), nil
	}

	return llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)
}

// truncateText tronque s à au plus max octets, sans couper au milieu d'un
// point de code UTF-8.
func truncateText(s string, max int64) string {
	if max <= 0 {
		return ""
	}
	if int64(len(s)) <= max {
		return s
	}

	b := s[:max]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}

	return b
}

// CloseSession ferme toutes les connexions ouvertes pour sessionKey (ex : à
// la fin d'une conversation). Idempotent : appelable plusieurs fois ou sur
// une session inexistante sans erreur.
func (m *Manager) CloseSession(sessionKey SessionKey) error {
	m.mu.Lock()
	servers := m.sessions[sessionKey]
	delete(m.sessions, sessionKey)
	m.mu.Unlock()

	if len(servers) == 0 {
		return nil
	}

	var errs []error
	for serverName, client := range servers {
		if err := client.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("mcp: fermeture du serveur %q (session %q): %w", serverName, sessionKey, err))
		}
	}

	m.logger.Info("mcp: session fermée", slog.String("session", string(sessionKey)))

	return joinErrors(errs)
}

// Close ferme toutes les connexions actives, toutes sessions confondues
// (arrêt du processus). Sûr à appeler plusieurs fois.
func (m *Manager) Close() error {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[SessionKey]map[string]genaimcp.Client)
	m.mu.Unlock()

	var errs []error
	for sessionKey, servers := range sessions {
		for serverName, client := range servers {
			if err := client.Stop(); err != nil {
				errs = append(errs, fmt.Errorf("mcp: fermeture du serveur %q (session %q): %w", serverName, sessionKey, err))
			}
		}
	}

	m.logger.Info("mcp: gestionnaire fermé")

	return joinErrors(errs)
}

// joinErrors agrège errs en une seule erreur (nil si errs est vide), sans
// s'arrêter à la première erreur rencontrée lors d'une fermeture.
func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
