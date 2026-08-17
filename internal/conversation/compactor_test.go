package conversation_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/conversation"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// fakeSummarizerClient retourne un résumé fixe et compte ses appels, en
// capturant le dernier prompt utilisateur reçu.
type fakeSummarizerClient struct {
	mu         sync.Mutex
	calls      int
	summary    string
	lastPrompt string
}

func (c *fakeSummarizerClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)

	c.mu.Lock()
	c.calls++
	for _, m := range opts.Messages {
		if m.Role() == llm.RoleUser {
			c.lastPrompt = m.Content()
		}
	}
	c.mu.Unlock()

	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, c.summary), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

func (c *fakeSummarizerClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeSummarizerClient) LastPrompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPrompt
}

// seedConversation insère la conversation et count messages numérotés.
func seedConversation(t *testing.T, db *persistence.DB, conv model.Conversation, count int) {
	t.Helper()

	conversations := persistence.NewConversationRepository()
	messages := persistence.NewMessageRepository()

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := conversations.Insert(context.Background(), tx, persistence.Conversation{
			ID:                conv.ID,
			OrgID:             conv.OrgID,
			Provider:          conv.Provider,
			ExternalChannelID: conv.ChannelID,
			Kind:              conv.Kind,
			Scope:             conv.Scope,
			ScopeID:           conv.ScopeID,
			CreatedAt:         "2026-08-18T09:00:00Z",
			UpdatedAt:         "2026-08-18T09:00:00Z",
		}); err != nil {
			return err
		}

		for i := range count {
			if err := messages.Insert(context.Background(), tx, persistence.Message{
				ID:                fmt.Sprintf("msg-%03d", i),
				ConversationID:    conv.ID,
				ExternalMessageID: fmt.Sprintf("ext-%03d", i),
				PrincipalID:       "alice",
				Role:              "user",
				Content:           fmt.Sprintf("message numéro %d", i),
				ContentKind:       "text",
				CreatedAt:         "2026-08-18T09:00:00Z",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seedConversation: %v", err)
	}
}

func getSummary(t *testing.T, db *persistence.DB, convID model.ConversationID) (persistence.ConversationSummary, bool) {
	t.Helper()

	repo := persistence.NewConversationSummaryRepository()
	var (
		summary persistence.ConversationSummary
		found   bool
	)
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		summary, found, err = repo.Get(context.Background(), tx, convID)
		return err
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	return summary, found
}

func TestCompactor_CompactsBeyondThreshold(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	client := &fakeSummarizerClient{summary: "résumé des débuts"}
	compactor := conversation.NewCompactor(db, client, 2, 0, nil, nil).
		WithClock(func() time.Time { return time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC) })

	if err := compactor.CompactIfNeeded(context.Background(), conv.ID); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}

	summary, found := getSummary(t, db, conv.ID)
	if !found {
		t.Fatal("aucun résumé persisté")
	}
	if summary.Summary != "résumé des débuts" {
		t.Errorf("summary = %q, attendu le texte du client factice", summary.Summary)
	}
	// 5 messages non couverts, fenêtre de 2 : les 3 plus anciens sont
	// compactés, les 2 plus récents restent verbatim.
	if summary.MessagesCovered != 3 {
		t.Errorf("messages_covered = %d, attendu 3", summary.MessagesCovered)
	}

	// Le prompt contient les messages compactés, pas les récents.
	prompt := client.LastPrompt()
	if !strings.Contains(prompt, "message numéro 0") || strings.Contains(prompt, "message numéro 4") {
		t.Errorf("le prompt de résumé doit contenir les anciens messages et pas les récents ; prompt: %q", prompt)
	}

	// Second appel sans nouveaux messages : sous le seuil, aucun appel LLM.
	if err := compactor.CompactIfNeeded(context.Background(), conv.ID); err != nil {
		t.Fatalf("CompactIfNeeded (2e): %v", err)
	}
	if client.Calls() != 1 {
		t.Errorf("appels LLM = %d, attendu 1 (pas de recompaction sous le seuil)", client.Calls())
	}
}

func TestCompactor_MergesWithPreviousSummary(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	client := &fakeSummarizerClient{summary: "premier résumé"}
	compactor := conversation.NewCompactor(db, client, 2, 0, nil, nil)

	if err := compactor.CompactIfNeeded(context.Background(), conv.ID); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}

	// 4 nouveaux messages : 2 (restants) + 4 = 6 non couverts >= 4 (seuil).
	messages := persistence.NewMessageRepository()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		for i := 5; i < 9; i++ {
			if err := messages.Insert(context.Background(), tx, persistence.Message{
				ID:                fmt.Sprintf("msg-%03d", i),
				ConversationID:    conv.ID,
				ExternalMessageID: fmt.Sprintf("ext-%03d", i),
				PrincipalID:       "alice",
				Role:              "user",
				Content:           fmt.Sprintf("message numéro %d", i),
				ContentKind:       "text",
				CreatedAt:         "2026-08-18T09:30:00Z",
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	client.summary = "résumé fusionné"
	if err := compactor.CompactIfNeeded(context.Background(), conv.ID); err != nil {
		t.Fatalf("CompactIfNeeded (2e): %v", err)
	}

	if !strings.Contains(client.LastPrompt(), "premier résumé") {
		t.Errorf("le prompt de la seconde compaction doit inclure l'ancien résumé ; prompt: %q", client.LastPrompt())
	}

	summary, _ := getSummary(t, db, conv.ID)
	if summary.Summary != "résumé fusionné" {
		t.Errorf("summary = %q, attendu le résumé fusionné", summary.Summary)
	}
	if summary.MessagesCovered != 7 {
		t.Errorf("messages_covered = %d, attendu 7 (3 puis 4 de plus)", summary.MessagesCovered)
	}
}

func TestCompactor_TruncatesSummaryToMaxChars(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	client := &fakeSummarizerClient{summary: strings.Repeat("é", 100)}
	compactor := conversation.NewCompactor(db, client, 2, 10, nil, nil)

	if err := compactor.CompactIfNeeded(context.Background(), conv.ID); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}

	summary, _ := getSummary(t, db, conv.ID)
	if got := len([]rune(summary.Summary)); got != 10 {
		t.Errorf("longueur du résumé = %d runes, attendu 10 (tronqué)", got)
	}
}

func TestHandler_SummaryInjectedAndCoveredHistoryExcluded(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	// Résumé couvrant les trois premiers messages (rowid 1 à 3).
	summaries := persistence.NewConversationSummaryRepository()
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return summaries.Upsert(context.Background(), tx, persistence.ConversationSummary{
			ConversationID:   conv.ID,
			Summary:          "résumé de test",
			LastMessageRowID: 3,
			MessagesCovered:  3,
			UpdatedAt:        "2026-08-18T09:45:00Z",
		})
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	recorder := &recordingAgent{}
	handler := conversation.NewHandler(db, recorder, nil, 20, audio.Config{}, nil, false, nil)

	identity := model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    "alice",
		OrgID:          conv.OrgID,
		ConversationID: conv.ID,
		Provider:       conv.Provider,
		ChannelID:      conv.ChannelID,
		ChannelKind:    conv.Kind,
		Scope:          conv.Scope,
		ScopeID:        conv.ScopeID,
	}

	if _, _, err := handler.Handle(context.Background(), identity, conv, testMessage("alice-ext", "nouveau message")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(recorder.requests) != 1 {
		t.Fatalf("requêtes agent = %d, attendu 1", len(recorder.requests))
	}
	req := recorder.requests[0]

	if req.Summary != "résumé de test" {
		t.Errorf("Summary = %q, attendu le résumé persisté", req.Summary)
	}

	var contents []string
	for _, m := range req.History {
		contents = append(contents, m.Content)
	}

	// Seuls les messages postérieurs à la frontière (rowid > 3) sont rejoués.
	if strings.Contains(strings.Join(contents, "\n"), "message numéro 0") {
		t.Errorf("l'historique rejoue un message couvert par le résumé: %q", contents)
	}
	if !strings.Contains(strings.Join(contents, "\n"), "message numéro 3") {
		t.Errorf("l'historique doit contenir les messages non couverts: %q", contents)
	}
}
