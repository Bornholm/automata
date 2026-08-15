package ingress_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

const testProviderName = "memory"

// selfUser est l'identité de l'assistant sur le fournisseur de test.
var selfUser = courier.NewUser("assistant-id", "Assistant")

// readyProvider décore provider/memory pour signaler, via un canal fermé,
// que le pipeline a bien démarré son écoute (Listen appelé). Cela évite
// toute dépendance à un délai arbitraire dans les tests : Deliver ne fait
// rien tant qu'aucun listener n'est enregistré.
type readyProvider struct {
	*memory.Provider

	once  sync.Once
	ready chan struct{}
}

func newReadyProvider(funcs ...memory.OptionFunc) *readyProvider {
	return &readyProvider{
		Provider: memory.NewProvider(funcs...),
		ready:    make(chan struct{}),
	}
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

// countingHandler enregistre chaque appel et retourne une réponse fixe.
type countingHandler struct {
	reply string

	mu    sync.Mutex
	calls int
}

func (h *countingHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()

	return h.reply, nil
}

func (h *countingHandler) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.calls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDB(t *testing.T) *persistence.DB {
	t.Helper()

	cfg := config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}

	db, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// testConfig construit une configuration minimale déclarant une conversation
// privée autorisée pour "alice" et un groupe connu dont "alice" est membre.
func testConfig() *config.Config {
	return &config.Config{
		Origins: []config.Origin{
			{Provider: testProviderName, ExternalUserID: "alice-ext", PrincipalID: "alice"},
		},
		Channels: []config.Channel{
			{
				Provider:  testProviderName,
				ChannelID: "private-chan",
				Kind:      config.ChannelKindPrivate,
				OrgID:     "home",
				Scope:     config.ScopePersonal,
				ScopeID:   "alice",
			},
			{
				Provider:  testProviderName,
				ChannelID: "group-chan",
				Kind:      config.ChannelKindGroup,
				OrgID:     "home",
				Scope:     config.ScopeGroup,
				ScopeID:   "main",
				Members:   []string{"alice"},
			},
		},
	}
}

func newTestPipeline(t *testing.T, handler ingress.Handler) (*ingress.Pipeline, *readyProvider) {
	t.Helper()

	resolver, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("identity.NewResolver: %v", err)
	}

	provider := newReadyProvider(
		memory.WithSelf(selfUser),
		memory.WithChannels(
			courier.NewChannel("private-chan", courier.ChannelKindDirect, "Alice"),
			courier.NewChannel("group-chan", courier.ChannelKindGroup, "Groupe"),
		),
	)
	t.Cleanup(func() { _ = provider.Close() })

	db := testDB(t)

	pipeline := ingress.NewPipeline(testProviderName, provider, resolver, db, handler, testLogger())

	return pipeline, provider
}

// runPipeline démarre pipeline.Run dans une goroutine et retourne une
// fonction d'arrêt qui annule le contexte et attend le retour de Run.
func runPipeline(t *testing.T, pipeline *ingress.Pipeline) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- pipeline.Run(ctx)
	}()

	return func() {
		cancel()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout en attente de l'arrêt du pipeline")
		}
	}
}

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

func TestPipeline_PrivateAuthorized(t *testing.T) {
	handler := &countingHandler{reply: "Message bien reçu."}
	pipeline, provider := newTestPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	ctx := context.Background()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("private-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessageMainPart("bonjour"),
	)

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	if !waitUntil(t, 2*time.Second, func() bool { return handler.Calls() == 1 }) {
		t.Fatalf("le handler n'a pas été appelé, calls=%d", handler.Calls())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("aucune réponse envoyée, sent=%d", len(provider.Sent()))
	}

	content, err := courier.GetMessageMainContent(ctx, provider.Sent()[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}

	if content != "Message bien reçu." {
		t.Errorf("contenu de la réponse = %q, attendu %q", content, "Message bien reçu.")
	}
}

func TestPipeline_GroupWithMention(t *testing.T) {
	handler := &countingHandler{reply: "Message bien reçu."}
	pipeline, provider := newTestPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	ctx := context.Background()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("group-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessageMainPart("@assistant bonjour"),
		courier.WithMessageMentions(courier.Mention{UserID: selfUser.ID(), DisplayName: selfUser.DisplayName()}),
	)

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	if !waitUntil(t, 2*time.Second, func() bool { return handler.Calls() == 1 }) {
		t.Fatalf("le handler n'a pas été appelé, calls=%d", handler.Calls())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("aucune réponse envoyée, sent=%d", len(provider.Sent()))
	}
}

func TestPipeline_GroupWithoutMention(t *testing.T) {
	handler := &countingHandler{reply: "Message bien reçu."}
	pipeline, provider := newTestPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	ctx := context.Background()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("group-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessageMainPart("bonjour sans mention"),
	)

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	// Laisser le temps au pipeline de traiter le message (ou de ne pas le
	// traiter) avant de vérifier l'absence d'effet.
	time.Sleep(100 * time.Millisecond)

	if calls := handler.Calls(); calls != 0 {
		t.Errorf("le handler a été appelé %d fois, attendu 0", calls)
	}

	if sent := len(provider.Sent()); sent != 0 {
		t.Errorf("%d réponse(s) envoyée(s), attendu 0", sent)
	}
}

func TestPipeline_UnknownOriginIgnored(t *testing.T) {
	handler := &countingHandler{reply: "Message bien reçu."}
	pipeline, provider := newTestPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	ctx := context.Background()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("private-chan"),
		courier.NewUser("unknown-ext", "Inconnu"),
		courier.WithMessageMainPart("bonjour"),
	)

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if calls := handler.Calls(); calls != 0 {
		t.Errorf("le handler a été appelé %d fois, attendu 0", calls)
	}

	if sent := len(provider.Sent()); sent != 0 {
		t.Errorf("%d réponse(s) envoyée(s), attendu 0", sent)
	}
}

func TestPipeline_DuplicateMessageProcessedOnce(t *testing.T) {
	handler := &countingHandler{reply: "Message bien reçu."}
	pipeline, provider := newTestPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	ctx := context.Background()

	msg := courier.NewMessage(
		courier.MessageID("fixed-id"),
		courier.NewChannelRef("private-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessageMainPart("bonjour"),
	)

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver (1er envoi): %v", err)
	}

	if !waitUntil(t, 2*time.Second, func() bool { return handler.Calls() == 1 }) {
		t.Fatalf("le handler n'a pas été appelé après le premier envoi, calls=%d", handler.Calls())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("aucune réponse envoyée après le premier envoi")
	}

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver (2e envoi): %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if calls := handler.Calls(); calls != 1 {
		t.Errorf("le handler a été appelé %d fois, attendu 1 (déduplication)", calls)
	}

	if sent := len(provider.Sent()); sent != 1 {
		t.Errorf("%d réponse(s) envoyée(s), attendu 1 (déduplication)", sent)
	}
}

func TestPipeline_ShutdownWithoutGoroutineLeak(t *testing.T) {
	handler := &countingHandler{reply: "Message bien reçu."}
	pipeline, provider := newTestPipeline(t, handler)

	before := runtime.NumGoroutine()

	stop := runPipeline(t, pipeline)
	provider.waitReady(t)

	stop()

	// Laisser le scheduler nettoyer les goroutines qui se terminent de
	// façon asynchrone (fermeture de canaux, libération du provider) avant
	// de comparer.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if after > before+1 {
		t.Errorf("fuite de goroutine suspectée: avant=%d, après=%d", before, after)
	}
}
