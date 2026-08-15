package conversation_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// recordingAgent est un agent.Agent de test qui journalise les requêtes
// reçues et retourne une réponse fixe (ou dynamique via replyFunc).
type recordingAgent struct {
	requests  []agent.Request
	replyFunc func(req agent.Request) string
}

func (a *recordingAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	a.requests = append(a.requests, req)
	reply := "ok"
	if a.replyFunc != nil {
		reply = a.replyFunc(req)
	}
	return agent.Result{Reply: reply}, nil
}

var _ agent.Agent = &recordingAgent{}

func openTestDB(t *testing.T) *persistence.DB {
	t.Helper()

	cfg := config.StorageApplication{
		Driver: "sqlite3",
		Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		Pragmas: config.Pragmas{
			ForeignKeys: true,
			JournalMode: "WAL",
			BusyTimeout: config.Duration(5 * time.Second),
		},
	}

	db, err := persistence.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func testConversation(id model.ConversationID, channelID string) model.Conversation {
	return model.Conversation{
		ID:        id,
		OrgID:     model.OrgID("home"),
		Provider:  "whatsapp",
		ChannelID: channelID,
		Kind:      model.ChannelPrivate,
		Scope:     model.ScopePersonal,
		ScopeID:   model.ScopeID("alice"),
	}
}

func testMessage(from, content string) courier.Message {
	return courier.NewMessage(
		courier.RandomMessageID(),
		courier.NewChannelRef("chan-1"),
		courier.NewUser(courier.UserID(from), from),
		courier.WithMessageMainPart(content),
	)
}

func TestHandler_HistoryIsolatedPerConversation(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false)

	identity := model.ExecutionIdentity{
		PrincipalID: model.PrincipalID("alice"),
	}

	convA := testConversation(model.ConversationID("conv-a"), "chan-a")
	convB := testConversation(model.ConversationID("conv-b"), "chan-b")

	ctx := context.Background()

	if _, err := h.Handle(ctx, identity, convA, testMessage("alice", "premier message A")); err != nil {
		t.Fatalf("Handle (A, 1): %v", err)
	}
	if _, err := h.Handle(ctx, identity, convB, testMessage("alice", "premier message B")); err != nil {
		t.Fatalf("Handle (B, 1): %v", err)
	}
	if _, err := h.Handle(ctx, identity, convA, testMessage("alice", "second message A")); err != nil {
		t.Fatalf("Handle (A, 2): %v", err)
	}

	// Le 3e appel (2e tour de la conversation A) doit voir l'historique de
	// A uniquement (1 message user + 1 réponse agent précédents), jamais
	// celui de B.
	lastReq := a.requests[len(a.requests)-1]
	if len(lastReq.History) != 2 {
		t.Fatalf("historique attendu de longueur 2 (isolé à la conversation A), obtenu %d: %+v", len(lastReq.History), lastReq.History)
	}
	for _, m := range lastReq.History {
		if m.Content == "premier message B" {
			t.Fatalf("l'historique de la conversation A contient un message de la conversation B: %+v", lastReq.History)
		}
	}
	if lastReq.History[0].Content != "premier message A" {
		t.Fatalf("premier message d'historique inattendu: %+v", lastReq.History[0])
	}
	if lastReq.History[0].Role != "user" {
		t.Fatalf("rôle du premier message d'historique inattendu: %q", lastReq.History[0].Role)
	}
	if lastReq.History[1].Role != "assistant" {
		t.Fatalf("rôle du second message d'historique inattendu: %q", lastReq.History[1].Role)
	}
}

func TestHandler_GroupAuthorAttribution(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false)

	conv := model.Conversation{
		ID:        model.ConversationID("conv-group"),
		OrgID:     model.OrgID("home"),
		Provider:  "whatsapp",
		ChannelID: "chan-group",
		Kind:      model.ChannelGroup,
		Scope:     model.ScopeGroup,
		ScopeID:   model.ScopeID("group-1"),
	}

	ctx := context.Background()

	// Deux principaux distincts s'expriment dans le même groupe.
	aliceIdentity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	bobIdentity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("bob")}

	if _, err := h.Handle(ctx, aliceIdentity, conv, testMessage("alice-ext", "message d'alice")); err != nil {
		t.Fatalf("Handle (alice): %v", err)
	}
	if _, err := h.Handle(ctx, bobIdentity, conv, testMessage("bob-ext", "message de bob")); err != nil {
		t.Fatalf("Handle (bob): %v", err)
	}

	messages := persistence.NewMessageRepository()

	var records []persistence.Message
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		records, err = messages.ListRecentByConversation(ctx, tx, conv.ID, 20)
		return err
	})
	if err != nil {
		t.Fatalf("ListRecentByConversation: %v", err)
	}

	// 2 tours = 4 messages persistés (user+assistant par tour).
	if len(records) != 4 {
		t.Fatalf("nombre de messages persistés inattendu: %d", len(records))
	}

	byContent := map[string]model.PrincipalID{}
	for _, m := range records {
		byContent[m.Content] = m.PrincipalID
	}

	if got := byContent["message d'alice"]; got != model.PrincipalID("alice") {
		t.Errorf("principal_id du message d'alice = %q, attendu alice", got)
	}
	if got := byContent["message de bob"]; got != model.PrincipalID("bob") {
		t.Errorf("principal_id du message de bob = %q, attendu bob", got)
	}
}
