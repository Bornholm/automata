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
	"maps"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bornholm/genai/llm"
	genaimcp "github.com/bornholm/genai/mcp"
	genaihttp "github.com/bornholm/genai/mcp/http"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
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

	metrics *observability.Metrics

	// baseCtx borne la durée de vie des connexions MCP mises en cache, et
	// n'est annulé que par Close.
	//
	// Il ne faut SURTOUT pas démarrer un client avec le contexte de la
	// requête qui l'a fait naître : ce contexte est annulé dès la fin du
	// traitement du message (voir le timeout de internal/ingress), ce qui
	// ferme la connexion sous-jacente alors qu'elle reste référencée ici.
	// L'appel suivant réutiliserait un client déjà mort ("client is
	// closing"). Le cas se produit dès qu'une session sert à plus d'un
	// message — typiquement une action agenda ou todo proposée dans un
	// message et exécutée après confirmation dans un autre.
	//
	// Chaque appel d'outil, lui, reste borné par le contexte de sa propre
	// requête (voir wrapTool) : découpler la durée de vie de la connexion ne
	// rend pas les appels insensibles à l'annulation.
	baseCtx    context.Context
	cancelBase context.CancelFunc

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

	baseCtx, cancelBase := context.WithCancel(context.Background())

	return &Manager{
		cfg:        cfg,
		logger:     logger,
		baseCtx:    baseCtx,
		cancelBase: cancelBase,
		sessions:   make(map[SessionKey]map[string]genaimcp.Client),
	}
}

// WithMetrics attache metrics à m : chaque appel d'outil MCP (succès ou
// erreur) et chaque troncature de résultat sont comptabilisés dès le
// prochain appel (PLAN.md §14.3, Phase 20). metrics nil désactive
// l'observation (comportement par défaut de NewManager). Retourne m pour
// permettre le chaînage à la construction.
func (m *Manager) WithMetrics(metrics *observability.Metrics) *Manager {
	m.metrics = metrics
	return m
}

// GetTools retourne les outils du serveur MCP nommé serverName pour la
// session sessionKey, en créant/réutilisant une connexion dédiée à cette
// session. Les outils retournés sont enveloppés pour appliquer limits
// (timeout par appel, troncature du résultat signalée au modèle).
//
// Équivaut à GetToolsFor sans principal : la connexion utilise la
// configuration du serveur telle quelle.
func (m *Manager) GetTools(ctx context.Context, sessionKey SessionKey, serverName string, limits Limits) ([]llm.Tool, error) {
	return m.GetToolsFor(ctx, sessionKey, "", serverName, limits)
}

// GetToolsFor retourne les outils du serveur serverName pour la session
// sessionKey, en appliquant la connexion propre à principalID lorsque la
// configuration en déclare une (identities.principals[].mcp, PLAN.md Phase 11
// point 5 : « injecter une identité filtrée »).
//
// Isolation : dès qu'un principal dispose de sa propre connexion, la clé de
// cache inclut son identifiant. Deux personnes d'un même canal de groupe ne
// partagent donc jamais une connexion authentifiée, ce qui interdit à l'une
// d'atteindre les ressources de l'autre avec son jeton. En l'absence de
// surcharge, la clé reste celle de la conversation : une seule connexion
// partagée, comme avant.
//
// principalID vide, ou principal sans surcharge pour ce serveur, redonne le
// comportement d'origine.
func (m *Manager) GetToolsFor(ctx context.Context, sessionKey SessionKey, principalID model.PrincipalID, serverName string, limits Limits) ([]llm.Tool, error) {
	override, hasOverride := m.overrideFor(principalID, serverName)

	cacheKey := sessionKey
	if hasOverride {
		cacheKey = SessionKey(string(sessionKey) + "|" + string(principalID))
	}

	client, err := m.getOrCreateClient(ctx, cacheKey, serverName, override)
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

// overrideFor retourne la surcharge de connexion déclarée par principalID
// pour serverName, s'il en existe une.
func (m *Manager) overrideFor(principalID model.PrincipalID, serverName string) (config.MCPOverride, bool) {
	if principalID == "" || m.cfg == nil {
		return config.MCPOverride{}, false
	}

	for _, principal := range m.cfg.Identities.Principals {
		if principal.ID != string(principalID) {
			continue
		}

		override, ok := principal.MCP[serverName]

		return override, ok
	}

	return config.MCPOverride{}, false
}

// getOrCreateClient retourne le client MCP existant pour (sessionKey,
// serverName), ou en crée et démarre un nouveau.
func (m *Manager) getOrCreateClient(ctx context.Context, sessionKey SessionKey, serverName string, override config.MCPOverride) (genaimcp.Client, error) {
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

	client, err := m.buildClient(serverName, override)
	if err != nil {
		return nil, err
	}

	// baseCtx, jamais ctx : voir le commentaire du champ Manager.baseCtx.
	if err := client.Start(m.baseCtx); err != nil {
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
// override, lorsqu'elle est renseignée, remplace l'URL et complète les
// en-têtes du serveur avec ceux du principal courant.
func (m *Manager) buildClient(serverName string, override config.MCPOverride) (genaimcp.Client, error) {
	serverCfg, ok := m.cfg.MCPServers[serverName]
	if !ok {
		return nil, fmt.Errorf("mcp: serveur %q introuvable dans la configuration (mcp_servers)", serverName)
	}

	switch serverCfg.Transport {
	case "http":
		url := serverCfg.URL
		if override.URL != "" {
			url = override.URL
		}

		// Les en-têtes du principal l'emportent sur ceux du serveur, ce qui
		// permet de ne surcharger que l'autorisation sans réécrire le reste.
		headers := make(map[string]string, len(serverCfg.Headers)+len(override.Headers))
		maps.Copy(headers, serverCfg.Headers)
		maps.Copy(headers, override.Headers)

		httpClient := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				transport: http.DefaultTransport,
			},
		}

		return genaihttp.NewClient(url, genaihttp.WithHTTPClient(httpClient)), nil
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
			m.metrics.IncMCPCall(serverName, toolName, err)

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
			m.metrics.IncToolResultTruncated()
		}

		m.metrics.IncMCPCall(serverName, toolName, nil)

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

	wrapped := llm.NewFuncTool(tool.Name(), tool.Description(), tool.Parameters(), execute)

	// Ce wrapper reconstruit l'outil pour appliquer timeout et troncature : il
	// perdrait l'annotation readOnlyHint reçue du serveur si on ne la
	// reportait pas explicitement ici.
	if annotated, ok := tool.(llm.AnnotatedTool); ok {
		if readOnly, known := annotated.ReadOnly(); known {
			wrapped = wrapped.WithReadOnlyHint(readOnly)
		}
	}

	return wrapped
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

	// Annule le contexte qui borne la durée de vie des connexions : c'est ce
	// qui libère effectivement les goroutines de transport, Stop ci-dessous
	// ne suffisant pas à lui seul.
	m.cancelBase()

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
