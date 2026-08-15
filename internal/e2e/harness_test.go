// Package e2e_test assemble le système complet d'Automata (identité,
// autorisation, persistance, mémoire réelle, moteur d'actions, pipeline
// ingress, scheduler) exactement comme internal/registry.Run le ferait,
// mais avec les composants externes (client LLM, serveurs MCP, fournisseur
// Courier) remplacés par des fakes contrôlables : voir PLAN.md Phase 21.
//
// L'objectif n'est pas de retester chaque règle déjà couverte par les
// suites unitaires de chaque package (internal/agent, internal/action,
// internal/scheduler, ...), mais de prouver que leur ASSEMBLAGE fonctionne :
// un message entrant traverse réellement l'ingress (règle de mention,
// déduplication), la résolution d'identité, l'autorisation, l'agent, le
// moteur d'actions et la persistance, sans court-circuit.
//
// Les patterns de construction (fakeCompletionClient scripté, serveur MCP
// HTTP factice, SQLite temporaire réelle, Amoxtli réel temporaire, provider
// Courier en mémoire) reprennent ceux déjà établis par
// internal/agent/agenda_test.go, internal/agent/memory_tools_test.go et
// internal/scheduler/scheduler_test.go, plutôt que d'en inventer de
// nouveaux.
package e2e_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/amoxtli"
	amoxtlibleve "github.com/bornholm/amoxtli/index/bleve"
	amoxtligorm "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/go-courier"
	couriermemory "github.com/bornholm/go-courier/provider/memory"
	goMCP "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bornholm/automata/internal/action"
	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// --- infrastructure partagée : sqlite temporaire, mémoire Amoxtli réelle --

const testProviderName = "whatsapp"

var selfUser = courier.NewUser("assistant-id", "Assistant")

func testStorageConfig(t *testing.T) config.StorageApplication {
	t.Helper()
	return config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}
}

func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()
	db, err := persistence.Open(context.Background(), testStorageConfig(t))
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newMemoryStore construit un memory.AmoxtliStore adossé à un Amoxtli réel
// (store SQLite + index bleve, répertoires temporaires), comme
// internal/memory/amoxtli_store_test.go (PLAN.md §16 : jamais de mock du
// Codex).
func newMemoryStore(t *testing.T) *memory.AmoxtliStore {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	store, err := amoxtligorm.NewSQLiteStore(filepath.Join(dir, "amoxtli.sqlite"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	idx, err := amoxtlibleve.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("bleve.OpenOrCreate: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: idx, Weight: 1}),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("amoxtli.New: %v", err)
	}
	t.Cleanup(func() { _ = codex.Close() })

	s, err := memory.NewAmoxtliStore(ctx, codex, "e2e-memory")
	if err != nil {
		t.Fatalf("memory.NewAmoxtliStore: %v", err)
	}

	return s
}

// --- configuration de test ------------------------------------------------

// baseOrgConfig construit une configuration minimale mais représentative :
// une organisation "home", trois principaux (alice/bob adultes avec accès
// complet à leur mémoire personnelle/groupe/org, leo avec un accès org en
// lecture seule uniquement), leurs canaux privés et un canal de groupe dont
// les trois sont membres. Les tests mutent cfg (Channels, MCPServers,
// Schedules...) selon leurs besoins avant de construire le système.
func baseOrgConfig() *config.Config {
	return &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Identities: config.Identities{
			Roles: map[string]config.Role{
				"adult": {Permissions: []string{
					"memory.personal.read", "memory.personal.write", "memory.personal.delete",
					"memory.group.read", "memory.group.write", "memory.group.delete",
					"memory.org.read", "memory.org.write", "memory.org.delete",
				}},
				"org-reader": {Permissions: []string{"memory.org.read"}},
			},
			Principals: []config.Principal{
				{ID: "alice", Kind: config.PrincipalKindHuman, DisplayName: "Alice", Roles: []string{"adult"}},
				{ID: "bob", Kind: config.PrincipalKindHuman, DisplayName: "Bob", Roles: []string{"adult"}},
				{ID: "leo", Kind: config.PrincipalKindHuman, DisplayName: "Léo", Roles: []string{"org-reader"}},
			},
		},
		Origins: []config.Origin{
			{Provider: testProviderName, ExternalUserID: "alice-ext", PrincipalID: "alice"},
			{Provider: testProviderName, ExternalUserID: "bob-ext", PrincipalID: "bob"},
			{Provider: testProviderName, ExternalUserID: "leo-ext", PrincipalID: "leo"},
		},
		Channels: []config.Channel{
			{
				Provider: testProviderName, ChannelID: "alice-priv",
				Kind: config.ChannelKindPrivate, OrgID: "home", Scope: config.ScopePersonal, ScopeID: "alice",
			},
			{
				Provider: testProviderName, ChannelID: "bob-priv",
				Kind: config.ChannelKindPrivate, OrgID: "home", Scope: config.ScopePersonal, ScopeID: "bob",
			},
			{
				Provider: testProviderName, ChannelID: "leo-priv",
				Kind: config.ChannelKindPrivate, OrgID: "home", Scope: config.ScopePersonal, ScopeID: "leo",
			},
			{
				Provider: testProviderName, ChannelID: "group-chan",
				Kind: config.ChannelKindGroup, OrgID: "home", Scope: config.ScopeGroup, ScopeID: "home-group",
				Members: []string{"alice", "bob", "leo"},
			},
		},
	}
}

// --- fakes réutilisés ------------------------------------------------------

// fakeClock est une horloge contrôlable manuellement (identique au pattern
// de internal/scheduler/scheduler_test.go).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// fakeCompletionClient est un llm.ChatCompletionClient scripté (identique
// au pattern de internal/agent/orchestrator_test.go), reproduit ici car
// non exporté par internal/agent.
type fakeCompletionClient struct {
	mu           sync.Mutex
	responseFunc func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error)
	turn         int
}

func (f *fakeCompletionClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)

	f.mu.Lock()
	turn := f.turn
	f.turn++
	f.mu.Unlock()

	return f.responseFunc(turn, opts)
}

func (f *fakeCompletionClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.turn
}

var _ llm.ChatCompletionClient = &fakeCompletionClient{}

func scriptedFinalResponse(text string) llm.ChatCompletionResponse {
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, text), llm.NewChatCompletionUsage(1, 1, 2))
}

func scriptedToolCallResponse(toolCalls ...llm.ToolCall) llm.ChatCompletionResponse {
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, ""), llm.NewChatCompletionUsage(1, 1, 2), toolCalls...)
}

// lastToolResultText retourne le contenu du dernier message "tool" observé
// dans opts.Messages, comme le verrait réellement le modèle au tour suivant.
func lastToolResultText(opts *llm.ChatCompletionOptions) string {
	for _, m := range slices.Backward(opts.Messages) {
		if m.Role() == llm.RoleTool {
			return m.Content()
		}
	}
	return ""
}

// spyTranscriber est un audio.Transcriber de test scripté (même pattern que
// internal/conversation/audio_test.go).
type spyTranscriber struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (t *spyTranscriber) Transcribe(ctx context.Context, data []byte) (string, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return t.reply, nil
}

var _ audio.Transcriber = &spyTranscriber{}

// --- serveur MCP calendrier factice (repris de internal/agent/agenda_test.go) --

type listEventsParams struct {
	CalendarID string `json:"calendar_id"`
	From       string `json:"from"`
	To         string `json:"to"`
}

type createEventParams struct {
	CalendarID string `json:"calendar_id"`
	Title      string `json:"title"`
	Start      string `json:"start"`
	End        string `json:"end"`
}

type calendarSpy struct {
	mu sync.Mutex

	listCalls   int
	createCalls int

	lastListCalendarID   string
	lastCreateCalendarID string
}

func (s *calendarSpy) recordList(calendarID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.lastListCalendarID = calendarID
}

func (s *calendarSpy) recordCreate(calendarID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.lastCreateCalendarID = calendarID
}

func (s *calendarSpy) snapshot() (listCalls, createCalls int, lastListCalendarID, lastCreateCalendarID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.createCalls, s.lastListCalendarID, s.lastCreateCalendarID
}

func newFakeCalendarServer(t *testing.T) (*httptest.Server, *calendarSpy) {
	t.Helper()

	spy := &calendarSpy{}

	server := goMCP.NewServer(&goMCP.Implementation{Name: "fake-calendar", Version: "v0.0.1"}, nil)

	goMCP.AddTool(server, &goMCP.Tool{Name: "list_events", Description: "list calendar events for a given range"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args listEventsParams) (*goMCP.CallToolResult, any, error) {
			spy.recordList(args.CalendarID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Aucun événement trouvé."}}}, nil, nil
		})

	goMCP.AddTool(server, &goMCP.Tool{Name: "create_event", Description: "create a calendar event"},
		func(ctx context.Context, req *goMCP.CallToolRequest, args createEventParams) (*goMCP.CallToolResult, any, error) {
			spy.recordCreate(args.CalendarID)
			return &goMCP.CallToolResult{Content: []goMCP.Content{&goMCP.TextContent{Text: "Événement créé avec succès."}}}, nil, nil
		})

	handler := goMCP.NewSSEHandler(func(*http.Request) *goMCP.Server {
		return server
	}, nil)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	return httpServer, spy
}

// newAgendaAgent construit un agent.AgendaToolAgent adossé au serveur
// calendrier factice, comme internal/agent/agenda_test.go.
func newAgendaAgent(t *testing.T, cfg *config.Config, client llm.ChatCompletionClient) agent.Agent {
	t.Helper()

	m := mcp.NewManager(cfg, nil)
	t.Cleanup(func() { _ = m.Close() })

	return agent.NewAgendaToolAgent(client, "system", "agenda", cfg.Organization.DisplayName, m, []string{"google-calendar"}, mcp.Limits{}, 5, cfg, nil)
}

// withCalendarResources déclare le serveur MCP google-calendar et les
// ressources "calendar" des canaux alice-priv/group-chan, indispensables à
// resource.ResolveCalendarID (internal/resource).
func withCalendarResources(cfg *config.Config, calendarServerURL string) {
	cfg.MCPServers = map[string]config.MCPServer{
		"google-calendar": {Transport: "http", URL: calendarServerURL},
	}

	for i := range cfg.Channels {
		switch cfg.Channels[i].ChannelID {
		case "alice-priv":
			cfg.Channels[i].Resources = map[string]string{"calendar": "alice-personal-calendar"}
		case "group-chan":
			cfg.Channels[i].Resources = map[string]string{"calendar": "main-group-calendar"}
		}
	}
}

// newMemoryOrchestrator construit un agent.OrchestratorAgent exposant les
// outils mémoire complets (search_memory/remember/forget_memory), adossé au
// client scripté et au store/authorizer fournis (même pattern que
// internal/agent/memory_tools_test.go, mais exécuté ici à travers tout le
// pipeline applicatif).
func newMemoryOrchestrator(client llm.ChatCompletionClient, store memory.Store, authorizer *authorization.Authorizer) agent.Agent {
	tools := agent.MemoryTools{Store: store, Authorizer: authorizer, Search: true, Remember: true, Forget: true}
	return agent.NewOrchestratorAgent(client, "system", "main", "Maison", map[string]delegation.Specialist{}, 5).WithMemoryTools(tools)
}

// simpleAgent construit un agent.OrchestratorAgent sans aucun outil (ni
// mémoire, ni délégation), pour les scénarios de texte/audio purs.
func simpleAgent(client llm.ChatCompletionClient) agent.Agent {
	return agent.NewOrchestratorAgent(client, "system", "main", "Maison", map[string]delegation.Specialist{}, 5)
}

// --- système de bout en bout piloté par messages (ingress) ----------------

// sysConfig regroupe les dépendances optionnelles d'un testSystem,
// composées via des sysOption.
type sysConfig struct {
	audioCfg    audio.Config
	transcriber audio.Transcriber
	memStore    *memory.AmoxtliStore
	engineOpts  []action.Option
}

type sysOption func(*sysConfig)

func withAudio(cfg audio.Config, transcriber audio.Transcriber) sysOption {
	return func(sc *sysConfig) {
		sc.audioCfg = cfg
		sc.transcriber = transcriber
	}
}

// withMemoryStore réutilise un memory.AmoxtliStore déjà construit par le
// test (nécessaire lorsque le test construit lui-même l'agent avec ce même
// store, avant de connaître le testSystem qui l'englobera).
func withMemoryStore(store *memory.AmoxtliStore) sysOption {
	return func(sc *sysConfig) { sc.memStore = store }
}

// readyProvider signale, via un canal fermé, que Listen a bien été appelé,
// pour éviter tout délai arbitraire (même pattern que
// internal/ingress/pipeline_test.go).
type readyProvider struct {
	*couriermemory.Provider

	once  sync.Once
	ready chan struct{}
}

func newReadyProvider(funcs ...couriermemory.OptionFunc) *readyProvider {
	return &readyProvider{Provider: couriermemory.NewProvider(funcs...), ready: make(chan struct{})}
}

func (p *readyProvider) Listen(ctx context.Context) (chan courier.Message, error) {
	ch, err := p.Provider.Listen(ctx)
	if err == nil {
		p.once.Do(func() { close(p.ready) })
	}
	return ch, err
}

func (p *readyProvider) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-p.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout en attente du démarrage de l'écoute du provider")
	}
}

var (
	_ courier.Provider     = (*readyProvider)(nil)
	_ courier.SelfProvider = (*readyProvider)(nil)
)

// testSystem assemble, pour un seul test, l'ensemble des composants
// applicatifs branchés comme le ferait internal/registry.Run : persistance
// SQLite réelle, résolveur d'identité, autorisateur, mémoire Amoxtli réelle,
// moteur d'actions, gestionnaire de conversation et pipeline ingress adossé
// à un fournisseur Courier en mémoire.
type testSystem struct {
	t *testing.T

	db           *persistence.DB
	cfg          *config.Config
	resolver     *identity.Resolver
	authorizer   *authorization.Authorizer
	memStore     *memory.AmoxtliStore
	actionEngine *action.Engine
	handler      *conversation.Handler
	provider     *readyProvider
	pipeline     *ingress.Pipeline

	stopPipeline func()
}

func channelKindOf(k config.ChannelKind) courier.ChannelKind {
	if k == config.ChannelKindGroup {
		return courier.ChannelKindGroup
	}
	return courier.ChannelKindDirect
}

// newTestSystem construit un testSystem complet et démarre son pipeline
// ingress. mainAgent est l'agent "main" branché sur conversation.Handler,
// déjà construit par l'appelant (orchestrateur avec outils mémoire, agent
// agenda, etc. selon le scénario).
func newTestSystem(t *testing.T, cfg *config.Config, mainAgent agent.Agent, opts ...sysOption) *testSystem {
	t.Helper()

	sc := &sysConfig{}
	for _, opt := range opts {
		opt(sc)
	}

	db := openTestDB(t)

	resolver, err := identity.NewResolver(cfg)
	if err != nil {
		t.Fatalf("identity.NewResolver: %v", err)
	}

	authorizer := authorization.NewAuthorizer(cfg)

	memStore := sc.memStore
	if memStore == nil {
		memStore = newMemoryStore(t)
	}

	engineOpts := append([]action.Option{
		action.WithMemoryStore(memStore),
		action.WithAuditEvents(persistence.NewAuditEventRepository()),
	}, sc.engineOpts...)

	actionEngine := action.NewEngine(db, authorizer, nil, cfg, engineOpts...)

	handler := conversation.NewHandler(db, mainAgent, actionEngine, 0, sc.audioCfg, sc.transcriber, false, nil)

	channels := make([]courier.Channel, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		if ch.Provider != testProviderName {
			continue
		}
		channels = append(channels, courier.NewChannel(courier.ChannelID(ch.ChannelID), channelKindOf(ch.Kind), ch.DisplayName))
	}

	provider := newReadyProvider(couriermemory.WithSelf(selfUser), couriermemory.WithChannels(channels...))
	t.Cleanup(func() { _ = provider.Close() })

	pipeline := ingress.NewPipeline(testProviderName, provider, resolver, db, handler, testLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pipeline.Run(ctx) }()

	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout en attente de l'arrêt du pipeline")
		}
	}
	t.Cleanup(stop)

	sys := &testSystem{
		t: t, db: db, cfg: cfg, resolver: resolver, authorizer: authorizer,
		memStore: memStore, actionEngine: actionEngine, handler: handler,
		provider: provider, pipeline: pipeline, stopPipeline: stop,
	}

	sys.provider.waitReady(t)

	return sys
}

// deliver injecte un message entrant sur le canal channelID, de la part de
// fromExternalID, avec un contenu textuel et des mentions optionnelles.
func (s *testSystem) deliver(fromExternalID, channelID, text string, mentioned bool) {
	s.t.Helper()

	opts := []courier.BaseMessageOptionFunc{courier.WithMessageMainPart(text)}
	if mentioned {
		opts = append(opts, courier.WithMessageMentions(courier.Mention{UserID: selfUser.ID(), DisplayName: selfUser.DisplayName()}))
	}

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(channelID)),
		courier.NewUser(courier.UserID(fromExternalID), fromExternalID),
		opts...,
	)

	if err := s.provider.Deliver(context.Background(), msg); err != nil {
		s.t.Fatalf("provider.Deliver: %v", err)
	}
}

// sendPrivate envoie un message texte sur un canal privé.
func (s *testSystem) sendPrivate(fromExternalID, channelID, text string) {
	s.deliver(fromExternalID, channelID, text, false)
}

// sendGroup envoie un message texte de groupe, mentionnant l'assistant ou
// non selon mentioned.
func (s *testSystem) sendGroup(fromExternalID, channelID, text string, mentioned bool) {
	s.deliver(fromExternalID, channelID, text, mentioned)
}

// sendVoiceNote envoie un message composé uniquement d'une pièce jointe
// note vocale (aucune partie texte "main"), comme
// internal/conversation/audio_test.go.
func (s *testSystem) sendVoiceNote(fromExternalID, channelID string, audioBytes []byte) {
	s.t.Helper()

	part := courier.NewAttachment("note.ogg", "audio/ogg", func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(&byteReader{data: audioBytes}), nil
	}, courier.WithAttachmentVoiceNote(2*time.Second))

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(channelID)),
		courier.NewUser(courier.UserID(fromExternalID), fromExternalID),
		courier.WithMessagePart(part),
	)

	if err := s.provider.Deliver(context.Background(), msg); err != nil {
		s.t.Fatalf("provider.Deliver: %v", err)
	}
}

// byteReader adapte un []byte en io.Reader minimal, pour éviter une
// dépendance supplémentaire à bytes.Reader partagée entre tests.
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// waitUntil interroge cond jusqu'à ce qu'elle retourne vrai, ou jusqu'au
// timeout (même pattern que internal/ingress/pipeline_test.go).
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}

	return cond()
}

// waitSent attend que le nombre de messages envoyés atteigne au moins n.
func (s *testSystem) waitSent(n int) []courier.Message {
	s.t.Helper()

	if !waitUntil(s.t, 2*time.Second, func() bool { return len(s.provider.Sent()) >= n }) {
		s.t.Fatalf("messages envoyés: got %d, expected >= %d", len(s.provider.Sent()), n)
	}
	return s.provider.Sent()
}

func mainContent(t *testing.T, msg courier.Message) string {
	t.Helper()
	content, err := courier.GetMessageMainContent(context.Background(), msg)
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	return content
}

// --- helpers de vérification directe en base -------------------------------

func conversationMessages(t *testing.T, db *persistence.DB, convID model.ConversationID) []persistence.Message {
	t.Helper()

	var records []persistence.Message
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		records, err = persistence.NewMessageRepository().ListRecentByConversation(context.Background(), tx, convID, 50)
		return err
	})
	if err != nil {
		t.Fatalf("ListRecentByConversation: %v", err)
	}
	return records
}

func countActionPlansByConversation(t *testing.T, db *persistence.DB, convID model.ConversationID) int {
	t.Helper()

	var count int
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		row := tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM action_plans WHERE conversation_id = ?`, convID)
		return row.Scan(&count)
	})
	if err != nil {
		t.Fatalf("count action_plans: %v", err)
	}
	return count
}
