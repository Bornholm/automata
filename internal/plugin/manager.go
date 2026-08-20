// Package plugin porte le côté hôte du système de plugins : découverte des
// binaires, cycle de vie des sous-processus (hashicorp/go-plugin, gRPC),
// service hôte (configurations, secrets, notifications) et pont vers les
// agents. Le patron vient du système de plugins de Xolo, avec trois
// corrections : le service hôte authentifie chaque connexion (un plugin ne
// lit jamais les données d'un autre), les secrets sont vérifiés contre
// l'appartenance du membre à l'organisation, et l'interface embarquée
// exige un jeton connu du seul reverse proxy.
package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/pkg/pluginsdk"
	proto "github.com/bornholm/automata/pkg/pluginsdk/proto"
)

// Entry est un plugin chargé : descripteur, client gRPC, port et jeton de
// son interface embarquée.
type Entry struct {
	Descriptor *proto.PluginDescriptor
	Client     proto.AutomataPluginClient

	gopClient     *goplugin.Client
	httpPort      uint32
	uiToken       string
	binaryPath    string
	lastRestartAt time.Time
}

// Status est l'état d'un plugin pour l'écran d'administration.
type Status struct {
	Name        string
	Version     string
	Description string
	Running     bool
	HasUI       bool
	HasTriggers bool
	HasSubAgent bool
	RestartedAt time.Time
}

// Manager découvre et pilote les sous-processus de plugins.
type Manager struct {
	cfg         config.Plugins
	hostService *HostService
	// agentNames refuse au chargement un plugin dont le nom entre en
	// collision avec un agent configuré : le nom du plugin devient un
	// outil delegate_to_<nom>, deux homonymes seraient indiscernables.
	agentNames map[string]struct{}

	// baseCtx borne la vie des sous-processus, des connexions broker et
	// (lot C) des flux de déclencheurs. JAMAIS un contexte de requête :
	// un plugin vit bien au-delà du tour qui l'a réveillé, et le lier à
	// une requête tuerait la connexion entre deux messages (leçon
	// d'internal/mcp/manager.go).
	baseCtx    context.Context
	cancelBase context.CancelFunc

	mu      sync.RWMutex
	plugins []*Entry
}

// NewManager crée le gestionnaire. Les dépendances du service hôte
// (persistance, scellement, notifications) sont portées par hostService.
func NewManager(cfg config.Plugins, hostService *HostService, agentNames []string) *Manager {
	baseCtx, cancel := context.WithCancel(context.Background())

	names := make(map[string]struct{}, len(agentNames))
	for _, n := range agentNames {
		names[n] = struct{}{}
	}

	return &Manager{
		cfg:         cfg,
		hostService: hostService,
		agentNames:  names,
		baseCtx:     baseCtx,
		cancelBase:  cancel,
	}
}

// Start parcourt le répertoire et lance chaque plugin. Un répertoire
// absent n'est pas une erreur ; un plugin qui refuse de se charger est
// journalisé et ignoré — jamais fatal pour l'instance.
func (m *Manager) Start(ctx context.Context) error {
	paths, err := m.scanDir()
	if err != nil {
		slog.WarnContext(ctx, "plugin: répertoire indisponible, aucun plugin chargé",
			"dir", m.cfg.Dir, "error", err)
		return nil
	}

	for _, path := range paths {
		entry, err := m.loadPlugin(path)
		if err != nil {
			slog.WarnContext(ctx, "plugin: chargement refusé", "path", path, "error", err)
			continue
		}

		m.mu.Lock()
		if _, existing := m.findEntry(entry.Descriptor.Name); existing != nil {
			m.mu.Unlock()
			entry.gopClient.Kill()
			slog.WarnContext(ctx, "plugin: nom déjà chargé, doublon ignoré",
				"name", entry.Descriptor.Name, "path", path)
			continue
		}
		m.plugins = append(m.plugins, entry)
		m.mu.Unlock()

		slog.InfoContext(ctx, "plugin: chargé",
			"name", entry.Descriptor.Name,
			"version", entry.Descriptor.Version,
			"ui", entry.httpPort != 0,
			"triggers", entry.Descriptor.HasTriggers)
	}

	return nil
}

// Loaded retourne les noms des plugins chargés, pour le câblage des
// exécuteurs d'actions.
func (m *Manager) Loaded() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.plugins))
	for _, e := range m.plugins {
		names = append(names, e.Descriptor.Name)
	}
	return names
}

// Statuses retourne l'état de chaque plugin pour l'administration.
func (m *Manager) Statuses() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]Status, 0, len(m.plugins))
	for _, e := range m.plugins {
		statuses = append(statuses, Status{
			Name:        e.Descriptor.Name,
			Version:     e.Descriptor.Version,
			Description: e.Descriptor.Description,
			Running:     !e.gopClient.Exited(),
			HasUI:       e.httpPort != 0,
			HasTriggers: e.Descriptor.HasTriggers,
			HasSubAgent: e.Descriptor.SubAgent != nil,
			RestartedAt: e.lastRestartAt,
		})
	}
	return statuses
}

// UIEndpoint retourne le port et le jeton de l'interface du plugin, relus
// à chaque requête du reverse proxy : ils changent à chaque redémarrage.
func (m *Manager) UIEndpoint(name string) (port uint32, token string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, entry := m.findEntry(name)
	if entry == nil || entry.httpPort == 0 {
		return 0, "", false
	}
	return entry.httpPort, entry.uiToken, true
}

// GetOrRestart retourne le client du plugin, en le relançant s'il est
// mort. La détection est paresseuse — au point d'usage — et bornée par un
// délai de refroidissement : un plugin qui meurt en boucle échoue
// proprement au lieu de consommer la machine.
func (m *Manager) GetOrRestart(ctx context.Context, name string) (proto.AutomataPluginClient, *proto.PluginDescriptor, bool) {
	m.mu.RLock()
	_, entry := m.findEntry(name)
	if entry == nil {
		m.mu.RUnlock()
		return nil, nil, false
	}
	if !entry.gopClient.Exited() {
		client, desc := entry.Client, entry.Descriptor
		m.mu.RUnlock()
		return client, desc, true
	}
	m.mu.RUnlock()

	return m.restart(ctx, name, false)
}

// Restart relance le plugin sans condition de refroidissement : c'est
// l'action humaine de l'écran d'administration.
func (m *Manager) Restart(ctx context.Context, name string) bool {
	_, _, ok := m.restart(ctx, name, true)
	return ok
}

func (m *Manager) restart(ctx context.Context, name string, force bool) (proto.AutomataPluginClient, *proto.PluginDescriptor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx, entry := m.findEntry(name)
	if entry == nil {
		return nil, nil, false
	}
	// Une autre goroutine a pu relancer entre les deux verrous.
	if !force && !entry.gopClient.Exited() {
		return entry.Client, entry.Descriptor, true
	}

	if !force {
		cooldown := m.cfg.EffectiveRestartCooldown()
		if !entry.lastRestartAt.IsZero() {
			if elapsed := time.Since(entry.lastRestartAt); elapsed < cooldown {
				slog.WarnContext(ctx, "plugin: refroidissement en cours, redémarrage refusé",
					"name", name, "remaining", (cooldown - elapsed).String())
				return nil, nil, false
			}
		}
	}

	entry.gopClient.Kill()

	newEntry, err := m.loadPlugin(entry.binaryPath)
	if err != nil {
		slog.ErrorContext(ctx, "plugin: redémarrage échoué", "name", name, "error", err)
		return nil, nil, false
	}

	newEntry.lastRestartAt = time.Now()
	m.plugins[idx] = newEntry

	slog.InfoContext(ctx, "plugin: redémarré", "name", name)

	return newEntry.Client, newEntry.Descriptor, true
}

// Shutdown arrête tous les sous-processus.
func (m *Manager) Shutdown() {
	m.cancelBase()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.plugins {
		e.gopClient.Kill()
	}
	m.plugins = nil
}

// findEntry doit être appelée sous verrou (lecture ou écriture).
func (m *Manager) findEntry(name string) (int, *Entry) {
	for i, e := range m.plugins {
		if e.Descriptor.Name == name {
			return i, e
		}
	}
	return -1, nil
}

// scanDir retourne les fichiers exécutables du répertoire, sans récursion.
// Les liens symboliques sont résolus et confinés au répertoire : un lien
// vers l'extérieur n'exécute jamais rien.
func (m *Manager) scanDir() ([]string, error) {
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return nil, err
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(m.cfg.Dir, e.Name())

		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		absDir, _ := filepath.Abs(m.cfg.Dir)
		absResolved, _ := filepath.Abs(resolved)
		if !isWithinDir(absResolved, absDir) {
			slog.Warn("plugin: lien symbolique hors du répertoire, ignoré", "path", path)
			continue
		}

		info, err := os.Stat(resolved)
		if err != nil || info.Mode()&0o111 == 0 {
			continue
		}
		paths = append(paths, resolved)
	}
	return paths, nil
}

func isWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return len(rel) > 0 && rel[0] != '.'
}

// loadPlugin lance le binaire, négocie le handshake, lit le descripteur et
// initialise l'interface embarquée. Toute la vie du sous-processus est
// bornée par baseCtx, jamais par un contexte de requête.
func (m *Manager) loadPlugin(binaryPath string) (*Entry, error) {
	ctx := m.baseCtx

	cmd := exec.Command(binaryPath)
	// Le niveau de journal courant est transmis au sous-processus : son
	// slog émet du JSON sur stderr que go-plugin reroute au bon niveau.
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("AUTOMATA_LOGGER_LEVEL=%d", currentSlogLevelInt(ctx)))
	if m.cfg.MemLimit != "" {
		cmd.Env = append(cmd.Env, "GOMEMLIMIT="+m.cfg.MemLimit)
	}

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  pluginsdk.HandshakeConfig,
		Plugins:          pluginsdk.PluginMap,
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   filepath.Base(binaryPath),
			Level:  slogLevelToHCLog(ctx),
			Output: os.Stderr,
		}),
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("handshake: %w", err)
	}

	raw, err := rpcClient.Dispense(pluginsdk.PluginName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("dispense: %w", err)
	}

	bundle, ok := raw.(*pluginsdk.PluginClientBundle)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("type de client inattendu: %T", raw)
	}

	desc, err := bundle.Describe(ctx, &proto.DescribeRequest{})
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("describe: %w", err)
	}
	if desc.Name == "" {
		client.Kill()
		return nil, fmt.Errorf("descripteur sans nom")
	}
	if _, taken := m.agentNames[desc.Name]; taken {
		client.Kill()
		return nil, fmt.Errorf("le nom %q entre en collision avec un agent configuré", desc.Name)
	}

	port, token := m.initialize(bundle.AutomataPluginClient, bundle.Broker, desc.Name)

	return &Entry{
		Descriptor: desc,
		Client:     bundle.AutomataPluginClient,
		gopClient:  client,
		httpPort:   port,
		uiToken:    token,
		binaryPath: binaryPath,
	}, nil
}

// initialize sert AutomataHostService au plugin par le broker, puis
// appelle Initialize. Le service est SCOPÉ à ce plugin : l'identité vient
// de la connexion, jamais des messages, un plugin ne peut donc pas lire
// les données d'un autre.
func (m *Manager) initialize(client proto.AutomataPluginClient, broker *goplugin.GRPCBroker, pluginName string) (uint32, string) {
	ctx := m.baseCtx
	brokerID := broker.NextId()
	scoped := m.hostService.scopedTo(pluginName)

	// AcceptAndServe bloque : goroutine obligatoire, et AVANT l'appel à
	// Initialize pour que le listener soit prêt quand le plugin compose
	// broker.Dial dans son handler.
	go broker.AcceptAndServe(brokerID, func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(opts...)
		proto.RegisterAutomataHostServiceServer(s, scoped)
		return s
	})

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := client.Initialize(initCtx, &proto.InitializeRequest{HostServiceBrokerId: brokerID})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			slog.DebugContext(ctx, "plugin: pas d'Initialize, aucune interface", "name", pluginName)
		} else {
			slog.WarnContext(ctx, "plugin: Initialize en échec, aucune interface", "name", pluginName, "error", err)
		}
		return 0, ""
	}

	return resp.HttpUiPort, resp.UiAuthToken
}

func currentSlogLevelInt(ctx context.Context) int {
	l := slog.Default()
	switch {
	case l.Enabled(ctx, slog.LevelDebug):
		return int(slog.LevelDebug)
	case l.Enabled(ctx, slog.LevelInfo):
		return int(slog.LevelInfo)
	case l.Enabled(ctx, slog.LevelWarn):
		return int(slog.LevelWarn)
	default:
		return int(slog.LevelError)
	}
}

func slogLevelToHCLog(ctx context.Context) hclog.Level {
	l := slog.Default()
	switch {
	case l.Enabled(ctx, slog.LevelDebug):
		return hclog.Debug
	case l.Enabled(ctx, slog.LevelInfo):
		return hclog.Info
	case l.Enabled(ctx, slog.LevelWarn):
		return hclog.Warn
	default:
		return hclog.Error
	}
}
