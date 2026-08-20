package conversation_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

const voiceNotePlaceholder = "[Message vocal transcrit pour traitement]"

// spyTranscriber est un audio.Transcriber de test qui enregistre s'il a été
// appelé (pour prouver qu'un message de groupe sans mention ne lit jamais le
// média) et retourne une réponse ou une erreur programmée.
type spyTranscriber struct {
	calls int
	reply string
	err   error
}

func (t *spyTranscriber) Transcribe(ctx context.Context, data []byte) (string, error) {
	t.calls++
	if t.err != nil {
		return "", t.err
	}
	return t.reply, nil
}

var _ audio.Transcriber = &spyTranscriber{}

// voiceNoteMessage construit un message courier composé uniquement d'une
// pièce jointe note vocale (aucune partie texte "main"), ce qui force
// GetMessageMainContent à ne rien trouver et déclenche le repli audio.
func voiceNoteMessage(from string, audioBytes []byte) courier.Message {
	return voiceNoteMessageWithOpener(from, func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(audioBytes)), nil
	})
}

// voiceNoteMessageWithOpener construit un message note vocale dont l'ouverture
// du flux est gouvernée par open, pour simuler un blocage sensible au
// contexte (timeout, annulation).
func voiceNoteMessageWithOpener(from string, open func(ctx context.Context) (io.ReadCloser, error)) courier.Message {
	part := courier.NewAttachment("note.ogg", "audio/ogg", open, courier.WithAttachmentVoiceNote(2*time.Second))

	return courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("chan-1"),
		courier.NewUser(courier.UserID(from), from),
		courier.WithMessagePart(part),
	)
}

// blockingUntilCtxDone est un opener d'attachment qui attend l'annulation du
// contexte (timeout ou annulation explicite) avant de retourner son erreur,
// pour exercer le comportement d'ExtractText sous contrainte de contexte.
func blockingUntilCtxDone(ctx context.Context) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func lastPersistedUserContent(t *testing.T, db *persistence.DB, convID model.ConversationID) string {
	t.Helper()

	messages := persistence.NewMessageRepository(nil)

	var records []persistence.Message
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		records, err = messages.ListRecentByConversation(context.Background(), tx, convID, 20)
		return err
	})
	if err != nil {
		t.Fatalf("ListRecentByConversation: %v", err)
	}

	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Role == "user" {
			return records[i].Content
		}
	}

	t.Fatal("aucun message user persisté")
	return ""
}

func TestHandler_VoiceNote_NotPersistedByDefault(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{reply: "bonjour, ceci est un test vocal"}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Second}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice"), "chan-voice")

	msg := voiceNoteMessage("alice", []byte("faux octets audio"))

	reply, _, err := h.Handle(context.Background(), identity, conv, msg)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("réponse inattendue: %q", reply)
	}

	if transcriber.calls != 1 {
		t.Fatalf("transcriber.calls = %d, attendu 1", transcriber.calls)
	}

	// L'agent doit recevoir le texte transcrit en Input, jamais l'octet brut
	// ni le texte neutre persisté.
	if len(a.requests) != 1 || a.requests[0].Input != "bonjour, ceci est un test vocal" {
		t.Fatalf("input agent inattendu: %+v", a.requests)
	}

	// Le message persisté en base doit contenir l'indication neutre, pas le
	// texte transcrit réel (PLAN.md §3.4, PersistTranscription=false).
	persisted := lastPersistedUserContent(t, db, conv.ID)
	if persisted != voiceNotePlaceholder {
		t.Fatalf("contenu persisté = %q, attendu %q", persisted, voiceNotePlaceholder)
	}
	if bytes.Contains([]byte(persisted), []byte("bonjour, ceci est un test vocal")) {
		t.Fatal("la transcription réelle ne doit jamais atteindre la base par défaut")
	}
}

func TestHandler_VoiceNote_PersistedWhenExplicitlyEnabled(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{reply: "texte transcrit à conserver"}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Second}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, true, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice-persist"), "chan-voice-persist")

	msg := voiceNoteMessage("alice", []byte("faux octets audio"))

	if _, _, err := h.Handle(context.Background(), identity, conv, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	persisted := lastPersistedUserContent(t, db, conv.ID)
	if persisted != "texte transcrit à conserver" {
		t.Fatalf("contenu persisté = %q, attendu le texte transcrit réel", persisted)
	}
}

func TestHandler_VoiceNote_UnsupportedFormat(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{err: audio.ErrUnsupportedFormat}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Second}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice-unsupported"), "chan-voice-unsupported")

	msg := voiceNoteMessage("alice", []byte("faux octets audio"))

	_, _, err := h.Handle(context.Background(), identity, conv, msg)
	if !errors.Is(err, audio.ErrUnsupportedFormat) {
		t.Fatalf("erreur attendue ErrUnsupportedFormat, obtenu %v", err)
	}
	if len(a.requests) != 0 {
		t.Fatal("l'agent n'aurait jamais dû être exécuté")
	}
}

func TestHandler_VoiceNote_TooLarge(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{reply: "ne doit jamais être atteint"}
	audioCfg := audio.Config{Enabled: true, MaxSize: 4, Timeout: time.Second}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice-toolarge"), "chan-voice-toolarge")

	msg := voiceNoteMessage("alice", []byte("beaucoup plus de 4 octets"))

	_, _, err := h.Handle(context.Background(), identity, conv, msg)
	if !errors.Is(err, audio.ErrTooLarge) {
		t.Fatalf("erreur attendue ErrTooLarge, obtenu %v", err)
	}
	if transcriber.calls != 0 {
		t.Fatal("le transcriber n'aurait jamais dû être appelé pour un audio trop volumineux")
	}
	if len(a.requests) != 0 {
		t.Fatal("l'agent n'aurait jamais dû être exécuté")
	}
}

func TestHandler_VoiceNote_Timeout(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{reply: "ne doit jamais être atteint"}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Nanosecond}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice-timeout"), "chan-voice-timeout")

	msg := voiceNoteMessageWithOpener("alice", blockingUntilCtxDone)

	_, _, err := h.Handle(context.Background(), identity, conv, msg)
	if err == nil {
		t.Fatal("erreur attendue (timeout)")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("erreur attendue liée à context.DeadlineExceeded, obtenu %v", err)
	}
	if len(a.requests) != 0 {
		t.Fatal("l'agent n'aurait jamais dû être exécuté")
	}
}

func TestHandler_VoiceNote_EmptyTranscription(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{err: audio.ErrEmptyTranscription}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Second}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice-empty"), "chan-voice-empty")

	msg := voiceNoteMessage("alice", []byte("faux octets audio"))

	_, _, err := h.Handle(context.Background(), identity, conv, msg)
	if !errors.Is(err, audio.ErrEmptyTranscription) {
		t.Fatalf("erreur attendue ErrEmptyTranscription, obtenu %v", err)
	}
	if len(a.requests) != 0 {
		t.Fatal("l'agent n'aurait jamais dû être exécuté")
	}
}

func TestHandler_VoiceNote_Cancellation(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	transcriber := &spyTranscriber{reply: "ne doit jamais être atteint"}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: time.Minute}

	h := conversation.NewHandler(db, a, nil, 0, audioCfg, transcriber, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-voice-cancel"), "chan-voice-cancel")

	msg := voiceNoteMessageWithOpener("alice", blockingUntilCtxDone)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := h.Handle(ctx, identity, conv, msg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur attendue context.Canceled, obtenu %v", err)
	}
	if len(a.requests) != 0 {
		t.Fatal("l'agent n'aurait jamais dû être exécuté")
	}
}

// Les trois échecs qui viennent de l'audio envoyé portent une explication
// destinée à l'utilisateur ; une panne du fournisseur de transcription, non.
// C'est cette distinction qui décide, plus haut, entre un conseil utile et
// « réessaie dans quelques instants ».
func TestHandler_VoiceNote_UserFacingExplanations(t *testing.T) {
	cases := []struct {
		name        string
		transcriber *spyTranscriber
		maxSize     int64
		explained   bool
		fragment    string
	}{
		{
			name:        "transcription vide",
			transcriber: &spyTranscriber{err: audio.ErrEmptyTranscription},
			maxSize:     1 << 20,
			explained:   true,
			fragment:    "rien entendu",
		},
		{
			name:        "format inconnu",
			transcriber: &spyTranscriber{err: audio.ErrUnsupportedFormat},
			maxSize:     1 << 20,
			explained:   true,
			fragment:    "format audio",
		},
		{
			name:        "audio trop volumineux",
			transcriber: &spyTranscriber{reply: "jamais atteint"},
			maxSize:     4,
			explained:   true,
			fragment:    "trop long",
		},
		{
			name:        "panne du fournisseur",
			transcriber: &spyTranscriber{err: errors.New("503 service unavailable")},
			maxSize:     1 << 20,
			explained:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)

			audioCfg := audio.Config{Enabled: true, MaxSize: tc.maxSize, Timeout: time.Second}
			h := conversation.NewHandler(db, &recordingAgent{}, nil, 0, audioCfg, tc.transcriber, false, nil)

			identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
			conv := testConversation(model.ConversationID("conv-"+tc.name), "chan-"+tc.name)

			_, _, err := h.Handle(context.Background(), identity, conv, voiceNoteMessage("alice", []byte("beaucoup plus de 4 octets")))
			if err == nil {
				t.Fatal("erreur attendue")
			}

			reply, explained := apperr.UserReply(err)
			if explained != tc.explained {
				t.Fatalf("explication utilisateur = %v (%q), attendu %v", explained, reply, tc.explained)
			}
			if tc.explained && !strings.Contains(reply, tc.fragment) {
				t.Errorf("explication = %q, attendu qu'elle mentionne %q", reply, tc.fragment)
			}
		})
	}
}
