package agent_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

func testImage(data string) media.Media {
	return media.Media{
		Kind:     media.KindImage,
		MimeType: "image/png",
		Filename: "photo.png",
		Data:     []byte(data),
	}
}

// TestOrchestratorAgent_AttachmentsReachTheModel vérifie que les pièces
// jointes du message courant sont bien portées par le message "user" envoyé
// au fournisseur — sans quoi le modèle ne verrait jamais l'image.
func TestOrchestratorAgent_AttachmentsReachTheModel(t *testing.T) {
	var userMessages []llm.Message

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, m := range opts.Messages {
				if m.Role() == llm.RoleUser {
					userMessages = append(userMessages, m)
				}
			}
			return scriptedFinalResponse("Je vois une photo."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{}, 5)

	_, err := a.Execute(context.Background(), agent.Request{
		Input:       "C'est quoi ?",
		Attachments: []media.Media{testImage("octets png")},
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if len(userMessages) != 1 {
		t.Fatalf("messages user envoyés au modèle: got %d, expected 1", len(userMessages))
	}

	attachments := userMessages[0].Attachments()
	if len(attachments) != 1 {
		t.Fatalf("pièces jointes portées par le message user: got %d, expected 1", len(attachments))
	}

	if attachments[0].Type() != llm.AttachmentTypeImage {
		t.Errorf("type de pièce jointe = %q, attendu image", attachments[0].Type())
	}

	decoded, err := base64.StdEncoding.DecodeString(attachments[0].Data())
	if err != nil {
		t.Fatalf("données non décodables: %v", err)
	}
	if string(decoded) != "octets png" {
		t.Errorf("données altérées: %q", decoded)
	}
}

// TestBuildChatMessages_HistoryAttachmentsOnlyOnUserMessages verrouille une
// contrainte des fournisseurs : ils REFUSENT la requête entière si un message
// system ou assistant porte une pièce jointe. Rejouer l'historique ne doit
// donc jamais en attacher à une réponse d'assistant.
func TestBuildChatMessages_HistoryAttachmentsOnlyOnUserMessages(t *testing.T) {
	var captured []llm.Message

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			captured = opts.Messages
			return scriptedFinalResponse("ok"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{}, 5)

	_, err := a.Execute(context.Background(), agent.Request{
		Input: "Et maintenant ?",
		History: []agent.Message{
			{Role: "user", Content: "Regarde", Attachments: []media.Media{testImage("image du tour 1")}},
			// Un média sur un tour assistant ne doit jamais être transmis,
			// même si la persistance en contenait un par erreur.
			{Role: "assistant", Content: "Je vois", Attachments: []media.Media{testImage("interdit")}},
		},
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	var sawUserAttachment bool

	for _, m := range captured {
		switch m.Role() {
		case llm.RoleUser:
			if len(m.Attachments()) > 0 {
				sawUserAttachment = true
			}
		case llm.RoleSystem, llm.RoleAssistant:
			if len(m.Attachments()) > 0 {
				t.Errorf("un message de rôle %q porte %d pièce(s) jointe(s) : les fournisseurs refusent la requête entière dans ce cas", m.Role(), len(m.Attachments()))
			}
		}
	}

	if !sawUserAttachment {
		t.Error("la pièce jointe du tour utilisateur précédent aurait dû être rejouée")
	}
}

// TestOrchestratorAgent_AttachmentsForwardedToSpecialist vérifie que les
// pièces jointes accompagnent la délégation : un modèle ne peut pas recopier
// une image dans relevant_input, donc sans cela un spécialiste ne pourrait
// jamais en voir une.
func TestOrchestratorAgent_AttachmentsForwardedToSpecialist(t *testing.T) {
	var received []media.Media

	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			received = req.Attachments
			return delegation.Result{Summary: "Événement repéré sur l'affiche."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_agenda", `{"goal":"Lire l'affiche"}`)), nil
			}
			return scriptedFinalResponse("C'est noté."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, 5)

	_, err := a.Execute(context.Background(), agent.Request{
		Input:       "Ajoute l'événement de cette affiche",
		Attachments: []media.Media{testImage("affiche")},
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if len(received) != 1 {
		t.Fatalf("pièces jointes reçues par le spécialiste: got %d, expected 1", len(received))
	}
	if string(received[0].Data) != "affiche" {
		t.Errorf("pièce jointe altérée: %q", received[0].Data)
	}
}

// TestOrchestratorAgent_SpecialistMediaReturnedToUser couvre le sens
// retour : un média produit par un spécialiste remonte jusqu'au résultat de
// l'orchestrateur, pour être envoyé à l'utilisateur.
func TestOrchestratorAgent_SpecialistMediaReturnedToUser(t *testing.T) {
	produced := media.Media{Kind: media.KindImage, MimeType: "image/png", Filename: "graphique.png", Data: []byte("graphique")}

	research := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "Voici le graphique.", Attachments: []media.Media{produced}}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_research", `{"goal":"Produire un graphique"}`)), nil
			}
			return scriptedFinalResponse("Le voici."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"research": research}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Fais un graphique"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if len(result.Attachments) != 1 {
		t.Fatalf("médias remontés: got %d, expected 1", len(result.Attachments))
	}
	if string(result.Attachments[0].Data) != "graphique" {
		t.Errorf("média altéré: %q", result.Attachments[0].Data)
	}
}

// Un modèle texte-seul (llm_clients.<nom>.vision: false) ne reçoit JAMAIS de
// pièce jointe — un fournisseur texte-seul rejette la requête entière dès
// qu'un message en contient — mais il est prévenu en texte de leur
// existence, et la délégation continue de les transporter vers un
// spécialiste qui les voit.
func TestOrchestratorAgent_TextOnlyModelNeverReceivesAttachments(t *testing.T) {
	var received []media.Media

	vision := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			received = req.Attachments
			return delegation.Result{Summary: "a poster for a concert on Friday"}, nil
		},
	}

	var modelAttachments int
	var inputSeen string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, m := range opts.Messages {
				modelAttachments += len(m.Attachments())
				if m.Role() == llm.RoleUser {
					inputSeen = m.Content()
				}
			}
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_vision", `{"goal":"describe the attached image"}`)), nil
			}
			return scriptedFinalResponse("C'est une affiche de concert vendredi."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"vision": vision}, 5).
		WithVision(false)

	history := []agent.Message{
		{Role: "user", Content: "Regarde", Attachments: []media.Media{testImage("image du tour 1")}},
		{Role: "assistant", Content: "Vu !"},
	}

	_, err := a.Execute(context.Background(), agent.Request{
		Identity:    model.ExecutionIdentity{PrincipalID: "alice", OrgID: "home"},
		History:     history,
		Input:       "Et celle-ci ?",
		Attachments: []media.Media{testImage("octets png")},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if modelAttachments != 0 {
		t.Errorf("le modèle a reçu %d pièce(s) jointe(s), attendu 0 (historique compris)", modelAttachments)
	}
	if !strings.Contains(inputSeen, "1 attachment(s) received (image/png)") {
		t.Errorf("le message user ne signale pas la pièce jointe : %q", inputSeen)
	}
	if len(received) != 1 || string(received[0].Data) != "octets png" {
		t.Errorf("le spécialiste a reçu %v, attendu la pièce jointe du tour", received)
	}
}
