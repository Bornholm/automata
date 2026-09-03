package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bornholm/genai/llm"
	genaimcp "github.com/bornholm/genai/mcp"
	genaihttp "github.com/bornholm/genai/mcp/http"
	genaistdio "github.com/bornholm/genai/mcp/stdio"
	genaistreamable "github.com/bornholm/genai/mcp/streamable"
)

// Pool de connexions MCP, une par (entrée, serveur, portée). La portée est
// le MEMBRE dès que le serveur porte un patron d'identifiant, et
// l'organisation sinon : deux personnes ne partagent jamais une connexion
// authentifiée avec les identifiants de l'une d'elles. C'est la règle du
// gestionnaire MCP de l'hôte, pour la même raison.

const (
	// connectTimeout borne l'établissement d'une connexion : un serveur
	// injoignable ne doit pas suspendre le tour.
	connectTimeout = 20 * time.Second
	// idleTimeout ferme une connexion inutilisée. Sans faucheuse, cent
	// membres actifs sont cent processus qui ne meurent jamais.
	idleTimeout = 10 * time.Minute
	// reapInterval est la période de passage de la faucheuse.
	reapInterval = time.Minute
	// retireGrace laisse un appel en vol se terminer sur une connexion
	// devenue obsolète après une montée de version.
	retireGrace = 2 * time.Minute
	// maxClients plafonne les connexions simultanées, toutes entrées
	// confondues. Atteint, le pool refuse d'en ouvrir une de plus plutôt
	// que de laisser un plugin épuiser la machine.
	maxClients = 16
)

// pool garde les connexions et les outils découverts.
type pool struct {
	// baseCtx borne la vie des connexions. JAMAIS le contexte de l'appel
	// qui les fait naître : celui-ci est annulé à la fin du tour, ce qui
	// fermerait la connexion sous le nez de l'appel suivant, qui
	// réutiliserait un client déjà mort. Leçon payée côté hôte.
	baseCtx    context.Context
	cancelBase context.CancelFunc

	mu      sync.Mutex
	clients map[string]*pooledClient
	// descriptors survit à la faucheuse : les outils d'un serveur ne
	// changent pas parce que la connexion s'est endormie, et c'est ce cache
	// qui rend ListSubAgents gratuit une fois l'entrée activée.
	descriptors map[string][]toolDescriptor
}

type pooledClient struct {
	client genaimcp.Client
	tools  map[string]llm.Tool
	// agentName, orgID et memberID servent à oublier les connexions d'une
	// entrée quand le membre la désactive ou change ses identifiants.
	agentName  string
	serverName string
	orgID      string
	memberID   string
	version    string
	lastUsed   time.Time
	// retiredAt marque une connexion devenue obsolète (montée de version).
	// Elle n'est plus servie, et la faucheuse la ferme après un délai de
	// grâce : un appel encore en vol doit pouvoir se terminer.
	retiredAt time.Time
}

// toolDescriptor est un outil tel qu'annoncé à l'hôte.
type toolDescriptor struct {
	Name        string
	Description string
	SchemaJSON  string
	ReadOnly    bool
}

func newPool() *pool {
	baseCtx, cancel := context.WithCancel(context.Background())

	p := &pool{
		baseCtx:     baseCtx,
		cancelBase:  cancel,
		clients:     map[string]*pooledClient{},
		descriptors: map[string][]toolDescriptor{},
	}
	go p.reap()

	return p
}

// connection identifie une connexion : l'entrée du catalogue, le serveur,
// et la portée du cloisonnement.
type connection struct {
	agent  catalogAgent
	server serverSpec
	orgID  string
	// memberID n'entre dans la clé que si le serveur porte un patron
	// d'identifiant. Sinon la connexion est partagée par l'organisation :
	// il n'y a rien de personnel à isoler.
	memberID string
	// values porte les identifiants du membre, résolus dans les patrons au
	// moment de la connexion. Jamais journalisées.
	values map[string]string
	// version est celle réellement installée. Elle entre dans la clé : une
	// montée de version ne réutilise pas une connexion qui parle encore à
	// l'ancien binaire.
	version string
}

func (c connection) key() string {
	scope := c.orgID
	if member := c.scopedMemberID(); member != "" {
		scope = c.orgID + "|" + member
	}
	return c.agent.Name + "|" + c.server.Name + "|" + c.version + "|" + scope
}

// scopedMemberID est le membre qui borne la connexion, ou la chaîne vide
// quand le serveur ne porte aucun identifiant et peut donc être partagé.
func (c connection) scopedMemberID() string {
	if c.server.needsCredentials() {
		return c.memberID
	}
	return ""
}

// tools retourne les outils du serveur, en se connectant si nécessaire.
func (p *pool) tools(ctx context.Context, conn connection) ([]toolDescriptor, error) {
	key := conn.key()

	p.mu.Lock()
	cached, ok := p.descriptors[key]
	p.mu.Unlock()
	if ok {
		return cached, nil
	}

	if _, err := p.connect(conn); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.descriptors[key], nil
}

// call exécute un outil du serveur.
func (p *pool) call(ctx context.Context, conn connection, toolName string, args map[string]any) (string, error) {
	entry, err := p.connect(conn)
	if err != nil {
		return "", err
	}

	tool, ok := entry.tools[toolName]
	if !ok {
		return "", fmt.Errorf("outil %q inconnu du serveur %q", toolName, conn.server.Name)
	}

	result, err := tool.Execute(ctx, args)
	if err != nil {
		return "", err
	}

	return result.Text(), nil
}

// connect retourne la connexion existante ou en établit une. La création
// est sérialisée : ce n'est pas le chemin le plus concurrent possible,
// mais elle garantit qu'une portée n'obtient jamais deux clients pour le
// même serveur, et qu'aucune n'obtient celui d'une autre.
func (p *pool) connect(conn connection) (*pooledClient, error) {
	key := conn.key()

	p.mu.Lock()
	defer p.mu.Unlock()

	if entry, ok := p.clients[key]; ok {
		entry.lastUsed = time.Now()
		return entry, nil
	}

	if len(p.clients) >= maxClients {
		return nil, fmt.Errorf("trop de connexions MCP ouvertes (%d) : réessayez dans quelques minutes", maxClients)
	}

	client, err := buildClient(conn)
	if err != nil {
		return nil, err
	}

	startCtx, cancel := context.WithTimeout(p.baseCtx, connectTimeout)
	defer cancel()
	if err := client.Start(startCtx); err != nil {
		// Le nom du serveur, jamais sa configuration résolue : elle porte
		// les identifiants de la personne.
		return nil, fmt.Errorf("connexion au serveur %q impossible", conn.server.Name)
	}

	listCtx, cancelList := context.WithTimeout(p.baseCtx, connectTimeout)
	defer cancelList()
	tools, err := client.GetTools(listCtx)
	if err != nil {
		_ = client.Stop()
		return nil, fmt.Errorf("outils du serveur %q illisibles", conn.server.Name)
	}

	entry := &pooledClient{
		client:     client,
		tools:      map[string]llm.Tool{},
		agentName:  conn.agent.Name,
		serverName: conn.server.Name,
		orgID:      conn.orgID,
		// Vide pour une connexion partagée par l'organisation : elle
		// n'appartient à aucun membre en particulier.
		memberID: conn.scopedMemberID(),
		version:  conn.version,
		lastUsed: time.Now(),
	}
	descriptors := make([]toolDescriptor, 0, len(tools))

	for _, tool := range tools {
		entry.tools[tool.Name()] = tool
		descriptors = append(descriptors, toolDescriptor{
			Name:        tool.Name(),
			Description: tool.Description(),
			SchemaJSON:  schemaJSON(tool),
			ReadOnly:    isReadOnly(tool, conn.server),
		})
	}

	p.clients[key] = entry
	p.descriptors[key] = descriptors

	slog.Info("subagents: connexion MCP établie",
		"agent", conn.agent.Name, "server", conn.server.Name, "tools", len(descriptors))

	return entry, nil
}

// isReadOnly décide si un outil s'exécute pendant le tour ou passe par la
// confirmation de l'hôte. L'annotation du serveur fait foi quand elle
// existe ; à défaut, la liste read_only du catalogue, décidée par
// l'exploitant. Sans l'un ni l'autre : écriture. C'est la convention de
// l'hôte, et la seule prudente — un outil non annoté n'est pas un outil
// inoffensif.
func isReadOnly(tool llm.Tool, server serverSpec) bool {
	if annotated, ok := tool.(llm.AnnotatedTool); ok {
		if readOnly, known := annotated.ReadOnly(); known {
			return readOnly
		}
	}
	return slices.Contains(server.ReadOnly, tool.Name())
}

// schemaJSON sérialise le schéma d'entrée de l'outil pour l'hôte.
func schemaJSON(tool llm.Tool) string {
	params := tool.Parameters()
	if len(params) == 0 {
		return `{"type":"object"}`
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return `{"type":"object"}`
	}
	return string(raw)
}

// headerRoundTripper pose les en-têtes résolus sur chaque requête. Ils
// portent les identifiants du membre : ils ne sont jamais journalisés.
type headerRoundTripper struct {
	headers   map[string]string
	transport http.RoundTripper
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		req = req.Clone(req.Context())
		for name, value := range t.headers {
			req.Header.Set(name, value)
		}
	}
	return t.transport.RoundTrip(req)
}

func newHeaderClient(headers map[string]string) *http.Client {
	return &http.Client{Transport: &headerRoundTripper{headers: headers, transport: http.DefaultTransport}}
}

// buildClient construit (sans le démarrer) le client du transport déclaré,
// patrons résolus.
func buildClient(conn connection) (genaimcp.Client, error) {
	server := conn.server

	switch server.Transport {
	case transportStdio:
		command, env, err := renderStdio(server, conn.values)
		if err != nil {
			return nil, err
		}
		return genaistdio.NewClient(command, genaistdio.WithEnv(env...)), nil

	case transportHTTP, transportStreamable:
		url, headers, err := renderHTTP(server, conn.values)
		if err != nil {
			return nil, err
		}
		httpClient := newHeaderClient(headers)
		if server.Transport == transportHTTP {
			return genaihttp.NewClient(url, genaihttp.WithHTTPClient(httpClient)), nil
		}
		return genaistreamable.NewClient(url, genaistreamable.WithHTTPClient(httpClient)), nil

	default:
		return nil, fmt.Errorf("transport %q non supporté", server.Transport)
	}
}

// renderStdio résout les patrons de la commande et de l'environnement, et
// compose l'environnement du processus enfant : celui du plugin, complété
// par les variables déclarées, qui l'emportent.
func renderStdio(server serverSpec, values map[string]string) ([]string, []string, error) {
	var missing []string

	command := make([]string, len(server.Command))
	for i, arg := range server.Command {
		rendered, miss := renderTemplate(arg, values)
		command[i] = rendered
		missing = append(missing, miss...)
	}

	env := os.Environ()
	for _, key := range slices.Sorted(maps.Keys(server.Env)) {
		rendered, miss := renderTemplate(server.Env[key], values)
		missing = append(missing, miss...)
		env = append(env, key+"="+rendered)
	}

	if err := errMissingPlaceholders(missing); err != nil {
		return nil, nil, err
	}

	return command, env, nil
}

// renderHTTP résout les patrons de l'URL et des en-têtes.
func renderHTTP(server serverSpec, values map[string]string) (string, map[string]string, error) {
	var missing []string

	url, miss := renderTemplate(server.URL, values)
	missing = append(missing, miss...)

	headers := make(map[string]string, len(server.Headers))
	for _, key := range slices.Sorted(maps.Keys(server.Headers)) {
		rendered, miss := renderTemplate(server.Headers[key], values)
		missing = append(missing, miss...)
		headers[key] = rendered
	}

	if err := errMissingPlaceholders(missing); err != nil {
		return "", nil, err
	}

	return url, headers, nil
}

// forget ferme et oublie les connexions d'une entrée pour une portée
// donnée : appelé quand le membre désactive l'entrée ou change ses
// identifiants, pour que la connexion suivante reparte des nouvelles
// valeurs.
func (p *pool) forget(agentName, orgID, memberID string) {
	p.mu.Lock()
	var closing []genaimcp.Client
	for key, entry := range p.clients {
		if entry.agentName != agentName || entry.orgID != orgID {
			continue
		}
		// Une connexion partagée par l'organisation (aucun identifiant en
		// jeu) n'appartient à personne : la désactivation d'un membre ne
		// doit pas la couper sous les pieds des autres.
		if entry.memberID != "" && entry.memberID != memberID {
			continue
		}
		closing = append(closing, entry.client)
		delete(p.clients, key)
		delete(p.descriptors, key)
	}
	p.mu.Unlock()

	for _, client := range closing {
		_ = client.Stop()
	}
}

// retire met de côté les connexions d'un serveur qui parlent à une autre
// version que celle installée. Elles ne sont PAS fermées ici : un appel
// peut être en vol, et le coupé au milieu se verrait par un échec sans
// cause visible. La faucheuse s'en charge après le délai de grâce.
func (p *pool) retire(agentName, serverName, version string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for key, entry := range p.clients {
		if entry.agentName != agentName || entry.serverName != serverName || entry.version == version {
			continue
		}
		if entry.retiredAt.IsZero() {
			entry.retiredAt = now
			slog.Info("subagents: connexion retirée après une montée de version",
				"agent", agentName, "server", serverName, "from", entry.version, "to", version)
		}
		// Le descripteur, lui, part tout de suite : une version peut avoir
		// ajouté ou retiré un outil, et c'est la nouvelle liste qui doit
		// être annoncée au tour suivant.
		delete(p.descriptors, key)
	}
}

// reap ferme les connexions inutilisées depuis idleTimeout, et les
// connexions retirées passé leur délai de grâce.
func (p *pool) reap() {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.baseCtx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			idleDeadline := now.Add(-idleTimeout)
			retireDeadline := now.Add(-retireGrace)

			p.mu.Lock()
			var closing []genaimcp.Client
			for key, entry := range p.clients {
				retired := !entry.retiredAt.IsZero() && entry.retiredAt.Before(retireDeadline)
				if entry.lastUsed.Before(idleDeadline) || retired {
					closing = append(closing, entry.client)
					delete(p.clients, key)
				}
			}
			p.mu.Unlock()

			for _, client := range closing {
				_ = client.Stop()
			}
		}
	}
}

// close arrête toutes les connexions.
func (p *pool) close() {
	p.mu.Lock()
	clients := p.clients
	p.clients = map[string]*pooledClient{}
	p.mu.Unlock()

	p.cancelBase()

	for _, entry := range clients {
		_ = entry.client.Stop()
	}
}
