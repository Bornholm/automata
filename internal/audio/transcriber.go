package audio

import (
	"context"
	"fmt"
	"strings"

	"github.com/bornholm/genai/llm"
)

// GenAITranscriber enveloppe un llm.TranscriptionClient GenAI pour
// implémenter Transcriber : détection du format audio, appel du client,
// rejet des formats non reconnus et des transcriptions vides.
type GenAITranscriber struct {
	client llm.TranscriptionClient
}

// NewGenAITranscriber construit un GenAITranscriber adossé à client.
func NewGenAITranscriber(client llm.TranscriptionClient) *GenAITranscriber {
	return &GenAITranscriber{client: client}
}

// Transcribe implémente Transcriber.
func (t *GenAITranscriber) Transcribe(ctx context.Context, audio []byte) (string, error) {
	format := llm.DetectAudioFormat(audio)
	if format == "" {
		return "", ErrUnsupportedFormat
	}

	res, err := t.client.Transcription(ctx, audio, llm.WithAudioFormat(format))
	if err != nil {
		return "", fmt.Errorf("audio: appel du client de transcription: %w", err)
	}

	text := strings.TrimSpace(res.Text())
	if text == "" {
		return "", ErrEmptyTranscription
	}

	return text, nil
}

var _ Transcriber = &GenAITranscriber{}
