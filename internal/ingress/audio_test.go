package ingress_test

import (
	"bytes"
	"context"
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
	calls int
}

func (t *spyTranscriber) Transcribe(ctx context.Context, data []byte) (string, error) {
	t.calls++
	return "ne doit jamais être atteint", nil
}

var _ audio.Transcriber = &spyTranscriber{}

func voiceNoteAttachment() courier.MessagePart {
	return courier.NewAttachment("note.ogg", "audio/ogg", func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("faux octets audio"))), nil
	}, courier.WithAttachmentVoiceNote(2*time.Second))
}

// TestPipeline_GroupWithoutMention_VoiceNoteNeverRead vérifie que, pour un
// message de groupe sans mention de l'assistant, le pipeline s'arrête avant
// même d'atteindre le Handler : le Transcriber n'est donc jamais invoqué,
// même si le message contient une note vocale (PLAN.md Phase 9, critère de
// test "groupe sans mention audio ignoré avant transcription").
func TestPipeline_GroupWithoutMention_VoiceNoteNeverRead(t *testing.T) {
	transcriber := &spyTranscriber{}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Second}

	db := testDB(t)
	handler := conversation.NewHandler(db, fixedReplyAgent{}, nil, 0, audioCfg, transcriber, false)

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

	pipeline := ingress.NewPipeline(testProviderName, provider, resolver, db, handler, testLogger())

	stop := runPipeline(t, pipeline)
	defer stop()

	provider.waitReady(t)

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("group-chan"),
		courier.NewUser("alice-ext", "Alice"),
		courier.WithMessagePart(voiceNoteAttachment()),
	)

	if err := provider.Deliver(context.Background(), msg); err != nil {
		t.Fatalf("provider.Deliver: %v", err)
	}

	// Laisser le temps au pipeline de traiter (ou de ne pas traiter) le
	// message avant de vérifier l'absence d'effet.
	time.Sleep(100 * time.Millisecond)

	if transcriber.calls != 0 {
		t.Fatalf("le transcriber a été appelé %d fois, attendu 0 (message de groupe sans mention)", transcriber.calls)
	}

	if sent := len(provider.Sent()); sent != 0 {
		t.Errorf("%d réponse(s) envoyée(s), attendu 0", sent)
	}
}
