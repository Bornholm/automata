package audio_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/audio"
)

// fakeAttachment est un courier.Attachment minimal pour les tests
// d'ExtractText : open produit le flux retourné par Reader.
type fakeAttachment struct {
	contentType string
	open        func(ctx context.Context) (io.ReadCloser, error)
}

func (a *fakeAttachment) Name() string                     { return "voice" }
func (a *fakeAttachment) ContentType() string              { return a.contentType }
func (a *fakeAttachment) Filename() string                 { return "voice.ogg" }
func (a *fakeAttachment) Size() int64                      { return -1 }
func (a *fakeAttachment) Disposition() courier.Disposition { return courier.DispositionAttachment }
func (a *fakeAttachment) Caption() string                  { return "" }
func (a *fakeAttachment) Reader(ctx context.Context) (io.ReadCloser, error) {
	return a.open(ctx)
}

var _ courier.Attachment = &fakeAttachment{}

// closeTrackingReader compte les appels à Close, pour vérifier qu'ExtractText
// referme toujours le flux qu'il ouvre.
type closeTrackingReader struct {
	io.Reader
	closed *bool
}

func (r *closeTrackingReader) Close() error {
	*r.closed = true
	return nil
}

// fakeTranscriber est un audio.Transcriber de test : il enregistre les
// octets reçus (pour vérifier qu'ils correspondent bien au flux borné) et
// retourne une réponse ou une erreur programmée.
type fakeTranscriber struct {
	called      bool
	receivedLen int
	reply       string
	err         error
}

func (t *fakeTranscriber) Transcribe(ctx context.Context, data []byte) (string, error) {
	t.called = true
	t.receivedLen = len(data)
	if t.err != nil {
		return "", t.err
	}
	return t.reply, nil
}

var _ audio.Transcriber = &fakeTranscriber{}

func newAttachment(data []byte, closed *bool) *fakeAttachment {
	return &fakeAttachment{
		contentType: "audio/ogg",
		open: func(ctx context.Context) (io.ReadCloser, error) {
			return &closeTrackingReader{Reader: bytes.NewReader(data), closed: closed}, nil
		},
	}
}

func TestExtractText_Success(t *testing.T) {
	closed := false
	attachment := newAttachment([]byte("petits octets audio"), &closed)

	transcriber := &fakeTranscriber{reply: "bonjour le monde"}

	cfg := audio.Config{Enabled: true, MaxSize: 1024, Timeout: time.Second}

	text, err := audio.ExtractText(context.Background(), cfg, transcriber, attachment)
	if err != nil {
		t.Fatalf("ExtractText: %v", err)
	}
	if text != "bonjour le monde" {
		t.Fatalf("texte inattendu: %q", text)
	}
	if !transcriber.called {
		t.Fatal("le transcriber n'a pas été appelé")
	}
	if !closed {
		t.Fatal("le flux de la pièce jointe n'a pas été refermé")
	}
}

func TestExtractText_TooLarge(t *testing.T) {
	closed := false
	data := bytes.Repeat([]byte("a"), 100)
	attachment := newAttachment(data, &closed)

	transcriber := &fakeTranscriber{reply: "ne doit jamais être atteint"}

	cfg := audio.Config{Enabled: true, MaxSize: 10, Timeout: time.Second}

	_, err := audio.ExtractText(context.Background(), cfg, transcriber, attachment)
	if !errors.Is(err, audio.ErrTooLarge) {
		t.Fatalf("erreur attendue ErrTooLarge, obtenu %v", err)
	}
	if transcriber.called {
		t.Fatal("le transcriber n'aurait jamais dû être appelé pour un audio trop volumineux")
	}
	if !closed {
		t.Fatal("le flux de la pièce jointe n'a pas été refermé")
	}
}

func TestExtractText_Timeout(t *testing.T) {
	closed := false
	attachment := &fakeAttachment{
		contentType: "audio/ogg",
		open: func(ctx context.Context) (io.ReadCloser, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	_ = closed

	transcriber := &fakeTranscriber{}

	cfg := audio.Config{Enabled: true, MaxSize: 1024, Timeout: 10 * time.Millisecond}

	_, err := audio.ExtractText(context.Background(), cfg, transcriber, attachment)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("erreur attendue context.DeadlineExceeded, obtenu %v", err)
	}
	if transcriber.called {
		t.Fatal("le transcriber n'aurait jamais dû être appelé après un timeout")
	}
}

func TestExtractText_Cancellation(t *testing.T) {
	attachment := &fakeAttachment{
		contentType: "audio/ogg",
		open: func(ctx context.Context) (io.ReadCloser, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	transcriber := &fakeTranscriber{}

	cfg := audio.Config{Enabled: true, MaxSize: 1024, Timeout: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := audio.ExtractText(ctx, cfg, transcriber, attachment)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur attendue context.Canceled, obtenu %v", err)
	}
	if transcriber.called {
		t.Fatal("le transcriber n'aurait jamais dû être appelé après une annulation")
	}
}

func TestExtractText_EmptyTranscription(t *testing.T) {
	closed := false
	attachment := newAttachment([]byte("octets audio"), &closed)

	transcriber := &fakeTranscriber{err: audio.ErrEmptyTranscription}

	cfg := audio.Config{Enabled: true, MaxSize: 1024, Timeout: time.Second}

	_, err := audio.ExtractText(context.Background(), cfg, transcriber, attachment)
	if !errors.Is(err, audio.ErrEmptyTranscription) {
		t.Fatalf("erreur attendue ErrEmptyTranscription, obtenu %v", err)
	}
}

func TestExtractText_UnsupportedFormat(t *testing.T) {
	closed := false
	attachment := newAttachment([]byte("pas un flux audio reconnu"), &closed)

	transcriber := &fakeTranscriber{err: audio.ErrUnsupportedFormat}

	cfg := audio.Config{Enabled: true, MaxSize: 1024, Timeout: time.Second}

	_, err := audio.ExtractText(context.Background(), cfg, transcriber, attachment)
	if !errors.Is(err, audio.ErrUnsupportedFormat) {
		t.Fatalf("erreur attendue ErrUnsupportedFormat, obtenu %v", err)
	}
}

func TestFindVoiceNote(t *testing.T) {
	voiceNote := courier.NewAttachment("note.ogg", "audio/ogg", func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}, courier.WithAttachmentVoiceNote(3*time.Second))

	plainAttachment := courier.NewAttachment("doc.pdf", "application/pdf", func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	})

	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("chan-1"),
		courier.NewUser("user-1", "User"),
		courier.WithMessagePart(plainAttachment),
		courier.WithMessagePart(voiceNote),
	)

	found, ok := audio.FindVoiceNote(msg)
	if !ok {
		t.Fatal("note vocale attendue trouvée")
	}
	if found.Filename() != "note.ogg" {
		t.Fatalf("pièce jointe inattendue trouvée: %q", found.Filename())
	}
}

func TestFindVoiceNote_None(t *testing.T) {
	msg := courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("chan-1"),
		courier.NewUser("user-1", "User"),
		courier.WithMessageMainPart("bonjour"),
	)

	_, ok := audio.FindVoiceNote(msg)
	if ok {
		t.Fatal("aucune note vocale ne devrait être trouvée")
	}
}
