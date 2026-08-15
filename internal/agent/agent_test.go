package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
)

// fakeStreamingClient est un client LLM de test implémentant
// llm.ChatCompletionStreamingClient sans réseau. streamFunc reçoit les
// options résolues (utile pour inspecter les messages envoyés) et retourne
// les chunks à émettre ainsi qu'une éventuelle erreur d'appel.
type fakeStreamingClient struct {
	streamFunc func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error)
	lastOpts   *llm.ChatCompletionOptions
}

func (f *fakeStreamingClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	opts := llm.NewChatCompletionOptions(funcs...)
	f.lastOpts = opts

	chunks, err := f.streamFunc(ctx, opts)
	if err != nil {
		return nil, err
	}

	ch := make(chan llm.StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range chunks {
			select {
			case <-ctx.Done():
				return
			case ch <- c:
			}
		}
	}()

	return ch, nil
}

var _ llm.ChatCompletionStreamingClient = &fakeStreamingClient{}

func TestGenAIAgent_Execute_SimpleReply(t *testing.T) {
	client := &fakeStreamingClient{
		streamFunc: func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error) {
			return []llm.StreamChunk{
				llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "Bonjour !")),
				llm.NewCompleteStreamChunk(llm.NewChatCompletionUsage(10, 2, 12)),
			}, nil
		},
	}

	a := agent.NewGenAIAgent(client, "Tu es un assistant.")

	result, err := a.Execute(context.Background(), agent.Request{
		Input: "Salut",
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if result.Reply != "Bonjour !" {
		t.Fatalf("réponse inattendue: %q", result.Reply)
	}

	if len(client.lastOpts.Messages) != 2 {
		t.Fatalf("nombre de messages envoyés inattendu: %d", len(client.lastOpts.Messages))
	}
	if client.lastOpts.Messages[0].Role() != llm.RoleSystem {
		t.Fatalf("le premier message doit être le system prompt, rôle=%s", client.lastOpts.Messages[0].Role())
	}
	if client.lastOpts.Messages[1].Content() != "Salut" {
		t.Fatalf("le dernier message doit être l'input utilisateur, contenu=%q", client.lastOpts.Messages[1].Content())
	}
}

func TestGenAIAgent_Execute_StreamingMultiChunk(t *testing.T) {
	client := &fakeStreamingClient{
		streamFunc: func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error) {
			return []llm.StreamChunk{
				llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "Bon")),
				llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "jour")),
				llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, " !")),
				llm.NewCompleteStreamChunk(llm.NewChatCompletionUsage(10, 3, 13)),
			}, nil
		},
	}

	a := agent.NewGenAIAgent(client, "system")

	result, err := a.Execute(context.Background(), agent.Request{
		History: []agent.Message{
			{Role: "user", Content: "Salut"},
			{Role: "assistant", Content: "Oui ?"},
		},
		Input: "Comment ça va ?",
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if result.Reply != "Bonjour !" {
		t.Fatalf("assemblage du texte incorrect: %q", result.Reply)
	}

	// system + 2 history + 1 input = 4
	if len(client.lastOpts.Messages) != 4 {
		t.Fatalf("nombre de messages envoyés inattendu: %d", len(client.lastOpts.Messages))
	}
}

func TestGenAIAgent_Execute_ContextCancelled(t *testing.T) {
	started := make(chan struct{})

	client := &fakeStreamingClient{
		streamFunc: func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error) {
			close(started)
			<-ctx.Done()
			return []llm.StreamChunk{
				llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "partiel")),
			}, nil
		},
	}

	a := agent.NewGenAIAgent(client, "system")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := a.Execute(ctx, agent.Request{Input: "Salut"})
		done <- err
	}()

	<-started
	cancel()

	err := <-done
	if err == nil {
		t.Fatal("erreur attendue après annulation du contexte")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur attendue enveloppant context.Canceled, obtenu: %v", err)
	}
}

func TestGenAIAgent_Execute_EmptyReply(t *testing.T) {
	client := &fakeStreamingClient{
		streamFunc: func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error) {
			return []llm.StreamChunk{
				llm.NewCompleteStreamChunk(llm.NewChatCompletionUsage(5, 0, 5)),
			}, nil
		},
	}

	a := agent.NewGenAIAgent(client, "system")

	_, err := a.Execute(context.Background(), agent.Request{Input: "Salut"})
	if !errors.Is(err, agent.ErrEmptyReply) {
		t.Fatalf("erreur ErrEmptyReply attendue, obtenu: %v", err)
	}
}

func TestGenAIAgent_Execute_ClientError(t *testing.T) {
	wantErr := errors.New("boom")

	client := &fakeStreamingClient{
		streamFunc: func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error) {
			return nil, wantErr
		},
	}

	a := agent.NewGenAIAgent(client, "system")

	_, err := a.Execute(context.Background(), agent.Request{Input: "Salut"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("erreur enveloppant %v attendue, obtenu: %v", wantErr, err)
	}
}

func TestGenAIAgent_Execute_StreamError(t *testing.T) {
	wantErr := errors.New("erreur de flux")

	client := &fakeStreamingClient{
		streamFunc: func(ctx context.Context, opts *llm.ChatCompletionOptions) ([]llm.StreamChunk, error) {
			return []llm.StreamChunk{
				llm.NewStreamChunk(llm.NewStreamDelta(llm.RoleAssistant, "début")),
				llm.NewErrorStreamChunk(wantErr),
			}, nil
		},
	}

	a := agent.NewGenAIAgent(client, "system")

	_, err := a.Execute(context.Background(), agent.Request{Input: "Salut"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("erreur enveloppant %v attendue, obtenu: %v", wantErr, err)
	}
}
