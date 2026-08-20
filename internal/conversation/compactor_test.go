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
	"github.com/bornholm/automata/internal/memory"
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
	messages := persistence.NewMessageRepository(nil)

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

	repo := persistence.NewConversationSummaryRepository(nil)
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

	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
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
	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
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

	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}

	// 4 nouveaux messages : 2 (restants) + 4 = 6 non couverts >= 4 (seuil).
	messages := persistence.NewMessageRepository(nil)
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
	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
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

	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
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
	summaries := persistence.NewConversationSummaryRepository(nil)
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

// scriptedClient retourne des réponses pré-écrites dans l'ordre des appels :
// nécessaire dès que la compaction enchaîne résumé PUIS extraction de faits,
// deux appels aux réponses de formes différentes.
type scriptedClient struct {
	mu        sync.Mutex
	responses []string
	calls     int
}

func (c *scriptedClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.calls >= len(c.responses) {
		return llm.ChatCompletionResponse(nil), fmt.Errorf("scriptedClient: appel %d inattendu", c.calls+1)
	}
	response := c.responses[c.calls]
	c.calls++

	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, response), llm.NewChatCompletionUsage(1, 1, 2)), nil
}

// recordingMemoryStore implémente memory.Store en ne retenant que les
// appels à Remember : suffisant pour vérifier ce que l'extraction écrit.
type recordingMemoryStore struct {
	mu         sync.Mutex
	remembered []memory.NewMemory
}

func (s *recordingMemoryStore) Remember(ctx context.Context, mem memory.NewMemory) (memory.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remembered = append(s.remembered, mem)
	return memory.Memory{ID: fmt.Sprintf("mem-%d", len(s.remembered)), Content: mem.Content}, nil
}

func (s *recordingMemoryStore) Search(ctx context.Context, q memory.Query) ([]memory.Memory, error) {
	return nil, nil
}

func (s *recordingMemoryStore) GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (memory.Memory, bool, error) {
	return memory.Memory{}, false, nil
}

func (s *recordingMemoryStore) Forget(ctx context.Context, id string) error { return nil }
func (s *recordingMemoryStore) Reindex(ctx context.Context) error           { return nil }
func (s *recordingMemoryStore) List(ctx context.Context) ([]memory.Memory, error) {
	return nil, nil
}

var _ memory.Store = &recordingMemoryStore{}

func TestCompactor_ExtractsDurableFacts(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	client := &scriptedClient{responses: []string{
		"résumé des débuts",
		"```json\n[\"Alice préfère les rappels le soir\", \"  \", \"Alice part en vacances le 3 septembre\"]\n```",
	}}
	store := &recordingMemoryStore{}

	compactor := conversation.NewCompactor(db, client, 2, 0, nil, nil).
		WithClock(func() time.Time { return time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC) }).
		WithMemoryStore(store, 0)

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}

	if err := compactor.CompactIfNeeded(context.Background(), identity, conv); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}

	if _, found := getSummary(t, db, conv.ID); !found {
		t.Fatal("aucun résumé persisté")
	}

	store.mu.Lock()
	remembered := store.remembered
	store.mu.Unlock()

	if len(remembered) != 2 {
		t.Fatalf("faits mémorisés = %d, attendu 2 (la chaîne vide doit être écartée)", len(remembered))
	}

	first := remembered[0]
	if first.Content != "Alice préfère les rappels le soir" {
		t.Errorf("contenu = %q, attendu le premier fait", first.Content)
	}
	if first.Scope != conv.Scope || first.ScopeID != conv.ScopeID || first.OrgID != conv.OrgID {
		t.Errorf("portée = %s/%s/%s, attendu celle de la conversation", first.OrgID, first.Scope, first.ScopeID)
	}
	if first.OwnerPrincipalID != identity.PrincipalID || first.CreatedBy != identity.PrincipalID {
		t.Errorf("auteur = %s/%s, attendu le principal du tour", first.OwnerPrincipalID, first.CreatedBy)
	}
	if first.Origin != "compaction" {
		t.Errorf("origin = %q, attendu \"compaction\"", first.Origin)
	}
	if first.SourceConversationID != conv.ID {
		t.Errorf("source = %q, attendu %q", first.SourceConversationID, conv.ID)
	}
}

func TestCompactor_ExtractionFailureDoesNotFailCompaction(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	// Seconde réponse illisible : l'extraction échoue, la compaction non.
	client := &scriptedClient{responses: []string{"résumé", "pas du JSON"}}
	store := &recordingMemoryStore{}

	compactor := conversation.NewCompactor(db, client, 2, 0, nil, nil).
		WithClock(func() time.Time { return time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC) }).
		WithMemoryStore(store, 0)

	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
		t.Fatalf("CompactIfNeeded doit réussir malgré l'échec d'extraction: %v", err)
	}

	if _, found := getSummary(t, db, conv.ID); !found {
		t.Fatal("aucun résumé persisté")
	}
	if len(store.remembered) != 0 {
		t.Errorf("faits mémorisés = %d, attendu 0", len(store.remembered))
	}
}

// recordingEpisodeStore capture les épisodes enregistrés.
type recordingEpisodeStore struct {
	mu       sync.Mutex
	err      error
	recorded []memory.NewEpisode
}

func (s *recordingEpisodeStore) RecordEpisode(ctx context.Context, ep memory.NewEpisode) (memory.Episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return memory.Episode{}, s.err
	}
	s.recorded = append(s.recorded, ep)
	return memory.Episode{ID: "ep-1", Content: ep.Content}, nil
}

func (s *recordingEpisodeStore) SearchEpisodes(ctx context.Context, query memory.EpisodeQuery) ([]memory.Episode, error) {
	return nil, nil
}

var _ memory.EpisodeStore = &recordingEpisodeStore{}

// Le fragment condensé doit être conservé VERBATIM, étiqueté par nom
// affiché — jamais par identifiant interne : le contenu d'un épisode est
// restitué tel quel au modèle par search_conversation_history.
func TestCompactor_RecordsEpisodeVerbatim(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	client := &fakeSummarizerClient{summary: "résumé"}
	store := &recordingEpisodeStore{}

	compactor := conversation.NewCompactor(db, client, 2, 0, nil, nil).
		WithClock(func() time.Time { return time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC) }).
		WithEpisodeStore(store, func(principalID model.PrincipalID) string {
			if principalID == "alice" {
				return "Alice Petit"
			}
			return ""
		})

	identity := model.ExecutionIdentity{PrincipalID: model.PrincipalID("alice")}

	if err := compactor.CompactIfNeeded(context.Background(), identity, conv); err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}

	store.mu.Lock()
	recorded := store.recorded
	store.mu.Unlock()

	if len(recorded) != 1 {
		t.Fatalf("épisodes enregistrés = %d, attendu 1", len(recorded))
	}

	ep := recorded[0]
	if !strings.Contains(ep.Content, "Alice Petit: message numéro 0") {
		t.Errorf("contenu inattendu: %q", ep.Content)
	}
	if strings.Contains(ep.Content, "alice") {
		t.Errorf("l'identifiant interne du principal fuit dans l'épisode: %q", ep.Content)
	}
	if !strings.Contains(ep.Content, "[2026-08-18 09:00]") {
		t.Errorf("horodatage absent: %q", ep.Content)
	}
	if ep.Scope != conv.Scope || ep.ScopeID != conv.ScopeID || ep.OrgID != conv.OrgID {
		t.Errorf("portée = %s/%s/%s, attendu celle de la conversation", ep.OrgID, ep.Scope, ep.ScopeID)
	}
	if ep.ConversationID != conv.ID {
		t.Errorf("conversation = %q, attendu %q", ep.ConversationID, conv.ID)
	}
	if ep.From.IsZero() || ep.To.IsZero() {
		t.Errorf("bornes temporelles absentes: from=%v to=%v", ep.From, ep.To)
	}
}

// Un échec d'enregistrement d'épisode ne fait pas échouer la compaction :
// le résumé est déjà persisté, l'épisode de cette vague est simplement
// perdu.
func TestCompactor_EpisodeFailureDoesNotFailCompaction(t *testing.T) {
	db := openTestDB(t)
	conv := testConversation("conv-1", "chan-1")
	seedConversation(t, db, conv, 5)

	client := &fakeSummarizerClient{summary: "résumé"}
	store := &recordingEpisodeStore{err: context.DeadlineExceeded}

	compactor := conversation.NewCompactor(db, client, 2, 0, nil, nil).
		WithClock(func() time.Time { return time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC) }).
		WithEpisodeStore(store, nil)

	if err := compactor.CompactIfNeeded(context.Background(), model.ExecutionIdentity{}, conv); err != nil {
		t.Fatalf("CompactIfNeeded aurait dû réussir malgré l'échec de l'épisode: %v", err)
	}

	if _, found := getSummary(t, db, conv.ID); !found {
		t.Fatal("le résumé aurait dû être persisté")
	}
}
