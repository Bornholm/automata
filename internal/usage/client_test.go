package usage

import (
	"context"
	"testing"

	"github.com/bornholm/genai/llm"
)

// fakeRecorder capture les traces enregistrées.
type fakeRecorder struct {
	records []Record
}

func (r *fakeRecorder) RecordUsage(_ context.Context, rec Record) {
	r.records = append(r.records, rec)
}

// fakeLLMClient implémente llm.Client avec des réponses fixes.
type fakeLLMClient struct {
	completionUsage llm.ChatCompletionUsage
	streamChunks    []llm.StreamChunk
}

func (c *fakeLLMClient) ChatCompletion(_ context.Context, _ ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, "ok"), c.completionUsage), nil
}

func (c *fakeLLMClient) ChatCompletionStream(_ context.Context, _ ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	out := make(chan llm.StreamChunk, len(c.streamChunks))
	for _, chunk := range c.streamChunks {
		out <- chunk
	}
	close(out)
	return out, nil
}

func (c *fakeLLMClient) Embeddings(_ context.Context, _ []string, _ ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	return nil, nil
}

func (c *fakeLLMClient) Transcription(_ context.Context, _ []byte, _ ...llm.TranscriptionOptionFunc) (llm.TranscriptionResponse, error) {
	return llm.NewTranscriptionResponse("bonjour", "fr", llm.NewTranscriptionUsage(80, 10, 90, 4.2)), nil
}

func testContext(recorder Recorder) context.Context {
	ctx := ContextWithRecorder(context.Background(), recorder)
	return ContextWithAttribution(ctx, Attribution{
		OrgID:          "famille-petit",
		PrincipalID:    "will",
		ConversationID: "conv-1",
		Component:      ComponentAgent,
		Agent:          "main",
	})
}

func TestWrapClient_RecordsChatCompletionWithCostAndAttribution(t *testing.T) {
	recorder := &fakeRecorder{}
	client := WrapClient(&fakeLLMClient{
		completionUsage: llm.NewChatCompletionUsageWithCost(100, 50, 150, 20, 0.0031, "USD"),
	}, "openrouter", "deepseek/x")

	if _, err := client.ChatCompletion(testContext(recorder)); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if len(recorder.records) != 1 {
		t.Fatalf("attendu 1 trace, obtenu %d", len(recorder.records))
	}

	rec := recorder.records[0]
	if rec.OrgID != "famille-petit" || rec.PrincipalID != "will" || rec.ConversationID != "conv-1" {
		t.Errorf("attribution inattendue: %+v", rec)
	}
	if rec.Component != ComponentAgent || rec.Agent != "main" || rec.Kind != KindChat {
		t.Errorf("composant/nature inattendus: %+v", rec)
	}
	if rec.Provider != "openrouter" || rec.Model != "deepseek/x" {
		t.Errorf("cible inattendue: %+v", rec)
	}
	if rec.PromptTokens != 100 || rec.CompletionTokens != 50 || rec.TotalTokens != 150 || rec.CachedTokens != 20 {
		t.Errorf("tokens inattendus: %+v", rec)
	}
	if !rec.CostReported || rec.CostAmount != 0.0031 || rec.CostCurrency != "USD" {
		t.Errorf("coût inattendu: %+v", rec)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt doit être renseigné")
	}
}

func TestWrapClient_RecordsTokensOnlyWhenNoCostReported(t *testing.T) {
	recorder := &fakeRecorder{}
	client := WrapClient(&fakeLLMClient{
		completionUsage: llm.NewChatCompletionUsage(10, 5, 15),
	}, "mistral", "mistral-small")

	if _, err := client.ChatCompletion(testContext(recorder)); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	rec := recorder.records[0]
	if rec.CostReported || rec.CostAmount != 0 {
		t.Errorf("aucun coût ne doit être rapporté: %+v", rec)
	}
	if rec.TotalTokens != 15 {
		t.Errorf("les tokens doivent rester enregistrés: %+v", rec)
	}
}

func TestWrapClient_RecordsStreamUsageAfterExhaustion(t *testing.T) {
	recorder := &fakeRecorder{}
	client := WrapClient(&fakeLLMClient{
		streamChunks: []llm.StreamChunk{
			llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "bon")),
			llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "jour")),
			llm.NewCompleteStreamChunk(llm.NewChatCompletionUsageWithCost(30, 12, 42, 0, 0.001, "USD")),
		},
	}, "openrouter", "deepseek/x")

	streaming, ok := client.(llm.ChatCompletionStreamingClient)
	if !ok {
		t.Fatal("le client décoré doit rester un ChatCompletionStreamingClient")
	}

	chunks, err := streaming.ChatCompletionStream(testContext(recorder))
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}

	count := 0
	for range chunks {
		count++
	}
	if count != 3 {
		t.Fatalf("les chunks doivent être relayés tels quels, attendu 3, obtenu %d", count)
	}

	if len(recorder.records) != 1 {
		t.Fatalf("attendu 1 trace après épuisement du flux, obtenu %d", len(recorder.records))
	}
	rec := recorder.records[0]
	if rec.Kind != KindChat || rec.TotalTokens != 42 || !rec.CostReported {
		t.Errorf("trace de flux inattendue: %+v", rec)
	}
}

func TestWrapClient_RecordsTranscription(t *testing.T) {
	recorder := &fakeRecorder{}
	client := WrapClient(&fakeLLMClient{}, "mistral", "voxtral")

	transcriber, ok := client.(llm.TranscriptionClient)
	if !ok {
		t.Fatal("le client décoré doit rester un TranscriptionClient")
	}

	ctx := ContextWithRecorder(context.Background(), recorder)
	ctx = ContextWithAttribution(ctx, Attribution{OrgID: "atelier", PrincipalID: "yann", Component: ComponentTranscription})

	if _, err := transcriber.Transcription(ctx, []byte("audio")); err != nil {
		t.Fatalf("Transcription: %v", err)
	}

	if len(recorder.records) != 1 {
		t.Fatalf("attendu 1 trace, obtenu %d", len(recorder.records))
	}
	rec := recorder.records[0]
	if rec.Kind != KindTranscription || rec.PromptTokens != 80 || rec.CompletionTokens != 10 || rec.TotalTokens != 90 {
		t.Errorf("trace de transcription inattendue: %+v", rec)
	}
	if rec.OrgID != "atelier" || rec.Component != ComponentTranscription {
		t.Errorf("attribution de transcription inattendue: %+v", rec)
	}
}

func TestWrapClient_NoRecorderIsTransparent(t *testing.T) {
	client := WrapClient(&fakeLLMClient{
		completionUsage: llm.NewChatCompletionUsage(1, 1, 2),
		streamChunks:    []llm.StreamChunk{llm.NewCompleteStreamChunk(llm.NewChatCompletionUsage(1, 1, 2))},
	}, "openrouter", "deepseek/x")

	// Aucun recorder dans le contexte : l'appel doit réussir sans paniquer
	// ni enregistrer quoi que ce soit.
	if _, err := client.ChatCompletion(context.Background()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	streaming := client.(llm.ChatCompletionStreamingClient)
	chunks, err := streaming.ChatCompletionStream(context.Background())
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	for range chunks {
	}
}

// fakeImageClient implémente llm.ImageGenerationClient.
type fakeImageClient struct{}

func (c *fakeImageClient) ImageGeneration(_ context.Context, _ string, _ ...llm.ImageGenerationOptionFunc) (llm.ImageGenerationResponse, error) {
	return llm.NewImageGenerationResponse(nil, llm.NewImageGenerationUsage(25, 1000, 1025)), nil
}

func TestWrapImageClient_RecordsGeneration(t *testing.T) {
	recorder := &fakeRecorder{}
	client := WrapImageClient(&fakeImageClient{}, "minimax", "image-01")

	if _, err := client.ImageGeneration(testContext(recorder), "un chat"); err != nil {
		t.Fatalf("ImageGeneration: %v", err)
	}

	if len(recorder.records) != 1 {
		t.Fatalf("attendu 1 trace, obtenu %d", len(recorder.records))
	}
	rec := recorder.records[0]
	if rec.Kind != KindImage || rec.TotalTokens != 1025 || rec.Provider != "minimax" {
		t.Errorf("trace d'image inattendue: %+v", rec)
	}
}
