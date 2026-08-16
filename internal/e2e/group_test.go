package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
)

// TestGroup_MessageWithoutMentionIgnored vérifie le scénario "message sans
// mention" : un message de groupe où l'assistant n'est pas mentionné est
// ignoré par l'ingress AVANT tout appel LLM (PLAN.md §3.3).
func TestGroup_MessageWithoutMentionIgnored(t *testing.T) {
	cfg := baseOrgConfig()

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("ne devrait jamais être appelé"), nil
		},
	}

	sys := newTestSystem(t, cfg, simpleAgent(client))

	sys.sendGroup("alice-ext", "group-chan", "bonjour sans mention", false)

	time.Sleep(150 * time.Millisecond)

	if got := client.callCount(); got != 0 {
		t.Errorf("appels LLM = %d, attendu 0 (message ignoré)", got)
	}
	if got := len(sys.provider.Sent()); got != 0 {
		t.Errorf("messages envoyés = %d, attendu 0", got)
	}
}

// TestGroup_MessageWithMentionProcessed vérifie le scénario "message avec
// mention" : traité normalement, une réponse est envoyée.
func TestGroup_MessageWithMentionProcessed(t *testing.T) {
	cfg := baseOrgConfig()

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Salut le groupe !"), nil
		},
	}

	sys := newTestSystem(t, cfg, simpleAgent(client))

	sys.sendGroup("alice-ext", "group-chan", "@assistant bonjour", true)

	sent := sys.waitSent(1)
	if got := mainContent(t, sent[0]); got != "Salut le groupe !" {
		t.Errorf("contenu de la réponse: got %q, expected %q", got, "Salut le groupe !")
	}
}

// TestGroup_Memory vérifie le scénario "mémoire du groupe" : remember/
// search_memory sont scopés au groupe courant, jamais à un principal
// individuel.
func TestGroup_Memory(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		switch turn {
		case 0:
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "remember", `{"content":"La corvée de vaisselle est pour Bob ce soir."}`)), nil
		case 1:
			return scriptedFinalResponse("Noté pour le groupe."), nil
		case 2:
			return scriptedToolCallResponse(llm.NewToolCall("call-2", "search_memory", `{"query":"vaisselle"}`)), nil
		default:
			return scriptedFinalResponse("Info groupe : " + lastToolResultText(opts)), nil
		}
	}

	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	sys.sendGroup("alice-ext", "group-chan", "@assistant souviens-toi de la vaisselle", true)
	sys.waitSent(1)

	sys.sendGroup("bob-ext", "group-chan", "@assistant qui fait la vaisselle ?", true)
	sent := sys.waitSent(2)

	got := mainContent(t, sent[1])
	if !strings.Contains(got, "vaisselle") {
		t.Fatalf("la mémoire du groupe devrait être retrouvée, obtenu: %q", got)
	}

	// La mémoire doit bien être scopée au groupe, jamais au principal
	// individuel qui l'a créée.
	results, err := store.Search(context.Background(), memory.Query{Text: "vaisselle", OrgID: "home", Scope: model.ScopeGroup, ScopeID: "home-group", MaxResults: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("mémoire de groupe attendue en portée group/home-group, got %d résultat(s)", len(results))
	}
}

// TestGroup_MemoryOrgAccessible vérifie le scénario "mémoire org" : une
// mémoire org est accessible depuis le groupe, selon les permissions du
// principal courant.
func TestGroup_MemoryOrgAccessible(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	if _, err := store.Remember(context.Background(), memory.NewMemory{
		Content: "Le code du portail est 4821.",
		Scope:   model.ScopeOrg, ScopeID: "home", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice",
	}); err != nil {
		t.Fatalf("seed org memory: %v", err)
	}

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "search_memory", `{"query":"portail"}`)), nil
		}
		return scriptedFinalResponse("Résultat : " + lastToolResultText(opts)), nil
	}

	// leo n'a que memory.org.read : accessible même depuis un groupe dont il
	// est membre (readScopes couvre group+org, mais leo n'a que org).
	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	sys.sendGroup("leo-ext", "group-chan", "@assistant quel est le code du portail ?", true)
	sent := sys.waitSent(1)

	got := mainContent(t, sent[0])
	if !strings.Contains(got, "4821") {
		t.Fatalf("résultat org attendu dans la réponse de groupe, obtenu: %q", got)
	}
}

// TestGroup_Agenda vérifie le scénario "agenda du groupe" : la lecture du
// calendrier depuis un groupe est résolue vers la ressource "calendar" du
// canal de groupe.
func TestGroup_Agenda(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := baseOrgConfig()
	withCalendarResources(cfg, httpServer.URL)

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"from":"2026-09-01","to":"2026-09-30"}`)), nil
		}
		return scriptedFinalResponse("Rien de prévu pour le groupe."), nil
	}

	sys := newTestSystem(t, cfg, mustAgendaAgent(t, cfg, client))

	sys.sendGroup("alice-ext", "group-chan", "@assistant qu'est-ce qu'on a de prévu ?", true)
	sys.waitSent(1)

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID != "main-group-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "main-group-calendar")
	}
}

// TestGroup_AgendaPersonalRejected vérifie le scénario "agenda personnel
// refusé" : un groupe ne peut jamais résoudre un calendrier personnel, même
// si le modèle forge un calendar_id personnel dans les arguments de l'outil.
func TestGroup_AgendaPersonalRejected(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := baseOrgConfig()
	withCalendarResources(cfg, httpServer.URL)

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"calendar_id":"alice-personal-calendar","from":"2026-09-01","to":"2026-09-30"}`)), nil
		}
		return scriptedFinalResponse("Voilà."), nil
	}

	sys := newTestSystem(t, cfg, mustAgendaAgent(t, cfg, client))

	sys.sendGroup("alice-ext", "group-chan", "@assistant donne-moi mon agenda perso", true)
	sys.waitSent(1)

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID == "alice-personal-calendar" {
		t.Fatal("calendar_id reçu ne doit jamais être l'agenda personnel depuis un groupe")
	}
	if lastListCalendarID != "main-group-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "main-group-calendar")
	}
}

// TestGroup_MultipleAuthors vérifie le scénario "plusieurs auteurs" : deux
// principaux différents dans le même groupe sont chacun correctement
// attribués (principal_id) dans l'historique persisté.
func TestGroup_MultipleAuthors(t *testing.T) {
	cfg := baseOrgConfig()

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Bien reçu."), nil
		},
	}

	sys := newTestSystem(t, cfg, simpleAgent(client))

	sys.sendGroup("alice-ext", "group-chan", "@assistant message d'alice", true)
	sys.waitSent(1)

	sys.sendGroup("bob-ext", "group-chan", "@assistant message de bob", true)
	sys.waitSent(2)

	convID := model.ConversationID(testProviderName + ":group-chan")
	records := conversationMessages(t, sys.db, convID)

	var aliceCount, bobCount int
	for _, m := range records {
		if m.Role != "user" {
			continue
		}
		switch m.PrincipalID {
		case "alice":
			aliceCount++
			if !strings.Contains(m.Content, "alice") {
				t.Errorf("message attribué à alice a un contenu inattendu: %q", m.Content)
			}
		case "bob":
			bobCount++
			if !strings.Contains(m.Content, "bob") {
				t.Errorf("message attribué à bob a un contenu inattendu: %q", m.Content)
			}
		default:
			t.Errorf("principal_id inattendu: %q", m.PrincipalID)
		}
	}

	if aliceCount != 1 || bobCount != 1 {
		t.Fatalf("attribution des messages incorrecte: alice=%d, bob=%d (attendu 1 chacun)", aliceCount, bobCount)
	}
}

// TestGroup_ConfirmationByAuthorizedMember vérifie le scénario
// "confirmation par un autre membre autorisé" : le principal A propose une
// action de groupe, le principal B (même groupe, permissions suffisantes)
// la confirme avec succès (PLAN.md §10.5 authorizeConfirmer : seule la
// portée personal restreint au créateur).
func TestGroup_ConfirmationByAuthorizedMember(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	seeded, err := store.Remember(context.Background(), memory.NewMemory{
		Content: "Ne pas oublier d'arroser les plantes.",
		Scope:   model.ScopeGroup, ScopeID: "home-group", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("seed group memory: %v", err)
	}

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "forget_memory", `{"id":"`+seeded.ID+`"}`)), nil
		}
		return scriptedFinalResponse("Je propose de supprimer ce souvenir du groupe."), nil
	}

	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	// Alice (principal A) propose la suppression.
	sys.sendGroup("alice-ext", "group-chan", "@assistant oublie les plantes", true)
	sent := sys.waitSent(1)

	if got := mainContent(t, sent[0]); !strings.Contains(got, "confirmer") {
		t.Fatalf("instructions de confirmation attendues, obtenu: %q", got)
	}

	callsBeforeConfirm := client.callCount()

	// Bob (principal B, même groupe) confirme, jamais Alice.
	sys.sendGroup("bob-ext", "group-chan", "confirmer", true)
	sent = sys.waitSent(2)

	if got := client.callCount(); got != callsBeforeConfirm {
		t.Fatalf("la confirmation littérale ne doit jamais invoquer le LLM: avant=%d après=%d", callsBeforeConfirm, got)
	}

	report := mainContent(t, sent[1])
	if !strings.Contains(report, "succès") {
		t.Fatalf("rapport d'exécution attendu avec succès, obtenu: %q", report)
	}

	_, found, err := store.GetByID(context.Background(), "home", model.ScopeGroup, "home-group", seeded.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if found {
		t.Fatal("la mémoire de groupe aurait dû être supprimée après confirmation par bob")
	}
}
