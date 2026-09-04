package ingress_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/media"
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

func (h *countingHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()

	return h.reply, nil, nil
}

func (h *countingHandler) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.calls
}

// syncBuffer sérialise les écritures d'un handler slog lues depuis une autre
// goroutine que celle du pipeline.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
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

	return newTestPipelineWithLogger(t, handler, testLogger())
}

func newTestPipelineWithLogger(t *testing.T, handler ingress.Handler, logger *slog.Logger) (*ingress.Pipeline, *readyProvider) {
	t.Helper()

	pipeline, provider, _ := newTestPipelineWithDB2(t, handler, logger)

	return pipeline, provider
}

// newTestPipelineWithDB construit un pipeline de test et expose sa base,
// pour les tests qui vérifient les écritures (liaison par jeton).
func newTestPipelineWithDB(t *testing.T, handler ingress.Handler) (*ingress.Pipeline, *readyProvider, *persistence.DB) {
	t.Helper()

	return newTestPipelineWithDB2(t, handler, testLogger())
}

func newTestPipelineWithDB2(t *testing.T, handler ingress.Handler, logger *slog.Logger) (*ingress.Pipeline, *readyProvider, *persistence.DB) {
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
			// Canaux des tests de liaison par jeton (linking_integration_test.go).
			courier.NewChannel("camille-ext", courier.ChannelKindDirect, "Camille Roux"),
			courier.NewChannel("atelier-group", courier.ChannelKindGroup, "Atelier"),
		),
	)
	t.Cleanup(func() { _ = provider.Close() })

	db := testDB(t)

	pipeline := ingress.NewPipeline(testProviderName, provider, resolver, db, handler, logger, nil)

	return pipeline, provider, db
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

// mediaReplyHandler répond avec un texte ET une pièce jointe, comme le fait
// un agent ayant produit une image via un outil.
type mediaReplyHandler struct {
	reply       string
	attachments []media.Media
}

func (h *mediaReplyHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	return h.reply, h.attachments, nil
}

// TestPipeline_ReplyWithAttachment vérifie le dernier maillon de la chaîne
// multimodale : le média produit par l'agent est réellement joint au message
// envoyé sur le canal, et non perdu au passage du transport.
func TestPipeline_ReplyWithAttachment(t *testing.T) {
	data := []byte("octets du graphique")

	handler := &mediaReplyHandler{
		reply: "Voici le graphique.",
		attachments: []media.Media{{
			Kind:     media.KindImage,
			MimeType: "image/png",
			Filename: "graphique.png",
			Data:     data,
		}},
	}

	pipeline, provider := newTestPipeline(t, handler)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	ctx := context.Background()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("private-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessageMainPart("fais un graphique"),
	)

	if err := provider.Deliver(ctx, msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("aucune réponse envoyée, sent=%d", len(provider.Sent()))
	}

	sent := provider.Sent()[0]

	content, err := courier.GetMessageMainContent(ctx, sent)
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}
	if content != "Voici le graphique." {
		t.Errorf("texte de la réponse = %q", content)
	}

	attachments := courier.Attachments(sent)
	if len(attachments) != 1 {
		t.Fatalf("pièces jointes envoyées: got %d, expected 1", len(attachments))
	}

	if got := attachments[0].Filename(); got != "graphique.png" {
		t.Errorf("filename = %q, attendu graphique.png", got)
	}
	if got := attachments[0].ContentType(); got != "image/png" {
		t.Errorf("content_type = %q, attendu image/png", got)
	}

	reader, err := attachments[0].Reader(ctx)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("lecture de la pièce jointe: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("données de la pièce jointe altérées: %q", got)
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

// Les identifiants d'une origine inconnue sont journalisés : c'est ainsi
// qu'on découvre ceux à déclarer dans identities et channels, un identifiant
// de groupe ou de conversation privée n'étant pas connaissable à l'avance.
func TestPipeline_UnknownOriginLogsIdentifiers(t *testing.T) {
	// Le pipeline journalise depuis sa propre goroutine : le tampon est lu par
	// le test pendant qu'il tourne encore.
	logs := &syncBuffer{}

	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	pipeline, provider := newTestPipelineWithLogger(t, &countingHandler{}, logger)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannel("groupe-inconnu", courier.ChannelKindGroup, "Famille"),
		courier.NewUser("unknown-ext", "Inconnu"),
		courier.WithMessageMainPart("bonjour"),
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	for _, want := range []string{
		"channel_id=groupe-inconnu",
		"channel_kind=group",
		"channel_name=Famille",
		"user_id=unknown-ext",
		"user_name=Inconnu",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("journal sans %q:\n%s", want, logs.String())
		}
	}

	// Le contenu du message n'a rien à faire dans le journal.
	if strings.Contains(logs.String(), "bonjour") {
		t.Errorf("le contenu du message a été journalisé:\n%s", logs.String())
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

// deadlineCapturingHandler enregistre si le ctx reçu par Handle porte une
// échéance (deadline).
type deadlineCapturingHandler struct {
	reply string

	mu          sync.Mutex
	hadDeadline bool
	deadline    time.Time
}

func (h *deadlineCapturingHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	h.mu.Lock()
	h.deadline, h.hadDeadline = ctx.Deadline()
	h.mu.Unlock()

	return h.reply, nil, nil
}

func (h *deadlineCapturingHandler) result() (bool, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.hadDeadline, h.deadline
}

// TestPipeline_HandleContextIsBounded vérifie que le ctx transmis à
// Handler.Handle porte une échéance bornée, jamais le ctx non borné de
// Pipeline.Run (plan de conception, Phase 19, point 8 "timeouts réseau") : sans cela,
// un appel LLM/MCP qui ne répond jamais bloquerait indéfiniment le
// traitement de tous les messages suivants du même fournisseur, la boucle
// de Pipeline.Run étant strictement séquentielle.
func TestPipeline_HandleContextIsBounded(t *testing.T) {
	handler := &deadlineCapturingHandler{reply: "ok"}
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

	var hadDeadline bool
	var deadline time.Time
	if !waitUntil(t, 2*time.Second, func() bool {
		hadDeadline, deadline = handler.result()
		return hadDeadline
	}) {
		t.Fatal("le ctx transmis à Handle ne porte aucune échéance (deadline)")
	}

	if remaining := time.Until(deadline); remaining <= 0 || remaining > 6*time.Minute {
		t.Errorf("échéance du ctx hors bornes attendues (0, 6min]: reste %s", remaining)
	}
}

// errorHandler échoue systématiquement, comme un tour dont les retries LLM
// sont épuisés.
type errorHandler struct{}

func (errorHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	return "", nil, errors.New("panne simulée du traitement")
}

func TestPipeline_HandlerErrorSendsFallback(t *testing.T) {
	pipeline, provider := newTestPipeline(t, errorHandler{})
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

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("aucune réponse de repli envoyée après l'échec du handler (envoyés: %d)", len(provider.Sent()))
	}

	content, err := courier.GetMessageMainContent(ctx, provider.Sent()[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}

	if content != ingress.FallbackReply(i18n.Default) {
		t.Errorf("contenu de la réponse de repli = %q, attendu %q", content, ingress.FallbackReply(i18n.Default))
	}
}

// explainedErrorHandler échoue avec une erreur porteuse d'un message destiné
// à l'utilisateur, comme le fait le traitement d'une note vocale inaudible.
type explainedErrorHandler struct{}

func (explainedErrorHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	return "", nil, fmt.Errorf("conversation: transcription de la note vocale: %w",
		apperr.Explain(audio.ErrEmptyTranscription, "Je n'ai rien entendu dans ce message vocal."))
}

// Un échec dû à ce que la personne a envoyé lui vaut une explication utile,
// pas le repli générique : « réessaie dans quelques instants » est un mauvais
// conseil quand réessayer à l'identique redonnera le même résultat.
func TestPipeline_ExplainedErrorReplacesFallback(t *testing.T) {
	pipeline, provider := newTestPipeline(t, explainedErrorHandler{})
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

	if !waitUntil(t, 2*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("aucune réponse envoyée (envoyés: %d)", len(provider.Sent()))
	}

	content, err := courier.GetMessageMainContent(ctx, provider.Sent()[0])
	if err != nil {
		t.Fatalf("GetMessageMainContent: %v", err)
	}

	if content != "Je n'ai rien entendu dans ce message vocal." {
		t.Errorf("réponse = %q, attendu l'explication portée par l'erreur", content)
	}
	if content == ingress.FallbackReply(i18n.Default) {
		t.Error("le repli générique a été envoyé au lieu de l'explication")
	}
}

// flakySendProvider fait échouer les premiers Send pour simuler une panne
// transitoire du fournisseur, puis délègue au provider mémoire.
type flakySendProvider struct {
	*readyProvider

	mu       sync.Mutex
	failures int
	attempts int
}

func (p *flakySendProvider) Send(ctx context.Context, msg courier.Message) error {
	p.mu.Lock()
	p.attempts++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()

	if fail {
		return errors.New("panne transitoire simulée")
	}

	return p.readyProvider.Send(ctx, msg)
}

func (p *flakySendProvider) Attempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.attempts
}

func TestPipeline_SendRetriesAfterTransientFailure(t *testing.T) {
	resolver, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("identity.NewResolver: %v", err)
	}

	provider := &flakySendProvider{
		readyProvider: newReadyProvider(
			memory.WithSelf(selfUser),
			memory.WithChannels(courier.NewChannel("private-chan", courier.ChannelKindDirect, "Alice")),
		),
		failures: 1,
	}
	t.Cleanup(func() { _ = provider.Close() })

	pipeline := ingress.NewPipeline(testProviderName, provider, resolver, testDB(t), &countingHandler{reply: "pong"}, testLogger(), nil)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("private-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessageMainPart("ping"),
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	// Le premier Send échoue, le retry attend 1 s avant la seconde tentative.
	if !waitUntil(t, 5*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("réponse jamais envoyée malgré le retry (tentatives: %d)", provider.Attempts())
	}

	if got := provider.Attempts(); got != 2 {
		t.Errorf("tentatives d'envoi = %d, attendu 2 (un échec puis un succès)", got)
	}
}

// statusProvider enregistre les changements de statut de saisie demandés
// par le pipeline.
type statusProvider struct {
	*readyProvider

	mu       sync.Mutex
	statuses []courier.Status
}

func (p *statusProvider) SetStatus(ctx context.Context, status courier.Status, channelID courier.ChannelID) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.statuses = append(p.statuses, status)
	return nil
}

func (p *statusProvider) Statuses() []courier.Status {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]courier.Status(nil), p.statuses...)
}

var _ courier.StatusProvider = (*statusProvider)(nil)

// blockingHandler bloque jusqu'à la fermeture de release, pour observer
// l'état du pipeline pendant un traitement long.
type blockingHandler struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (h *blockingHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	h.once.Do(func() { close(h.entered) })

	select {
	case <-h.release:
	case <-ctx.Done():
	}

	return "fini", nil, nil
}

func TestPipeline_TypingIndicatorDuringHandling(t *testing.T) {
	resolver, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("identity.NewResolver: %v", err)
	}

	provider := &statusProvider{
		readyProvider: newReadyProvider(
			memory.WithSelf(selfUser),
			memory.WithChannels(courier.NewChannel("private-chan", courier.ChannelKindDirect, "Alice")),
		),
	}
	t.Cleanup(func() { _ = provider.Close() })

	handler := &blockingHandler{release: make(chan struct{}), entered: make(chan struct{})}

	pipeline := ingress.NewPipeline(testProviderName, provider, resolver, testDB(t), handler, testLogger(), nil)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverText(t, provider.readyProvider, "private-chan", "question longue")

	select {
	case <-handler.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("le handler n'a jamais été appelé")
	}

	// L'indicateur doit être actif pendant le traitement.
	if !waitUntil(t, 2*time.Second, func() bool {
		statuses := provider.Statuses()
		return len(statuses) > 0 && statuses[0] == courier.StatusTyping
	}) {
		t.Fatalf("aucun StatusTyping émis pendant le traitement (statuts: %v)", provider.Statuses())
	}

	close(handler.release)

	// Après le tour, l'indicateur est explicitement effacé.
	if !waitUntil(t, 2*time.Second, func() bool {
		statuses := provider.Statuses()
		return len(statuses) >= 2 && statuses[len(statuses)-1] == courier.StatusIdle
	}) {
		t.Fatalf("aucun StatusIdle émis après le traitement (statuts: %v)", provider.Statuses())
	}
}

// recordingHandler capture le contenu principal de chaque message traité,
// dans l'ordre.
type recordingHandler struct {
	mu       sync.Mutex
	contents []string
	parts    [][]string
	// voiceChecks relève, pour chaque tour, le nom exigé par le marqueur
	// de mention vocale ("" pour un tour ordinaire).
	voiceChecks []string
}

func (h *recordingHandler) Handle(ctx context.Context, identity model.ExecutionIdentity, conversation model.Conversation, message courier.Message) (string, []media.Media, error) {
	content, err := courier.GetMessageMainContent(ctx, message)
	if err != nil {
		content = "<erreur: " + err.Error() + ">"
	}

	var names []string
	for _, part := range message.Parts() {
		if part.Name() != courier.PartMain {
			names = append(names, part.Name())
		}
	}

	requiredName, _ := model.VoiceMentionRequired(ctx)

	h.mu.Lock()
	h.contents = append(h.contents, content)
	h.parts = append(h.parts, names)
	h.voiceChecks = append(h.voiceChecks, requiredName)
	h.mu.Unlock()

	return "", nil, nil
}

// VoiceChecks relève le marqueur de mention vocale de chaque tour.
func (h *recordingHandler) VoiceChecks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]string(nil), h.voiceChecks...)
}

// Parts relève, pour chaque tour, le nom des parts autres que la principale
// — les pièces jointes que la fusion des rafales doit préserver.
func (h *recordingHandler) Parts() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([][]string(nil), h.parts...)
}

func (h *recordingHandler) Contents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]string(nil), h.contents...)
}

// newCoalescingTestPipeline construit un pipeline avec une fenêtre de
// coalescence courte, adaptée aux tests.
func newCoalescingTestPipeline(t *testing.T, handler ingress.Handler, window time.Duration) (*ingress.Pipeline, *readyProvider) {
	t.Helper()

	pipeline, provider := newTestPipeline(t, handler)
	return pipeline.WithCoalesceWindow(window), provider
}

func deliverText(t *testing.T, provider *readyProvider, channelID, text string, funcs ...courier.BaseMessageOptionFunc) {
	t.Helper()

	options := append([]courier.BaseMessageOptionFunc{courier.WithMessageMainPart(text)}, funcs...)
	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef(courier.ChannelID(channelID)),
		courier.NewUser("alice-ext", "Alice"),
		options...,
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}
}

func TestPipeline_CoalescesBurstIntoSingleTurn(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 150*time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverText(t, provider, "private-chan", "un")
	deliverText(t, provider, "private-chan", "deux")
	deliverText(t, provider, "private-chan", "trois")

	if !waitUntil(t, 2*time.Second, func() bool { return len(handler.Contents()) == 1 }) {
		t.Fatalf("tours traités = %d, attendu 1 (rafale fusionnée) ; contenus: %q", len(handler.Contents()), handler.Contents())
	}

	if got, want := handler.Contents()[0], "un\ndeux\ntrois"; got != want {
		t.Errorf("contenu fusionné = %q, attendu %q", got, want)
	}

	// Aucun tour supplémentaire ne doit suivre.
	time.Sleep(200 * time.Millisecond)
	if got := len(handler.Contents()); got != 1 {
		t.Errorf("tours traités après attente = %d, attendu 1", got)
	}
}

func TestPipeline_BurstBrokenByNonTextMessage(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 150*time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverText(t, provider, "private-chan", "un")
	deliverText(t, provider, "private-chan", "photo",
		courier.WithMessagePart(courier.NewMessagePart("piece.png", "image/png", courier.OpenerFromString("octets"))))
	deliverText(t, provider, "private-chan", "deux")

	if !waitUntil(t, 2*time.Second, func() bool { return len(handler.Contents()) == 3 }) {
		t.Fatalf("tours traités = %d, attendu 3 (le message non textuel casse la fusion) ; contenus: %q", len(handler.Contents()), handler.Contents())
	}

	if got, want := handler.Contents(), []string{"un", "photo", "deux"}; !slices.Equal(got, want) {
		t.Errorf("contenus = %q, attendu %q (ordre d'arrivée préservé)", got, want)
	}
}

func TestPipeline_GroupBurstSingleMentionSuffices(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 150*time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverText(t, provider, "group-chan", "au fait")
	deliverText(t, provider, "group-chan", "@assistant tu peux regarder ?",
		courier.WithMessageMentions(courier.Mention{UserID: selfUser.ID(), DisplayName: selfUser.DisplayName()}))

	if !waitUntil(t, 2*time.Second, func() bool { return len(handler.Contents()) == 1 }) {
		t.Fatalf("tours traités = %d, attendu 1 (une mention dans la rafale suffit)", len(handler.Contents()))
	}

	if got, want := handler.Contents()[0], "au fait\n@assistant tu peux regarder ?"; got != want {
		t.Errorf("contenu fusionné = %q, attendu %q", got, want)
	}
}

func TestPipeline_GroupBurstWithoutMentionIgnored(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 150*time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverText(t, provider, "group-chan", "on se retrouve où ?")
	deliverText(t, provider, "group-chan", "vers 18h ?")

	time.Sleep(400 * time.Millisecond)

	if got := len(handler.Contents()); got != 0 {
		t.Errorf("tours traités = %d, attendu 0 (rafale de groupe sans mention)", got)
	}
}

// deliverGroupVoice remet un message vocal sur le canal de groupe. Le canal
// y est typé explicitement : c'est le type porté par le message qui décide
// du sursis de mention, avant toute résolution d'identité.
func deliverGroupVoice(t *testing.T, provider *readyProvider, funcs ...courier.BaseMessageOptionFunc) {
	t.Helper()

	options := append([]courier.BaseMessageOptionFunc{
		courier.WithMessagePart(courier.NewAttachment("vocal.ogg", "audio/ogg", courier.OpenerFromString("octets"))),
	}, funcs...)

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannel("group-chan", courier.ChannelKindGroup, "Groupe"),
		courier.NewUser("alice-ext", "Alice"),
		options...,
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}
}

// deliverGroupText remet un message texte sur le canal de groupe, canal typé.
func deliverGroupText(t *testing.T, provider *readyProvider, text string, funcs ...courier.BaseMessageOptionFunc) {
	t.Helper()

	options := append([]courier.BaseMessageOptionFunc{courier.WithMessageMainPart(text)}, funcs...)

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannel("group-chan", courier.ChannelKindGroup, "Groupe"),
		courier.NewUser("alice-ext", "Alice"),
		options...,
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}
}

func selfMention() courier.BaseMessageOptionFunc {
	return courier.WithMessageMentions(courier.Mention{UserID: selfUser.ID(), DisplayName: selfUser.DisplayName()})
}

// Un vocal ne peut porter aucune mention : sur WhatsApp, un message audio
// n'a pas de légende. Envoyé dans un groupe, il était donc TOUJOURS jeté,
// quoi qu'écrive ensuite la personne — le seul geste possible, « le vocal
// puis @assistant », restait sans effet. Le texte qui suit adresse
// désormais le vocal, et les deux ne font qu'un seul tour.
func TestPipeline_GroupVoiceAddressedByFollowingMention(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 150*time.Millisecond)
	pipeline.WithMentionGraceWindow(600 * time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverGroupVoice(t, provider)
	deliverGroupText(t, provider, "@assistant tu peux m'écouter ça ?", selfMention())

	if !waitUntil(t, 3*time.Second, func() bool { return len(handler.Contents()) == 1 }) {
		t.Fatalf("tours traités = %d, attendu 1 (le vocal et sa mention sont un seul geste) ; contenus: %q", len(handler.Contents()), handler.Contents())
	}

	if got, want := handler.Contents()[0], "@assistant tu peux m'écouter ça ?"; got != want {
		t.Errorf("contenu = %q, attendu %q", got, want)
	}

	// Le vocal doit survivre à la fusion : le jeter au moment même où la
	// mention vient de le rendre adressé donnerait un tour sans son objet.
	if got := handler.Parts(); len(got) != 1 || !slices.Contains(got[0], "vocal.ogg") {
		t.Errorf("parts du message traité = %v, le vocal devrait y figurer", got)
	}
}

// La mention arrive après la fenêtre de coalescence : taper « @assistant »
// prend plus de temps que les quelques secondes d'une rafale. Sans le
// sursis, le vocal serait jugé non adressé et jeté juste avant.
func TestPipeline_GroupVoiceMentionArrivesAfterCoalesceWindow(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 100*time.Millisecond)
	pipeline.WithMentionGraceWindow(2 * time.Second)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverGroupVoice(t, provider)
	time.Sleep(400 * time.Millisecond) // au-delà de la fenêtre de coalescence
	deliverGroupText(t, provider, "@assistant écoute", selfMention())

	if !waitUntil(t, 4*time.Second, func() bool { return len(handler.Contents()) == 1 }) {
		t.Fatalf("tours traités = %d, attendu 1 (le sursis laisse arriver la mention) ; contenus: %q", len(handler.Contents()), handler.Contents())
	}
}

// Un vocal de groupe sans mention n'est plus jeté par le pipeline : il
// continue jusqu'au handler AVEC le marqueur de mention vocale — c'est la
// transcription qui décidera. Le texte voisin non adressé, lui, reste
// ignoré comme avant.
func TestPipeline_GroupVoiceWithoutMentionIsHandedOverForTranscriptionCheck(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 100*time.Millisecond)
	pipeline.WithMentionGraceWindow(300 * time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverGroupVoice(t, provider)
	deliverGroupText(t, provider, "c'était rigolo hier")

	// Le vocal atteint le handler, marqué ; le texte non adressé, jamais.
	if !waitUntil(t, 3*time.Second, func() bool { return len(handler.Contents()) == 1 }) {
		t.Fatalf("tours = %d, un attendu (le vocal, marqué)", len(handler.Contents()))
	}
	checks := handler.VoiceChecks()
	if len(checks) != 1 || checks[0] == "" {
		t.Errorf("marqueur de mention vocale absent : %v", checks)
	}
	if parts := handler.Parts(); len(parts) != 1 || !slices.Contains(parts[0], "vocal.ogg") {
		t.Errorf("le tour marqué devrait porter le vocal, parts = %v", parts)
	}

	time.Sleep(300 * time.Millisecond)
	if got := len(handler.Contents()); got != 1 {
		t.Errorf("tours = %d, le texte non adressé ne doit pas être traité", got)
	}
}

// Un TEXTE de groupe sans mention reste ignoré : l'exception ne vaut que
// pour les vocaux, qui ne peuvent pas porter de mention.
func TestPipeline_GroupTextWithoutMentionStillIgnored(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 100*time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverGroupText(t, provider, "on se retrouve à 18h ?")

	time.Sleep(600 * time.Millisecond)
	if got := len(handler.Contents()); got != 0 {
		t.Errorf("tours = %d, attendu 0 (texte de groupe sans mention)", got)
	}
}

// Un vocal adressé par une mention voisine n'est PAS marqué : la mention
// écrite suffit, la transcription n'a rien à vérifier.
func TestPipeline_AdjacentMentionSkipsTheVoiceCheck(t *testing.T) {
	handler := &recordingHandler{}
	pipeline, provider := newCoalescingTestPipeline(t, handler, 150*time.Millisecond)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverGroupVoice(t, provider)
	deliverGroupText(t, provider, "@assistant écoute ça", selfMention())

	if !waitUntil(t, 3*time.Second, func() bool { return len(handler.Contents()) == 1 }) {
		t.Fatalf("tours = %d, un attendu", len(handler.Contents()))
	}
	if checks := handler.VoiceChecks(); checks[0] != "" {
		t.Errorf("marqueur inattendu %q : la mention écrite dispensait de vérifier", checks[0])
	}
}
