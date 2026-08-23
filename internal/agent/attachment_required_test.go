package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
)

// attachmentDependentSpecialist déclare la capacité qui intéresse
// l'orchestrateur : ce spécialiste n'a rien à faire sans pièce jointe.
type attachmentDependentSpecialist struct {
	fakeSpecialist
}

func (s *attachmentDependentSpecialist) RequiresAttachments() bool { return true }

var _ delegation.AttachmentDependent = &attachmentDependentSpecialist{}

// Incident de production du 2026-08-23 : un message ambigu (« Mon profil »)
// a déclenché une délégation vers la vision alors qu'aucune image n'avait
// été envoyée. Le modèle multimodal, sollicité à vide, a décrit un
// petit-déjeuner qui n'existait pas, et l'orchestrateur l'a relayé.
// Le spécialiste ne doit même pas être appelé.
func TestOrchestratorAgent_AttachmentDependentSpecialistRefusedWithoutFiles(t *testing.T) {
	vision := &attachmentDependentSpecialist{}
	vision.executeFunc = func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
		t.Error("le spécialiste a été exécuté alors qu'aucune pièce jointe n'accompagne le tour")
		return delegation.Result{Summary: "un petit-déjeuner très appétissant"}, nil
	}

	var toolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_vision", `{"goal":"Décris l'image"}`)), nil
			}
			// Le refus doit parvenir au modèle en clair : c'est ce qui lui
			// permet de répondre honnêtement au lieu de broder.
			for _, m := range opts.Messages {
				if m.Role() == llm.RoleTool {
					toolResult = m.Content()
				}
			}
			return scriptedFinalResponse("Je n'ai pas d'image à regarder."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"vision": vision}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Mon profil"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if vision.callCount() != 0 {
		t.Errorf("délégations exécutées: got %d, expected 0", vision.callCount())
	}
	if !strings.Contains(toolResult, "aucun") {
		t.Errorf("le modèle n'a pas été prévenu de l'absence de pièce jointe: %q", toolResult)
	}
	if result.Reply == "" {
		t.Error("le tour doit se conclure normalement malgré le refus")
	}
}

// Le refus ne doit pas déborder : dès qu'une image accompagne le tour, la
// délégation se déroule comme avant.
func TestOrchestratorAgent_AttachmentDependentSpecialistRunsWithFiles(t *testing.T) {
	vision := &attachmentDependentSpecialist{}
	vision.executeFunc = func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "Une affiche de concert, le 4 mai."}, nil
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_vision", `{"goal":"Décris l'image"}`)), nil
			}
			return scriptedFinalResponse("C'est une affiche de concert."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"vision": vision}, 5)

	_, err := a.Execute(context.Background(), agent.Request{
		Input:       "C'est quoi cette affiche ?",
		Attachments: []media.Media{testImage("affiche")},
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if vision.callCount() != 1 {
		t.Errorf("délégations exécutées: got %d, expected 1", vision.callCount())
	}
}
