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

// Incident de production du 2026-09-04 : une carte envoyée à 00:02, puis
// « C'est le quartier Malakoff ? » à 00:05. Le tour ne portait aucune pièce
// jointe, la délégation vers la vision a été refusée, et l'assistant a
// demandé de renvoyer la capture. L'image est pourtant encore dans
// l'historique rejoué : le spécialiste doit la recevoir à la place.
func TestOrchestratorAgent_AttachmentDependentSpecialistFallsBackToEarlierImages(t *testing.T) {
	vision := &attachmentDependentSpecialist{}
	vision.executeFunc = func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "La carte montre le 14e arrondissement."}, nil
	}

	a := agent.NewOrchestratorAgent(delegateOnceClient("delegate_to_vision"), "system", "main",
		map[string]delegation.Specialist{"vision": vision}, 5)

	history := []agent.Message{
		{Role: "user", Content: "Voici la carte.", Attachments: []media.Media{testImage("carte")}},
		{Role: "assistant", Content: "Bien reçue."},
	}

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "C'est le quartier Malakoff ?",
		History: history,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if vision.callCount() != 1 {
		t.Fatalf("le spécialiste devait être appelé une fois, %d appels", vision.callCount())
	}
	got := vision.calls[0].Attachments
	if len(got) != 1 || string(got[0].Data) != "carte" {
		t.Fatalf("l'image du message précédent devait être transmise comme pièce du tour: %+v", got)
	}
	if len(vision.calls[0].RecentAttachments) != 0 {
		t.Fatalf("un spécialiste sans capacité fichiers ne reçoit rien dans RecentAttachments: %+v", vision.calls[0].RecentAttachments)
	}
}

// Le repli ne vaut que pour ce que le spécialiste peut regarder : une vidéo
// réservée aux outils n'est pas une image, la délégation reste refusée.
func TestOrchestratorAgent_AttachmentDependentSpecialistIgnoresEarlierToolOnlyFiles(t *testing.T) {
	vision := &attachmentDependentSpecialist{}
	vision.executeFunc = func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
		t.Error("le spécialiste a été exécuté sur un fichier qu'il ne peut pas regarder")
		return delegation.Result{}, nil
	}

	a := agent.NewOrchestratorAgent(delegateOnceClient("delegate_to_vision"), "system", "main",
		map[string]delegation.Specialist{"vision": vision}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "Regarde ça",
		History: historyWithToolFile("video.mp4"),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if vision.callCount() != 0 {
		t.Fatalf("délégations exécutées: got %d, expected 0", vision.callCount())
	}
}

// Les pièces du tour courant gardent la priorité : le repli ne mélange pas
// la photo d'aujourd'hui avec celle d'hier.
func TestOrchestratorAgent_AttachmentDependentSpecialistPrefersCurrentAttachments(t *testing.T) {
	vision := &attachmentDependentSpecialist{}
	vision.executeFunc = func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "vu"}, nil
	}

	a := agent.NewOrchestratorAgent(delegateOnceClient("delegate_to_vision"), "system", "main",
		map[string]delegation.Specialist{"vision": vision}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:       "Et celle-ci ?",
		Attachments: []media.Media{testImage("nouvelle")},
		History: []agent.Message{
			{Role: "user", Content: "Voici la carte.", Attachments: []media.Media{testImage("ancienne")}},
			{Role: "assistant", Content: "Bien reçue."},
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := vision.calls[0].Attachments
	if len(got) != 1 || string(got[0].Data) != "nouvelle" {
		t.Fatalf("seule la pièce du tour courant devait partir: %+v", got)
	}
}

// L'orchestrateur doit savoir qu'une image envoyée plus tôt reste
// examinable : sans annonce, il répond « renvoie la capture » avant même
// d'essayer de déléguer.
func TestOrchestratorAgent_AnnouncesEarlierImagesWithAnAttachmentDependentSpecialist(t *testing.T) {
	vision := &attachmentDependentSpecialist{}

	var input string
	client := &fakeCompletionClient{
		responseFunc: func(_ int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleUser {
					input = msg.Content()
				}
			}
			return scriptedFinalResponse("Je délègue."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"vision": vision}, 5)

	history := historyWithToolFile("video.mp4")
	history[0].Attachments = append(history[0].Attachments, testImage("carte"))

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "C'est le quartier Malakoff ?",
		History: history,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(input, "photo.png") {
		t.Fatalf("l'image déjà reçue doit être annoncée: %q", input)
	}
	if strings.Contains(input, "video.mp4") {
		t.Fatalf("une vidéo qu'aucun spécialiste ne peut ouvrir ne doit pas être annoncée: %q", input)
	}
}
