// Package mcp fournit le gestionnaire de connexions aux serveurs MCP
// déclarés dans la configuration (plan de conception, §9, Phase 11).
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
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bornholm/genai/llm"
	genaimcp "github.com/bornholm/genai/mcp"
	genaihttp "github.com/bornholm/genai/mcp/http"
	genaistdio "github.com/bornholm/genai/mcp/stdio"
	genaistreamable "github.com/bornholm/genai/mcp/streamable"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
)

// Limits décrit les limites appliquées à l'exécution des outils MCP d'une
// session pour un serveur donné (plan de conception, §9.4). Une valeur nulle pour un
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
	// garantie exigée par plan de conception, §9.3. Les connexions MCP ne sont pas sur
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
// prochain appel (plan de conception, §14.3, Phase 20). metrics nil désactive
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
// configuration en déclare une (identities.principals[].mcp, plan de conception, Phase 11
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

		// Serveur stdio surchargé : la connexion est un processus enfant
		// lancé avec les identifiants du principal. La frontière de sécurité
		// est donc le principal, pas la conversation — partager ce processus
		// entre toutes les sessions du même principal est sûr (chaque appel
		// arrive déjà authentifié comme lui) et borne le nombre de processus
		// à un par (principal, serveur), au lieu d'un par conversation
		// active. Les serveurs http conservent la clé par session : une
		// connexion HTTP ne coûte rien et la garantie historique de §9.3
		// reste inchangée pour eux.
		if server, ok := m.cfg.MCPServers[serverName]; ok && server.Transport == "stdio" {
			cacheKey = SessionKey("principal|" + string(principalID))
		}
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
		return nil, fmt.Errorf("mcp: connexion au serveur %q (session %q)%s: %w",
			serverName, sessionKey, authHint(serverName, m.cfg.MCPServers[serverName], err), err)
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
// Transports supportés : "http" (HTTP+SSE, révision 2024-11-05 du protocole),
// "streamable-http" (révision 2025-03-26 et suivantes) et "stdio" (commande
// locale). Les deux transports HTTP se configurent de la même façon — URL et
// en-têtes, éventuellement surchargés par principal — et ne diffèrent que par
// le protocole parlé sur le fil ; un serveur ne parle en général que l'un des
// deux. Sur "stdio", ce sont les arguments et l'environnement qui portent les
// patrons {{nom}} résolus par les values du principal courant (voir
// config.MCPOverride). Toute autre valeur de Transport retourne une erreur
// claire.
func (m *Manager) buildClient(serverName string, override config.MCPOverride) (genaimcp.Client, error) {
	serverCfg, ok := m.cfg.MCPServers[serverName]
	if !ok {
		return nil, fmt.Errorf("mcp: serveur %q introuvable dans la configuration (mcp_servers)", serverName)
	}

	switch serverCfg.Transport {
	case "http":
		url, headers, err := renderHTTPConfig(serverCfg, override)
		if err != nil {
			return nil, fmt.Errorf("mcp: serveur %q: %w", serverName, err)
		}

		httpClient := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				transport: http.DefaultTransport,
			},
		}

		return genaihttp.NewClient(url, genaihttp.WithHTTPClient(httpClient)), nil
	case "streamable-http":
		url, headers, err := renderHTTPConfig(serverCfg, override)
		if err != nil {
			return nil, fmt.Errorf("mcp: serveur %q: %w", serverName, err)
		}

		httpClient := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				transport: http.DefaultTransport,
			},
		}

		return genaistreamable.NewClient(url, genaistreamable.WithHTTPClient(httpClient)), nil
	case "stdio":
		command, env, err := renderStdioCommand(serverCfg, override)
		if err != nil {
			return nil, fmt.Errorf("mcp: serveur %q: %w", serverName, err)
		}

		return genaistdio.NewClient(command, genaistdio.WithEnv(env...)), nil
	default:
		return nil, fmt.Errorf("mcp: transport %q non supporté pour le serveur %q (transports: %s)", serverCfg.Transport, serverName, config.SupportedMCPTransports)
	}
}

// renderStdioCommand résout les patrons {{nom}} de la commande et de
// l'environnement du serveur stdio avec les values du principal courant, et
// construit l'environnement complet du processus enfant (celui du worker,
// complété par les variables déclarées — ces dernières l'emportent, étant
// ajoutées en dernier).
//
// Un patron sans valeur est une erreur, jamais un passage tel quel : lancer
// une commande avec un {{host}} littéral au mieux échouerait de façon
// obscure, au pire se connecterait ailleurs que prévu. L'erreur ne cite que
// les NOMS de patrons manquants — les valeurs, elles, sont des secrets
// potentiels et ne sont jamais journalisées (AGENTS.md).
func renderStdioCommand(serverCfg config.MCPServer, override config.MCPOverride) ([]string, []string, error) {
	// Défense en profondeur : config.Validate refuse déjà un serveur stdio
	// sans commande, mais le client genai paniquerait sur une commande vide
	// — un garde local coûte une ligne et évite un crash du worker si un
	// chemin de construction contourne un jour la validation.
	if len(serverCfg.Command) == 0 {
		return nil, nil, fmt.Errorf("commande vide pour un transport stdio")
	}

	var missing []string

	command := make([]string, len(serverCfg.Command))
	for i, arg := range serverCfg.Command {
		rendered, miss := config.RenderMCPTemplate(arg, override.Values)
		command[i] = rendered
		missing = append(missing, miss...)
	}

	env := os.Environ()
	for _, key := range slices.Sorted(maps.Keys(serverCfg.Env)) {
		rendered, miss := config.RenderMCPTemplate(serverCfg.Env[key], override.Values)
		missing = append(missing, miss...)
		env = append(env, key+"="+rendered)
	}

	if err := errMissingPlaceholders(missing); err != nil {
		return nil, nil, err
	}

	return command, env, nil
}

// renderHTTPConfig résout les patrons {{nom}} de la configuration http
// effective (URL du principal si elle remplace celle du serveur, en-têtes
// fusionnés — voir config.MCPServer.EffectiveHTTPConfig) avec les values du
// principal courant. Même contrat qu'en stdio : un patron sans valeur est
// une erreur citant les noms manquants, jamais les valeurs.
func renderHTTPConfig(serverCfg config.MCPServer, override config.MCPOverride) (string, map[string]string, error) {
	var missing []string

	url, headers := serverCfg.EffectiveHTTPConfig(override)

	url, miss := config.RenderMCPTemplate(url, override.Values)
	missing = append(missing, miss...)

	for _, key := range slices.Sorted(maps.Keys(headers)) {
		rendered, miss := config.RenderMCPTemplate(headers[key], override.Values)
		missing = append(missing, miss...)
		headers[key] = rendered
	}

	if err := errMissingPlaceholders(missing); err != nil {
		return "", nil, err
	}

	return url, headers, nil
}

// errMissingPlaceholders transforme une liste de noms de patrons non
// résolus en erreur unique, dédupliquée et triée. nil si la liste est vide.
// Seuls les NOMS apparaissent : les valeurs sont des secrets potentiels et
// ne sont jamais journalisées (AGENTS.md).
func errMissingPlaceholders(missing []string) error {
	if len(missing) == 0 {
		return nil
	}

	slices.Sort(missing)
	missing = slices.Compact(missing)

	return fmt.Errorf("patrons sans valeur pour le principal courant ({{%s}}) : serveur indisponible sans surcharge identities.principals[].mcp", strings.Join(missing, "}}, {{"))
}

// headerRoundTripper injecte des en-têtes HTTP fixes (déjà résolus par
// config.Load, secrets compris) sur chaque requête. Ces en-têtes ne doivent
// jamais être journalisés (AGENTS.md : « ne pas journaliser les contenus
// privés » ; plan de conception, §9.4 : journaliser uniquement les métadonnées sûres).
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
// Identité filtrée (plan de conception, §9.3) : l'API réelle de genai/mcp/http.Client
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

// authHint ajoute la cause probable d'un refus d'authentification. Le
// serveur, lui, se contente d'un « Unauthorized » qui ne dit ni quoi
// corriger ni où : sans en-tête d'autorisation dans la configuration,
// c'est presque toujours qu'il en manque un.
func authHint(serverName string, server config.MCPServer, err error) string {
	if err == nil {
		return ""
	}

	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "unauthorized") && !strings.Contains(message, "forbidden") &&
		!strings.Contains(message, "401") && !strings.Contains(message, "403") {
		return ""
	}

	for name := range server.Headers {
		if strings.EqualFold(name, "authorization") {
			// Un en-tête est bien posé : c'est sa valeur qui est refusée,
			// pas son absence.
			return " — le jeton d'autorisation configuré a été refusé"
		}
	}

	return " — aucun en-tête Authorization dans mcp_servers." + serverName +
		".headers, alors que le serveur en exige un"
}
