package conversation_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

// attachmentsTestConfig autorise les images png et un document texte.
func attachmentsTestConfig() media.Config {
	return media.Config{
		Enabled:       true,
		MaxSize:       4096,
		MaxCount:      3,
		AcceptedTypes: []string{"image/png", "text/plain"},
		MaxHistory:    4,
		MaxReply:      2,
	}
}

// messageWithAttachment construit un message porteur d'un texte principal et
// d'une pièce jointe.
func messageWithAttachment(from, text, filename, contentType string, data []byte) courier.Message {
	open := func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}

	return courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("chan-1"),
		courier.NewUser(courier.UserID(from), from),
		courier.WithMessageMainPart(text),
		courier.WithMessagePart(courier.NewAttachment(filename, contentType, open, courier.WithAttachmentSize(int64(len(data))))),
	)
}

// TestHandler_AttachmentReachesAgent vérifie le cas nominal : l'image jointe
// au message parvient à l'agent, en plus du texte.
func TestHandler_AttachmentReachesAgent(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithAttachments(attachmentsTestConfig())

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-att"), "chan-1")

	data := []byte("octets png")
	msg := messageWithAttachment("alice", "C'est quoi ce champignon ?", "photo.png", "image/png", data)

	if _, _, err := h.Handle(context.Background(), identity, conv, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(a.requests) != 1 {
		t.Fatalf("exécutions de l'agent: got %d, expected 1", len(a.requests))
	}

	req := a.requests[0]
	if req.Input != "C'est quoi ce champignon ?" {
		t.Errorf("input = %q", req.Input)
	}
	if len(req.Attachments) != 1 {
		t.Fatalf("pièces jointes reçues par l'agent: got %d, expected 1", len(req.Attachments))
	}
	if got := req.Attachments[0]; !bytes.Equal(got.Data, data) || got.MimeType != "image/png" {
		t.Errorf("pièce jointe inattendue: mime=%q data=%q", got.MimeType, got.Data)
	}
}

// TestHandler_RejectedAttachmentIsAnnouncedToAgent vérifie qu'une pièce
// jointe écartée n'est jamais silencieuse : l'agent doit pouvoir l'expliquer
// plutôt que de répondre à côté d'un document qu'il n'a jamais vu.
func TestHandler_RejectedAttachmentIsAnnouncedToAgent(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithAttachments(attachmentsTestConfig())

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-rejet"), "chan-1")

	msg := messageWithAttachment("alice", "Tu peux lire ça ?", "contrat.pdf", "application/pdf", []byte("%PDF-1.4"))

	if _, _, err := h.Handle(context.Background(), identity, conv, msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	req := a.requests[0]
	if len(req.Attachments) != 0 {
		t.Fatalf("aucune pièce jointe ne devait être transmise, obtenu %d", len(req.Attachments))
	}
	if !strings.Contains(req.Input, "contrat.pdf") || !strings.Contains(req.Input, "non transmises") {
		t.Errorf("l'agent devrait être informé du rejet, input = %q", req.Input)
	}
}

// TestHandler_AttachmentReplayedFromHistory couvre la conservation : au tour
// suivant, l'image du tour précédent est rejouée dans l'historique, ce qui
// permet à l'utilisateur d'y faire référence (« et sur la photo d'avant ? »).
func TestHandler_AttachmentReplayedFromHistory(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithAttachments(attachmentsTestConfig())

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-replay"), "chan-1")

	data := []byte("octets png")
	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, messageWithAttachment("alice", "Regarde", "photo.png", "image/png", data)); err != nil {
		t.Fatalf("Handle (premier tour): %v", err)
	}

	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "Et sur la photo d'avant ?")); err != nil {
		t.Fatalf("Handle (second tour): %v", err)
	}

	if len(a.requests) != 2 {
		t.Fatalf("exécutions de l'agent: got %d, expected 2", len(a.requests))
	}

	second := a.requests[1]
	if len(second.Attachments) != 0 {
		t.Errorf("le second message ne porte aucune pièce jointe propre, obtenu %d", len(second.Attachments))
	}

	var replayed int
	for _, m := range second.History {
		replayed += len(m.Attachments)

		for _, att := range m.Attachments {
			if !bytes.Equal(att.Data, data) {
				t.Errorf("pièce jointe rejouée altérée: %q", att.Data)
			}
			if m.Role != "user" {
				t.Errorf("seuls les messages user peuvent porter des pièces jointes, rôle = %q", m.Role)
			}
		}
	}

	if replayed != 1 {
		t.Fatalf("pièces jointes rejouées depuis l'historique: got %d, expected 1", replayed)
	}
}

// TestHandler_HistoryAttachmentsNotReplayedWhenDisabled vérifie que le rejeu
// s'éteint avec max_history : sans borne, une conversation riche en images
// ferait croître indéfiniment chaque requête au modèle.
func TestHandler_HistoryAttachmentsNotReplayedWhenDisabled(t *testing.T) {
	db := openTestDB(t)

	cfg := attachmentsTestConfig()
	cfg.MaxHistory = 0

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).WithAttachments(cfg)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-nohist"), "chan-1")

	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, messageWithAttachment("alice", "Regarde", "photo.png", "image/png", []byte("png"))); err != nil {
		t.Fatalf("Handle (premier tour): %v", err)
	}
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "Alors ?")); err != nil {
		t.Fatalf("Handle (second tour): %v", err)
	}

	for _, m := range a.requests[1].History {
		if len(m.Attachments) != 0 {
			t.Fatalf("aucune pièce jointe ne devait être rejouée, obtenu %d", len(m.Attachments))
		}
	}
}

// TestHandler_ReplyAttachmentsForwarded vérifie la sortie : un média produit
// par l'agent est remonté à l'ingress pour être envoyé à l'utilisateur.
func TestHandler_ReplyAttachmentsForwarded(t *testing.T) {
	db := openTestDB(t)

	produced := media.Media{
		Kind:     media.KindImage,
		MimeType: "image/png",
		Filename: "graphique.png",
		Data:     []byte("octets du graphique"),
	}

	a := &producingAgent{attachments: []media.Media{produced}}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithAttachments(attachmentsTestConfig())

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-out"), "chan-1")

	reply, attachments, err := h.Handle(context.Background(), identity, conv, testMessage("alice", "Fais-moi un graphique"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if reply == "" {
		t.Error("la réponse textuelle devrait être conservée")
	}
	if len(attachments) != 1 {
		t.Fatalf("pièces jointes de la réponse: got %d, expected 1", len(attachments))
	}
	if !bytes.Equal(attachments[0].Data, produced.Data) {
		t.Errorf("média renvoyé altéré: %q", attachments[0].Data)
	}
}

// TestHandler_ReplyAttachmentsBounded vérifie qu'un outil prolixe n'inonde
// pas la conversation de l'utilisateur.
func TestHandler_ReplyAttachmentsBounded(t *testing.T) {
	db := openTestDB(t)

	many := make([]media.Media, 0, 5)
	for range 5 {
		many = append(many, media.Media{Kind: media.KindImage, MimeType: "image/png", Filename: "x.png", Data: []byte("x")})
	}

	a := &producingAgent{attachments: many}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil).
		WithAttachments(attachmentsTestConfig()) // MaxReply = 2

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-bound"), "chan-1")

	_, attachments, err := h.Handle(context.Background(), identity, conv, testMessage("alice", "Génère"))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(attachments) != 2 {
		t.Fatalf("pièces jointes de la réponse: got %d, expected 2 (borne max_reply)", len(attachments))
	}
}

// producingAgent est un agent.Agent de test qui joint des médias à sa
// réponse, comme le ferait un spécialiste ayant appelé un outil MCP.
type producingAgent struct {
	attachments []media.Media
}

func (a *producingAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	return agent.Result{Reply: "voici", Attachments: a.attachments}, nil
}

var _ agent.Agent = &producingAgent{}
