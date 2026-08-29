package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/mcp"
	"github.com/bornholm/automata/internal/model"
)

// fakeImageGenerator rend une image PNG fixe, ou une erreur.
type fakeImageGenerator struct {
	err     error
	prompts []string
}

var pngPixel = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\x0d\n\x2d\xb4\x00\x00\x00\x00IEND\xaeB`\x82")

func (f *fakeImageGenerator) ImageGeneration(ctx context.Context, prompt string, funcs ...llm.ImageGenerationOptionFunc) (llm.ImageGenerationResponse, error) {
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return nil, f.err
	}
	return llm.NewImageGenerationResponse([]llm.GeneratedImage{
		llm.NewGeneratedImage(pngPixel, "image/png", ""),
	}, nil), nil
}

// L'image générée par l'outil doit remonter dans Result.Attachments : c'est
// ce qui la fait partir sur le canal. Un texte de confirmation sans image
// serait le pire des cas — l'assistant décrirait une image que personne ne
// reçoit.
func TestGenerateImage_AttachmentReachesResult(t *testing.T) {
	generator := &fakeImageGenerator{}

	cfg := &config.Config{
		Organization: config.Organization{ID: "home", DisplayName: "Maison"},
		Agents: map[string]config.Agent{
			"imagine": {
				Type:            config.AgentTypeSpecialist,
				SystemPrompt:    config.SystemPrompt{Content: "image specialist"},
				ImageGeneration: true,
			},
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "generate_image", `{"prompt":"a red apple","aspect_ratio":"1:1"}`)), nil
			}
			return scriptedFinalResponse("Voici ton image !"), nil
		},
	}

	// Construction directe du spécialiste, générateur factice injecté : le
	// registre réel passerait par BuildImageGenerationClient et le réseau.
	specialist := agent.NewMCPToolAgent(client, "system", "imagine", cfg, nil, nil, mcp.Limits{}, 3).
		WithExtraTools(agent.NewGenerateImageToolForTest(generator))

	res, err := specialist.Execute(context.Background(), agent.Request{
		Identity: model.ExecutionIdentity{PrincipalID: "alice", OrgID: "home"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(generator.prompts) != 1 || generator.prompts[0] != "a red apple" {
		t.Errorf("prompts reçus = %v", generator.prompts)
	}

	if len(res.Attachments) != 1 {
		t.Fatalf("attachments = %d, attendu 1 : l'image doit remonter jusqu'au canal", len(res.Attachments))
	}
	if res.Attachments[0].MimeType != "image/png" {
		t.Errorf("mime = %q, attendu image/png", res.Attachments[0].MimeType)
	}
	if string(res.Attachments[0].Data) != string(pngPixel) {
		t.Error("les octets de l'image ne correspondent pas")
	}
	if res.Reply != "Voici ton image !" {
		t.Errorf("reply = %q", res.Reply)
	}
}

// L'image générée part à l'utilisateur, jamais au modèle. La réinjecter
// exigerait un client acceptant les images en entrée : un modèle qui n'en
// accepte pas (« no endpoints found that support image input ») fait alors
// échouer le tour entier APRÈS une génération pourtant réussie et facturée.
func TestGenerateImage_IsNotSentBackToTheModel(t *testing.T) {
	generator := &fakeImageGenerator{}

	var attachmentsSeenByModel int

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "generate_image", `{"prompt":"a red apple"}`)), nil
			}

			for _, m := range opts.Messages {
				attachmentsSeenByModel += len(m.Attachments())
			}

			return scriptedFinalResponse("Voici ton image !"), nil
		},
	}

	cfg := &config.Config{Organization: config.Organization{ID: "home", DisplayName: "Maison"}}

	specialist := agent.NewMCPToolAgent(client, "system", "imagine", cfg, nil, nil, mcp.Limits{}, 3).
		WithExtraTools(agent.NewGenerateImageToolForTest(generator))

	res, err := specialist.Execute(context.Background(), agent.Request{
		Identity: model.ExecutionIdentity{PrincipalID: "alice", OrgID: "home"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if attachmentsSeenByModel != 0 {
		t.Errorf("le modèle a reçu %d pièce(s) jointe(s), attendu 0", attachmentsSeenByModel)
	}

	if len(res.Attachments) != 1 {
		t.Fatalf("attachments = %d, attendu 1 : l'image doit tout de même atteindre l'utilisateur", len(res.Attachments))
	}
}

// Un échec de génération ne fait pas échouer le tour : le modèle reçoit
// l'erreur en résultat d'outil et peut l'expliquer.
func TestGenerateImage_FailureIsExplainedNotFatal(t *testing.T) {
	generator := &fakeImageGenerator{err: context.DeadlineExceeded}

	var toolResultSeen string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "generate_image", `{"prompt":"a red apple"}`)), nil
			}
			for _, m := range opts.Messages {
				if m.Role() == llm.RoleTool {
					toolResultSeen = m.Content()
				}
			}
			return scriptedFinalResponse("Je n'ai pas réussi à générer l'image."), nil
		},
	}

	cfg := &config.Config{Organization: config.Organization{ID: "home", DisplayName: "Maison"}}
	specialist := agent.NewMCPToolAgent(client, "system", "imagine", cfg, nil, nil, mcp.Limits{}, 3).
		WithExtraTools(agent.NewGenerateImageToolForTest(generator))

	res, err := specialist.Execute(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Attachments) != 0 {
		t.Errorf("attachments = %d, attendu 0", len(res.Attachments))
	}
	if !strings.Contains(toolResultSeen, "image generation failed") {
		t.Errorf("résultat d'outil = %q, attendu l'explication de l'échec", toolResultSeen)
	}
}
