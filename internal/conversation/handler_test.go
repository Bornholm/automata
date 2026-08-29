package conversation_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
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

// deadlineRecordingClient note si l'appel LLM reçoit une échéance.
type deadlineRecordingClient struct {
	called      bool
	hadDeadline bool
	deadline    time.Time
}

func (c *deadlineRecordingClient) ChatCompletion(ctx context.Context, _ ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	c.called = true
	c.deadline, c.hadDeadline = ctx.Deadline()
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, "résumé"), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

// La compaction reçoit un budget de temps PROPRE : ses appels LLM (résumé,
// épisode, extraction de faits) partent sinon avec le contexte du tour, et
// un fournisseur resté muet a déjà consommé les cinq minutes du tour entier
// — avant même l'enregistrement du message entrant (incident du
// 2026-08-29). Le client doit donc voir une échéance même quand le contexte
// du tour n'en porte aucune.
func TestHandler_CompactionHasItsOwnDeadline(t *testing.T) {
	db := openTestDB(t)

	client := &deadlineRecordingClient{}
	compactor := conversation.NewCompactor(db, client, 1, 0, nil, nil)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 1, audio.Config{}, nil, false, nil).WithCompactor(compactor)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-a"), "chan-a")
	ctx := context.Background()

	// Premier tour : deux messages persistés (user + assistant) — le seuil
	// de compaction (historyLimit × 2) est atteint pour le tour suivant.
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "premier message")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "second message")); err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	if !client.called {
		t.Fatal("la compaction n'a pas eu lieu : le test ne prouve rien")
	}
	if !client.hadDeadline {
		t.Fatal("l'appel LLM de compaction doit porter une échéance propre")
	}
	if remaining := time.Until(client.deadline); remaining > 2*time.Minute {
		t.Errorf("échéance trop lointaine (%s) : le budget du tour reste exposé", remaining)
	}
}

func TestHandler_HistoryIsolatedPerConversation(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{
		PrincipalID: model.PrincipalID("alice"),
	}

	convA := testConversation(model.ConversationID("conv-a"), "chan-a")
	convB := testConversation(model.ConversationID("conv-b"), "chan-b")

	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, convA, testMessage("alice", "premier message A")); err != nil {
		t.Fatalf("Handle (A, 1): %v", err)
	}
	if _, _, err := h.Handle(ctx, identity, convB, testMessage("alice", "premier message B")); err != nil {
		t.Fatalf("Handle (B, 1): %v", err)
	}
	if _, _, err := h.Handle(ctx, identity, convA, testMessage("alice", "second message A")); err != nil {
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
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil)

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

	if _, _, err := h.Handle(ctx, aliceIdentity, conv, testMessage("alice-ext", "message d'alice")); err != nil {
		t.Fatalf("Handle (alice): %v", err)
	}
	if _, _, err := h.Handle(ctx, bobIdentity, conv, testMessage("bob-ext", "message de bob")); err != nil {
		t.Fatalf("Handle (bob): %v", err)
	}

	messages := persistence.NewMessageRepository(nil)

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

// Incident de production du 2026-08-23 : l'orchestrateur a cessé d'appeler
// open_profile_link et s'est mis à recopier un lien vu dans l'historique,
// périmé depuis une heure. Un lien de profil est un secret à usage unique :
// il ne doit pas revenir au modèle au tour suivant, ni y revenir
// indéfiniment ensuite.
func TestHandler_ProfileLinksNeverReturnToTheModel(t *testing.T) {
	db := openTestDB(t)

	const link = "https://automata.example.fr/p/cw2vj0.bq7yaptagka09h9mnt7c"

	a := &recordingAgent{}
	a.replyFunc = func(req agent.Request) string {
		return "Voici le lien vers ton profil : " + link + "\n\nIl est valable 15 minutes."
	}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-link"), "chan-1")
	ctx := context.Background()

	// Premier tour : l'agent répond avec un lien.
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "envoie-moi mon profil")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}
	// Second tour : ce lien ne doit plus figurer dans le contexte.
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "et mon solde ?")); err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	lastReq := a.requests[len(a.requests)-1]
	if len(lastReq.History) == 0 {
		t.Fatal("historique vide au second tour")
	}

	for _, m := range lastReq.History {
		if strings.Contains(m.Content, "cw2vj0") || strings.Contains(m.Content, "bq7yaptagka09h9mnt7c") {
			t.Errorf("un lien de profil est revenu au modèle: %q", m.Content)
		}
	}

	// Le modèle doit tout de même savoir qu'un lien a été donné, sans quoi
	// il ne comprendrait pas la suite de l'échange.
	var mentioned bool
	for _, m := range lastReq.History {
		if strings.Contains(m.Content, "lien de profil") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Error("le caviardage a effacé jusqu'à la trace du lien")
	}
}
