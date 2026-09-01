package ingress_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/memory"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/ingress"
	"github.com/bornholm/automata/internal/persistence"
)

// fixedReplyAgent est un agent.Agent minimal de test, retournant toujours la
// même réponse.
type fixedReplyAgent struct{}

func (fixedReplyAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	return agent.Result{Reply: "Message bien reçu."}, nil
}

var _ agent.Agent = fixedReplyAgent{}

// spyTranscriber est un audio.Transcriber de test qui enregistre le nombre
// d'appels, pour prouver que la lecture du média n'a jamais lieu avant la
// vérification de mention en groupe (PLAN.md §3.3, §3.4).
type spyTranscriber struct {
	calls      int
	transcript string
}

func (t *spyTranscriber) Transcribe(ctx context.Context, data []byte) (string, error) {
	t.calls++
	return t.transcript, nil
}

var _ audio.Transcriber = &spyTranscriber{}

func voiceNoteAttachment() courier.MessagePart {
	return courier.NewAttachment("note.ogg", "audio/ogg", func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("faux octets audio"))), nil
	}, courier.WithAttachmentVoiceNote(2*time.Second))
}

// Un vocal de groupe sans mention écrite est TRANSCRIT pour y chercher le
// nom de l'assistant : c'est la seule façon de rendre un vocal adressable,
// un audio ne pouvant porter aucune mention. Si le nom n'y est pas, le tour
// s'arrête là — aucune réponse, rien en base : le vocal a été écouté puis
// oublié.
func TestPipeline_GroupVoice_TranscribedButSilentWithoutSpokenName(t *testing.T) {
	transcriber := &spyTranscriber{transcript: "on se retrouve à la gare à 18h"}
	pipeline, provider, db := newVoicePipeline(t, transcriber)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverVoiceToGroup(t, provider)

	time.Sleep(400 * time.Millisecond)

	if transcriber.calls != 1 {
		t.Fatalf("transcriber appelé %d fois, attendu 1 (la mention se cherche dans le contenu)", transcriber.calls)
	}
	if sent := len(provider.Sent()); sent != 0 {
		t.Errorf("%d réponse(s) envoyée(s), attendu 0 (le nom n'a pas été prononcé)", sent)
	}

	// Rien n'est conservé : le vocal non adressé n'a jamais existé.
	var count int
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages`).Scan(&count)
	}); err != nil {
		t.Fatalf("comptage: %v", err)
	}
	if count != 0 {
		t.Errorf("%d message(s) persisté(s), attendu 0", count)
	}
}

// Le nom prononcé dans le vocal vaut mention : l'assistant répond, sans
// qu'aucun texte n'ait été écrit dans le groupe.
func TestPipeline_GroupVoice_SpokenNameTriggersAReply(t *testing.T) {
	transcriber := &spyTranscriber{transcript: "dis-moi Assistant, quel temps fera-t-il samedi ?"}
	pipeline, provider, _ := newVoicePipeline(t, transcriber)
	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	deliverVoiceToGroup(t, provider)

	if !waitUntil(t, 3*time.Second, func() bool { return len(provider.Sent()) == 1 }) {
		t.Fatalf("%d réponse(s), une attendue (le nom a été prononcé)", len(provider.Sent()))
	}
}

// newVoicePipeline assemble un pipeline avec le vrai handler de
// conversation et un transcriber espionné : le circuit complet de la
// mention vocale, de l'ingress à la décision.
func newVoicePipeline(t *testing.T, transcriber *spyTranscriber) (*ingress.Pipeline, *readyProvider, *persistence.DB) {
	t.Helper()

	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Second}
	db := testDB(t)
	handler := conversation.NewHandler(db, fixedReplyAgent{}, nil, 0, audioCfg, transcriber, false, nil)

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

	return ingress.NewPipeline(testProviderName, provider, resolver, db, handler, testLogger(), nil), provider, db
}

func deliverVoiceToGroup(t *testing.T, provider *readyProvider) {
	t.Helper()

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannel("group-chan", courier.ChannelKindGroup, "Groupe"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessagePart(voiceNoteAttachment()),
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}
}
