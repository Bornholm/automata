package audio_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/audio"
)

// fakeTranscriptionClient est un llm.TranscriptionClient de test : il
// enregistre s'il a été appelé (pour vérifier que la détection de format
// court-circuite l'appel réseau sur un format non reconnu) et retourne une
// réponse programmée.
type fakeTranscriptionClient struct {
	called bool
	text   string
	err    error
}

func (c *fakeTranscriptionClient) Transcription(ctx context.Context, data []byte, funcs ...llm.TranscriptionOptionFunc) (llm.TranscriptionResponse, error) {
	c.called = true
	if c.err != nil {
		return nil, c.err
	}
	return llm.NewTranscriptionResponse(c.text, "", nil), nil
}

var _ llm.TranscriptionClient = &fakeTranscriptionClient{}

// wavHeader est un en-tête RIFF/WAVE minimal, suffisant pour que
// llm.DetectAudioFormat le reconnaisse comme AudioFormatWAV.
var wavHeader = []byte("RIFF\x00\x00\x00\x00WAVEfmt ")

func TestGenAITranscriber_UnsupportedFormat(t *testing.T) {
	client := &fakeTranscriptionClient{text: "ne doit jamais être atteint"}
	transcriber := audio.NewGenAITranscriber(client)

	_, err := transcriber.Transcribe(context.Background(), []byte("pas un format audio reconnu"))
	if !errors.Is(err, audio.ErrUnsupportedFormat) {
		t.Fatalf("erreur attendue ErrUnsupportedFormat, obtenu %v", err)
	}
	if client.called {
		t.Fatal("le client de transcription n'aurait jamais dû être appelé pour un format non reconnu")
	}
}

func TestGenAITranscriber_Success(t *testing.T) {
	client := &fakeTranscriptionClient{text: "  bonjour le monde  "}
	transcriber := audio.NewGenAITranscriber(client)

	text, err := transcriber.Transcribe(context.Background(), wavHeader)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "bonjour le monde" {
		t.Fatalf("texte inattendu (trim attendu): %q", text)
	}
	if !client.called {
		t.Fatal("le client de transcription aurait dû être appelé")
	}
}

func TestGenAITranscriber_EmptyTranscription(t *testing.T) {
	client := &fakeTranscriptionClient{text: "   "}
	transcriber := audio.NewGenAITranscriber(client)

	_, err := transcriber.Transcribe(context.Background(), wavHeader)
	if !errors.Is(err, audio.ErrEmptyTranscription) {
		t.Fatalf("erreur attendue ErrEmptyTranscription, obtenu %v", err)
	}
}

func TestGenAITranscriber_ClientError(t *testing.T) {
	client := &fakeTranscriptionClient{err: errors.New("panne réseau")}
	transcriber := audio.NewGenAITranscriber(client)

	_, err := transcriber.Transcribe(context.Background(), wavHeader)
	if err == nil {
		t.Fatal("erreur attendue")
	}
}
