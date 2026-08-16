package e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/audio"
	"github.com/bornholm/automata/internal/authorization"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
)

// TestPrivate_Text vérifie le scénario le plus simple (PLAN.md Phase 21,
// "texte") : un message texte depuis une origine privée connue traverse
// l'ingress, l'identité, le gestionnaire de conversation et l'agent, et une
// réponse est bien envoyée.
func TestPrivate_Text(t *testing.T) {
	cfg := baseOrgConfig()

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Bonjour Alice !"), nil
		},
	}

	sys := newTestSystem(t, cfg, simpleAgent(client))

	sys.sendPrivate("alice-ext", "alice-priv", "bonjour")

	sent := sys.waitSent(1)
	if got := mainContent(t, sent[0]); got != "Bonjour Alice !" {
		t.Errorf("contenu de la réponse: got %q, expected %q", got, "Bonjour Alice !")
	}
}

// TestPrivate_Audio vérifie le scénario "audio" (PLAN.md §3.4, Phase 9) : une
// note vocale est transcrite puis traitée comme du texte, mais seul un
// placeholder neutre est persisté en base, jamais la transcription brute.
func TestPrivate_Audio(t *testing.T) {
	cfg := baseOrgConfig()

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Bien reçu."), nil
		},
	}

	transcriber := &spyTranscriber{reply: "il faut acheter du pain"}
	audioCfg := audio.Config{Enabled: true, MaxSize: 1 << 20, Timeout: 5 * time.Second}

	sys := newTestSystem(t, cfg, simpleAgent(client), withAudio(audioCfg, transcriber))

	sys.sendVoiceNote("alice-ext", "alice-priv", []byte("faux octets audio"))

	sys.waitSent(1)

	transcriber.mu.Lock()
	calls := transcriber.calls
	transcriber.mu.Unlock()
	if calls != 1 {
		t.Fatalf("transcriber.calls = %d, attendu 1", calls)
	}

	convID := model.ConversationID(testProviderName + ":alice-priv")
	records := conversationMessages(t, sys.db, convID)

	var userContent string
	for _, m := range records {
		if m.Role == "user" {
			userContent = m.Content
		}
	}

	const placeholder = "[Message vocal transcrit pour traitement]"
	if userContent != placeholder {
		t.Fatalf("contenu persisté = %q, attendu le placeholder neutre %q (jamais la transcription brute)", userContent, placeholder)
	}
	if strings.Contains(userContent, "pain") {
		t.Fatal("la transcription réelle ne doit jamais atteindre la base par défaut")
	}
}

// TestPrivate_Memory vérifie le scénario "mémoire personnelle" : remember
// puis search_memory dans la même conversation privée retrouve bien
// l'information mémorisée, à travers la pile complète (agent -> outils
// mémoire -> mémoire Amoxtli réelle -> persistance).
func TestPrivate_Memory(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		switch turn {
		case 0:
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "remember", `{"content":"Le café préféré d'Alice est un espresso."}`)), nil
		case 1:
			return scriptedFinalResponse("C'est noté."), nil
		case 2:
			return scriptedToolCallResponse(llm.NewToolCall("call-2", "search_memory", `{"query":"café"}`)), nil
		default:
			return scriptedFinalResponse("D'après mes notes : " + lastToolResultText(opts)), nil
		}
	}

	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	sys.sendPrivate("alice-ext", "alice-priv", "Souviens-toi que j'aime l'espresso.")
	sys.waitSent(1)

	sys.sendPrivate("alice-ext", "alice-priv", "Quel est mon café préféré ?")
	sent := sys.waitSent(2)

	got := mainContent(t, sent[1])
	if !strings.Contains(got, "espresso") {
		t.Fatalf("la mémoire retrouvée devrait contenir 'espresso', obtenu: %q", got)
	}
}

// TestPrivate_MemoryOrgReadFromPrivate vérifie le scénario "lecture org" :
// un principal disposant de memory.org.read recherche et obtient des
// résultats de portée org depuis une conversation privée.
func TestPrivate_MemoryOrgReadFromPrivate(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	if _, err := store.Remember(context.Background(), memory.NewMemory{
		Content: "Réunion familiale samedi à 18h.",
		Scope:   model.ScopeOrg, ScopeID: "home", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice",
	}); err != nil {
		t.Fatalf("seed org memory: %v", err)
	}

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "search_memory", `{"query":"réunion"}`)), nil
		}
		return scriptedFinalResponse("Résultat : " + lastToolResultText(opts)), nil
	}

	// leo n'a que memory.org.read (voir baseOrgConfig) : aucun accès
	// personnel ou groupe, uniquement org.
	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	sys.sendPrivate("leo-ext", "leo-priv", "Qu'y a-t-il de prévu pour la famille ?")
	sent := sys.waitSent(1)

	got := mainContent(t, sent[0])
	if !strings.Contains(got, "Réunion familiale") {
		t.Fatalf("résultat org attendu dans la réponse, obtenu: %q", got)
	}
}

// TestPrivate_MemoryOrgWriteRefused vérifie le scénario "écriture org
// refusée" : même avec la permission memory.org.delete, une tentative de
// suppression d'une mémoire org depuis une conversation privée est refusée
// par la règle invariante d'internal/authorization (jamais d'écriture/
// suppression org depuis un privé), pas seulement empêchée par l'absence de
// paramètre exposé au modèle.
func TestPrivate_MemoryOrgWriteRefused(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	seeded, err := store.Remember(context.Background(), memory.NewMemory{
		Content: "Règle du foyer : ne jamais oublier de fermer le portail.",
		Scope:   model.ScopeOrg, ScopeID: "home", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("seed org memory: %v", err)
	}

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "forget_memory", `{"id":"`+seeded.ID+`"}`)), nil
		}
		return scriptedFinalResponse("D'accord."), nil
	}

	// alice a memory.org.delete (rôle "adult") : la permission est
	// accordée, mais la règle invariante de portée doit tout de même
	// refuser toute écriture/suppression org depuis un privé.
	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	sys.sendPrivate("alice-ext", "alice-priv", "Oublie la règle du portail.")
	sys.waitSent(1)

	convID := model.ConversationID(testProviderName + ":alice-priv")
	if count := countActionPlansByConversation(t, sys.db, convID); count != 0 {
		t.Fatalf("aucune proposition de suppression org n'aurait dû être créée depuis un privé, got %d plan(s)", count)
	}

	results, err := store.Search(context.Background(), memory.Query{Text: "portail", OrgID: "home", Scope: model.ScopeOrg, ScopeID: "home", MaxResults: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("la mémoire org devrait toujours exister après la tentative refusée, got %d résultat(s)", len(results))
	}
}

// TestPrivate_Agenda vérifie le scénario "agenda personnel" : la lecture du
// calendrier personnel depuis une conversation privée est résolue vers la
// ressource "calendar" du canal privé, jamais un identifiant forgé.
func TestPrivate_Agenda(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := baseOrgConfig()
	withCalendarResources(cfg, httpServer.URL)

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "list_events", `{"calendar_id":"","from":"2026-09-01","to":"2026-09-30"}`)), nil
		}
		return scriptedFinalResponse("Rien de prévu ce mois-ci."), nil
	}

	sys := newTestSystem(t, cfg, mustAgendaAgent(t, cfg, client))

	sys.sendPrivate("alice-ext", "alice-priv", "Qu'est-ce que j'ai de prévu ?")
	sent := sys.waitSent(1)

	if got := mainContent(t, sent[0]); !strings.Contains(got, "prévu") {
		t.Fatalf("réponse inattendue: %q", got)
	}

	_, _, lastListCalendarID, _ := spy.snapshot()
	if lastListCalendarID != "alice-personal-calendar" {
		t.Fatalf("calendar_id reçu = %q, attendu %q", lastListCalendarID, "alice-personal-calendar")
	}
}

// TestPrivate_Confirmation vérifie le scénario "confirmation" : une action
// proposée (suppression de mémoire personnelle) n'est exécutée qu'après que
// l'utilisateur a répondu "confirmer" en toutes lettres, jamais décidé par
// le modèle (interception AVANT tout appel LLM dans
// internal/conversation.Handler).
func TestPrivate_Confirmation(t *testing.T) {
	cfg := baseOrgConfig()

	store := newMemoryStore(t)
	authorizer := authorization.NewAuthorizer(cfg)

	seeded, err := store.Remember(context.Background(), memory.NewMemory{
		Content: "Rendez-vous chez le dentiste vendredi.",
		Scope:   model.ScopePersonal, ScopeID: "alice", OrgID: "home",
		OwnerPrincipalID: "alice", CreatedBy: "alice",
	})
	if err != nil {
		t.Fatalf("seed personal memory: %v", err)
	}

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "forget_memory", `{"id":"`+seeded.ID+`"}`)), nil
		}
		return scriptedFinalResponse("Je te propose de supprimer ce souvenir."), nil
	}

	sys := newTestSystem(t, cfg, newMemoryOrchestrator(client, store, authorizer), withMemoryStore(store))

	sys.sendPrivate("alice-ext", "alice-priv", "Oublie mon rendez-vous chez le dentiste.")
	sent := sys.waitSent(1)

	proposal := mainContent(t, sent[0])
	if !strings.Contains(proposal, "confirmer") {
		t.Fatalf("instructions de confirmation attendues, obtenu: %q", proposal)
	}

	callsAfterProposal := client.callCount()

	sys.sendPrivate("alice-ext", "alice-priv", "confirmer")
	sent = sys.waitSent(2)

	if got := client.callCount(); got != callsAfterProposal {
		t.Fatalf("la confirmation littérale ne doit jamais invoquer le LLM: appels avant=%d après=%d", callsAfterProposal, got)
	}

	report := mainContent(t, sent[1])
	if !strings.Contains(report, "succès") {
		t.Fatalf("rapport d'exécution attendu avec succès, obtenu: %q", report)
	}

	_, found, err := store.GetByID(context.Background(), "home", model.ScopePersonal, "alice", seeded.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if found {
		t.Fatal("la mémoire aurait dû être supprimée après confirmation")
	}
}

// TestPrivate_ImageSeenByAgentAndMediaReplied couvre la chaîne multimodale
// complète, à travers le système assemblé : une image envoyée par
// l'utilisateur traverse l'ingress, est extraite, parvient au modèle sur le
// message "user", et un média produit en réponse repart réellement sur le
// canal.
func TestPrivate_ImageSeenByAgentAndMediaReplied(t *testing.T) {
	cfg := baseOrgConfig()

	sent := []byte("octets de la photo")
	produced := media.Media{
		Kind:     media.KindImage,
		MimeType: "image/png",
		Filename: "annotee.png",
		Data:     []byte("octets annotés"),
	}

	var seen []llm.Attachment

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		for _, m := range opts.Messages {
			if m.Role() == llm.RoleUser {
				seen = append(seen, m.Attachments()...)
			}
		}
		return scriptedFinalResponse("Je vois un cèpe."), nil
	}

	sys := newTestSystem(t, cfg,
		&mediaProducingAgent{client: client, produced: []media.Media{produced}},
		withAttachments(media.Config{
			Enabled:       true,
			MaxSize:       4096,
			MaxCount:      3,
			AcceptedTypes: []string{"image/png"},
			MaxHistory:    4,
			MaxReply:      2,
		}),
	)

	sys.sendImage("alice-ext", "alice-priv", "C'est quoi ce champignon ?", "photo.png", sent)
	delivered := sys.waitSent(1)

	// L'image a bien atteint le modèle, portée par un message "user".
	if len(seen) != 1 {
		t.Fatalf("pièces jointes vues par le modèle: got %d, expected 1", len(seen))
	}
	decoded, err := base64.StdEncoding.DecodeString(seen[0].Data())
	if err != nil {
		t.Fatalf("données non décodables: %v", err)
	}
	if !bytes.Equal(decoded, sent) {
		t.Errorf("l'image reçue par le modèle est altérée: %q", decoded)
	}

	// Le média produit est réellement reparti sur le canal.
	attachments := courier.Attachments(delivered[0])
	if len(attachments) != 1 {
		t.Fatalf("pièces jointes envoyées à l'utilisateur: got %d, expected 1", len(attachments))
	}
	if got := attachments[0].Filename(); got != "annotee.png" {
		t.Errorf("filename = %q, attendu annotee.png", got)
	}
}

// mediaProducingAgent est un OrchestratorAgent réel auquel on ajoute des
// médias en réponse, pour simuler un outil qui en produit.
type mediaProducingAgent struct {
	client   llm.ChatCompletionClient
	produced []media.Media
}

func (a *mediaProducingAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	inner := agent.NewOrchestratorAgent(a.client, "system", "main", "Maison", map[string]delegation.Specialist{}, 5)

	result, err := inner.Execute(ctx, req)
	if err != nil {
		return agent.Result{}, err
	}

	result.Attachments = append(result.Attachments, a.produced...)

	return result, nil
}

// TestPrivate_AgendaWriteConfirmedExecutesWithResolvedResource couvre le
// chemin complet d'une ÉCRITURE agenda : le spécialiste propose, rien n'est
// exécuté, l'utilisateur répond "confirmer" en toutes lettres, et c'est
// seulement alors que l'appel MCP réel a lieu.
//
// Le point vérifié en propre est la résolution tardive de la ressource
// (PLAN.md §10.5 point 6) : l'identifiant de calendrier n'est jamais figé
// dans l'action persistée, il est réinjecté depuis la portée du plan au
// moment d'exécuter. C'est ce qui garantit qu'une action confirmée écrit
// dans le calendrier courant de sa portée, et pas dans celui qu'un modèle
// aurait pu suggérer.
func TestPrivate_AgendaWriteConfirmedExecutesWithResolvedResource(t *testing.T) {
	httpServer, spy := newFakeCalendarServer(t)
	cfg := baseOrgConfig()
	withCalendarResources(cfg, httpServer.URL)

	client := &fakeCompletionClient{}
	client.responseFunc = func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
		if turn == 0 {
			// Le modèle tente au passage d'imposer son propre calendrier.
			return scriptedToolCallResponse(llm.NewToolCall("call-1", "create_event",
				`{"calendar_id":"forged-by-model","title":"Dentiste","start":"2026-09-12T14:00:00+02:00","end":"2026-09-12T15:00:00+02:00"}`)), nil
		}
		return scriptedFinalResponse("Je te propose ce rendez-vous."), nil
	}

	agendaAgent, mcpManager := newAgendaAgent(t, cfg, client)
	sys := newTestSystem(t, cfg, agendaAgent, withMCPManager(mcpManager))

	sys.sendPrivate("alice-ext", "alice-priv", "Ajoute un rendez-vous chez le dentiste vendredi.")
	sent := sys.waitSent(1)

	proposal := mainContent(t, sent[0])
	if !strings.Contains(proposal, "confirmer") {
		t.Fatalf("instructions de confirmation attendues, obtenu: %q", proposal)
	}

	if _, createCalls, _, _ := spy.snapshot(); createCalls != 0 {
		t.Fatalf("aucune création réelle ne doit avoir lieu avant confirmation, appelée %d fois", createCalls)
	}

	sys.sendPrivate("alice-ext", "alice-priv", "confirmer")
	sent = sys.waitSent(2)

	if report := mainContent(t, sent[1]); !strings.Contains(report, "succès") {
		t.Fatalf("rapport d'exécution attendu avec succès, obtenu: %q", report)
	}

	_, createCalls, _, lastCreateCalendarID := spy.snapshot()
	if createCalls != 1 {
		t.Fatalf("create_event aurait dû être exécuté exactement une fois après confirmation, appelé %d fois", createCalls)
	}
	if lastCreateCalendarID == "forged-by-model" {
		t.Fatal("le calendar_id forgé par le modèle n'aurait jamais dû atteindre le serveur mcp")
	}
	if lastCreateCalendarID != "alice-personal-calendar" {
		t.Fatalf("calendar_id reçu à l'exécution = %q, attendu %q", lastCreateCalendarID, "alice-personal-calendar")
	}
}
