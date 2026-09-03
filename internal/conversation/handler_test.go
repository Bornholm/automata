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
	// answeredWithoutTools reproduit un tour qui disposait d'outils et n'en
	// a appelé aucun — la condition structurelle de l'annotation de refus.
	answeredWithoutTools bool
}

func (a *recordingAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	a.requests = append(a.requests, req)
	reply := "ok"
	if a.replyFunc != nil {
		reply = a.replyFunc(req)
	}
	return agent.Result{Reply: reply, AnsweredWithoutTools: a.answeredWithoutTools}, nil
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

	// Le modèle doit tout de même savoir qu'un lien a été donné, et quoi
	// faire pour en produire un neuf. La consigne est en anglais, comme
	// tout ce qui s'adresse au modèle : en français, au milieu d'un fil
	// français, elle se lisait comme une réponse à recopier.
	var mentioned bool
	for _, m := range lastReq.History {
		if strings.Contains(m.Content, "open_profile_link") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Error("le caviardage a effacé jusqu'à la trace du lien")
	}
}

// Le marqueur de caviardage ne s'adresse qu'au modèle : une personne ne
// doit jamais le lire. Reproduit le défaut vu en production le
// 2026-09-03 — « Voici votre lien : [lien de profil déjà utilisé] » —
// où le modèle recopie ce qu'il voit dans l'historique au lieu d'appeler
// open_profile_link.
func TestHandler_CopiedRedactionMarkerIsRepairedWithAFreshLink(t *testing.T) {
	db := openTestDB(t)

	const firstLink = "https://automata.test/p/cw2vj0.bq7yaptagka09h9mnt7c"
	const freshLink = "https://automata.test/p/h07jrm.bq7yaptagka09h9mnt7c"

	// Un modèle qui, au second tour, recopie servilement la dernière
	// réponse de l'assistant telle qu'il la voit dans son historique.
	a := &recordingAgent{}
	a.replyFunc = func(req agent.Request) string {
		if len(req.History) == 0 {
			return "Voici votre lien : " + firstLink
		}
		last := req.History[len(req.History)-1].Content
		return "Voici votre lien : " + strings.TrimPrefix(last, "Voici votre lien : ")
	}

	h := conversation.NewHandler(db, a, nil, 10, audio.Config{}, nil, false, nil).
		WithBilling(fakeProfileLinks{url: freshLink})

	identity := model.ExecutionIdentity{
		PrincipalID: model.PrincipalID("alice"),
		OrgID:       model.OrgID("home"),
	}
	conv := testConversation(model.ConversationID("conv-a"), "chan-a")
	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "Mon profil")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}

	reply, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "Mon profil"))
	if err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	// Le modèle a bien recopié : c'est la situation qu'on répare.
	if strings.Contains(reply, "[") {
		t.Errorf("un marqueur est parti à l'utilisateur: %q", reply)
	}
	if !strings.Contains(reply, freshLink) {
		t.Errorf("aucun lien neuf dans la réponse: %q", reply)
	}
	// Et surtout pas l'ancien, qui est un secret déjà consommé.
	if strings.Contains(reply, firstLink) {
		t.Errorf("l'ancien lien a été renvoyé: %q", reply)
	}
}

// Sans générateur de liens, le marqueur ne doit pas non plus partir tel
// quel : la personne mérite de savoir que quelque chose a manqué.
func TestHandler_CopiedRedactionMarkerNeverReachesTheUser(t *testing.T) {
	db := openTestDB(t)

	const firstLink = "https://automata.test/p/cw2vj0.bq7yaptagka09h9mnt7c"

	a := &recordingAgent{}
	a.replyFunc = func(req agent.Request) string {
		if len(req.History) == 0 {
			return "Voici votre lien : " + firstLink
		}
		return req.History[len(req.History)-1].Content
	}

	// Aucun WithBilling : rien pour produire un lien de remplacement.
	h := conversation.NewHandler(db, a, nil, 10, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{
		PrincipalID: model.PrincipalID("alice"),
		OrgID:       model.OrgID("home"),
	}
	conv := testConversation(model.ConversationID("conv-b"), "chan-b")
	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "Mon profil")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}
	reply, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "Mon profil"))
	if err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	if strings.Contains(reply, "[") || strings.Contains(reply, "profile link") {
		t.Errorf("le marqueur est parti à l'utilisateur: %q", reply)
	}
	if !strings.Contains(reply, "redemande") {
		t.Errorf("la réponse ne dit pas quoi faire: %q", reply)
	}
}

// Un refus inventé — écrit sans qu'aucun outil ait été appelé — revient dans
// l'historique du tour suivant, où le modèle le recopie au lieu d'essayer.
// Il doit donc y revenir marqué. Reproduit la boucle observée en production
// le 2026-09-03, où sept tours d'affilée se sont terminés sans aucun appel
// d'outil sur une demande de lien de profil.
func TestHandler_ReplyWrittenWithoutToolsComesBackMarked(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{answeredWithoutTools: true}
	a.replyFunc = func(req agent.Request) string {
		return "Le service de profil n'est pas disponible en ce moment."
	}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-refus"), "chan-1")
	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "mon profil")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "et mon solde ?")); err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	lastReq := a.requests[len(a.requests)-1]

	var marked bool
	for _, m := range lastReq.History {
		if m.Role == "assistant" && strings.Contains(m.Content, "no tool was called") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("la réponse est revenue au modèle sans le constat: %+v", lastReq.History)
	}

	// Le message de la personne n'est jamais marqué : elle n'a pas d'outils
	// à appeler, et son texte lui appartient.
	for _, m := range lastReq.History {
		if m.Role == "user" && strings.Contains(m.Content, "no tool was called") {
			t.Errorf("un message de la personne a été marqué: %q", m.Content)
		}
	}
}

// Un tour qui a appelé un outil a observé quelque chose : sa réponse revient
// intacte, quoi qu'elle dise. Sans quoi la boucle qu'on enlève serait
// remplacée par une boucle de tentatives sur ce qui vient d'échouer.
func TestHandler_ReplyBackedByAToolCallIsNeverMarked(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{answeredWithoutTools: false}
	a.replyFunc = func(req agent.Request) string {
		return "Le service de profil n'est pas disponible en ce moment."
	}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-outil"), "chan-1")
	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "mon profil")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}
	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "et alors ?")); err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	for _, m := range a.requests[len(a.requests)-1].History {
		if strings.Contains(m.Content, "no tool was called") {
			t.Errorf("une réponse appuyée sur un appel d'outil a été marquée: %q", m.Content)
		}
	}
}

// Le constat ne s'adresse qu'au modèle. S'il le recopie dans sa réponse, la
// personne ne doit pas le lire — même leçon que le marqueur de caviardage,
// recopié tel quel en production le 2026-09-03.
func TestHandler_CopiedToollessMarkerNeverReachesTheUser(t *testing.T) {
	db := openTestDB(t)

	a := &recordingAgent{answeredWithoutTools: true}
	a.replyFunc = func(req agent.Request) string {
		if len(a.requests) == 1 {
			return "Le service est indisponible."
		}
		// Second tour : le modèle recopie ce qu'il voit dans l'historique.
		for _, m := range req.History {
			if strings.Contains(m.Content, "no tool was called") {
				return m.Content
			}
		}
		return "rien à recopier"
	}
	h := conversation.NewHandler(db, a, nil, 0, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}
	conv := testConversation(model.ConversationID("conv-recopie"), "chan-1")
	ctx := context.Background()

	if _, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "mon profil")); err != nil {
		t.Fatalf("Handle (1): %v", err)
	}
	reply, _, err := h.Handle(ctx, identity, conv, testMessage("alice", "et alors ?"))
	if err != nil {
		t.Fatalf("Handle (2): %v", err)
	}

	if strings.Contains(reply, "no tool was called") {
		t.Fatalf("le constat a été envoyé à la personne: %q", reply)
	}
}
