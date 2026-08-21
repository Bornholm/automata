package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

// fakeCompletionClient est un client LLM de test implémentant
// llm.ChatCompletionClient (non-streaming) sans réseau, piloté tour par
// tour par responseFunc : cela permet de scripter précisément les
// tool-calls demandés par le modèle simulé, condition nécessaire pour
// piloter la boucle de tool-calling d'OrchestratorAgent (PLAN.md Phase 8).
type fakeCompletionClient struct {
	mu           sync.Mutex
	responseFunc func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error)
	optsHistory  []*llm.ChatCompletionOptions
	turn         int
}

func (f *fakeCompletionClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	opts := llm.NewChatCompletionOptions(funcs...)

	f.mu.Lock()
	f.optsHistory = append(f.optsHistory, opts)
	turn := f.turn
	f.turn++
	f.mu.Unlock()

	return f.responseFunc(turn, opts)
}

func (f *fakeCompletionClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.optsHistory)
}

var _ llm.ChatCompletionClient = &fakeCompletionClient{}

// fakeSpecialist est un delegation.Specialist de test qui enregistre chaque
// requête reçue et délègue le comportement à executeFunc.
type fakeSpecialist struct {
	mu          sync.Mutex
	executeFunc func(ctx context.Context, req delegation.Request) (delegation.Result, error)
	calls       []delegation.Request
}

func (s *fakeSpecialist) Execute(ctx context.Context, req delegation.Request) (delegation.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()

	return s.executeFunc(ctx, req)
}

func (s *fakeSpecialist) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

var _ delegation.Specialist = &fakeSpecialist{}

// scriptedFinalResponse construit un tour de réponse finale (aucun
// tool-call) du modèle simulé.
func scriptedFinalResponse(text string) llm.ChatCompletionResponse {
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, text), llm.NewChatCompletionUsage(1, 1, 2))
}

// scriptedToolCallResponse construit un tour de réponse du modèle simulé
// demandant les tool-calls fournis.
func scriptedToolCallResponse(toolCalls ...llm.ToolCall) llm.ChatCompletionResponse {
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, ""), llm.NewChatCompletionUsage(1, 1, 2), toolCalls...)
}

func TestOrchestratorAgent_DelegatesToAgenda(t *testing.T) {
	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			if req.Goal != "Vérifier les rendez-vous de demain" {
				t.Errorf("goal transmis inattendu: %q", req.Goal)
			}
			return delegation.Result{Summary: "Réunion à 10h, dentiste à 15h."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_agenda", `{"goal":"Vérifier les rendez-vous de demain","relevant_input":"L'utilisateur demande son agenda de demain."}`)), nil
			}
			return scriptedFinalResponse("Demain : réunion à 10h, dentiste à 15h."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Qu'ai-je demain ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if agenda.callCount() != 1 {
		t.Fatalf("le spécialiste agenda aurait dû être appelé une fois, appelé %d fois", agenda.callCount())
	}

	if !strings.Contains(result.Reply, "dentiste") {
		t.Fatalf("la réponse finale devrait refléter le résultat agrégé, obtenu: %q", result.Reply)
	}
}

func TestOrchestratorAgent_DelegatesToResearch(t *testing.T) {
	research := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			if req.Goal != "Trouver la capitale du Portugal" {
				t.Errorf("goal transmis inattendu: %q", req.Goal)
			}
			return delegation.Result{Summary: "La capitale du Portugal est Lisbonne."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_research", `{"goal":"Trouver la capitale du Portugal"}`)), nil
			}
			return scriptedFinalResponse("La capitale du Portugal est Lisbonne."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"research": research}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Quelle est la capitale du Portugal ?"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if research.callCount() != 1 {
		t.Fatalf("le spécialiste research aurait dû être appelé une fois, appelé %d fois", research.callCount())
	}

	if !strings.Contains(result.Reply, "Lisbonne") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

func TestOrchestratorAgent_DelegatesToTodo(t *testing.T) {
	todo := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			if req.Goal != "Ajouter une tâche : acheter du pain" {
				t.Errorf("goal transmis inattendu: %q", req.Goal)
			}
			return delegation.Result{Summary: "Tâche ajoutée : acheter du pain."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_todo", `{"goal":"Ajouter une tâche : acheter du pain"}`)), nil
			}
			return scriptedFinalResponse("C'est noté : acheter du pain."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"todo": todo}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Ajoute acheter du pain à ma liste"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if todo.callCount() != 1 {
		t.Fatalf("le spécialiste todo aurait dû être appelé une fois, appelé %d fois", todo.callCount())
	}

	if !strings.Contains(result.Reply, "pain") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

func TestOrchestratorAgent_NoDelegationWhenUnnecessary(t *testing.T) {
	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			t.Fatal("le spécialiste agenda ne devrait jamais être appelé")
			return delegation.Result{}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Bonjour ! Comment puis-je vous aider ?"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Salut"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if agenda.callCount() != 0 {
		t.Fatalf("aucun appel au spécialiste attendu, obtenu %d", agenda.callCount())
	}

	if result.Reply != "Bonjour ! Comment puis-je vous aider ?" {
		t.Fatalf("réponse inattendue: %q", result.Reply)
	}

	if client.callCount() != 1 {
		t.Fatalf("un seul appel de complétion attendu, obtenu %d", client.callCount())
	}
}

func TestOrchestratorAgent_MultipleDelegationsSameTurn(t *testing.T) {
	var order []string
	var mu sync.Mutex

	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			mu.Lock()
			order = append(order, "agenda")
			mu.Unlock()
			return delegation.Result{Summary: "Rien de prévu."}, nil
		},
	}
	todo := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			mu.Lock()
			order = append(order, "todo")
			mu.Unlock()
			return delegation.Result{Summary: "2 tâches en attente."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(
					llm.NewToolCall("call-1", "delegate_to_agenda", `{"goal":"Vérifier l'agenda"}`),
					llm.NewToolCall("call-2", "delegate_to_todo", `{"goal":"Vérifier les tâches"}`),
				), nil
			}

			// Au second tour, les deux résultats d'outils doivent avoir été
			// transmis au modèle avant la réponse finale.
			var sawAgendaResult, sawTodoResult bool
			for _, m := range opts.Messages {
				if m.Role() != llm.RoleTool {
					continue
				}
				if strings.Contains(m.Content(), "Rien de prévu") {
					sawAgendaResult = true
				}
				if strings.Contains(m.Content(), "tâches en attente") {
					sawTodoResult = true
				}
			}
			if !sawAgendaResult || !sawTodoResult {
				t.Errorf("les résultats des deux spécialistes devraient être présents avant la réponse finale (agenda=%v, todo=%v)", sawAgendaResult, sawTodoResult)
			}

			return scriptedFinalResponse("Rien de prévu, et 2 tâches en attente."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda, "todo": todo}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Fais le point"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()

	if len(gotOrder) != 2 || gotOrder[0] != "agenda" || gotOrder[1] != "todo" {
		t.Fatalf("ordre d'exécution séquentiel attendu [agenda todo], obtenu %v", gotOrder)
	}

	if !strings.Contains(result.Reply, "2 tâches") {
		t.Fatalf("réponse finale inattendue: %q", result.Reply)
	}
}

// proposingSpecialist retourne un spécialiste factice qui propose count
// actions lors de chaque délégation.
func proposingSpecialist(count int) *fakeSpecialist {
	return &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			proposals := make([]delegation.ProposedAction, 0, count)
			for i := range count {
				proposals = append(proposals, delegation.ProposedAction{
					Summary:            fmt.Sprintf("Créer la tâche n°%d", i+1),
					AgentID:            "todo",
					MCPServer:          "todo",
					ToolName:           "create_task",
					Arguments:          map[string]any{"title": fmt.Sprintf("tâche %d", i+1)},
					RequiredPermission: "todo.personal.write",
					Scope:              model.ScopePersonal,
					ScopeID:            model.ScopeID("alice"),
				})
			}

			return delegation.Result{Summary: "Tâches préparées.", ProposedActions: proposals}, nil
		},
	}
}

// scriptDelegateThenReply pilote le modèle simulé pour déléguer une fois à
// todo, puis répondre.
func scriptDelegateThenReply(reply string) *fakeCompletionClient {
	return &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_todo", `{"goal":"Créer les tâches"}`)), nil
			}
			return scriptedFinalResponse(reply), nil
		},
	}
}

// TestOrchestratorAgent_MaxActionsPerTurnRespected vérifie qu'un lot
// d'actions tenant sous le plafond est transmis intact (PLAN.md §9.4).
func TestOrchestratorAgent_MaxActionsPerTurnRespected(t *testing.T) {
	todo := proposingSpecialist(3)
	client := scriptDelegateThenReply("J'ai préparé 3 tâches.")

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"todo": todo}, 5).
		WithMaxActionsPerTurn(5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Crée mes tâches"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if got := len(result.ProposedActions); got != 3 {
		t.Fatalf("actions proposées: got %d, expected 3", got)
	}

	if strings.Contains(result.Reply, "limite") {
		t.Errorf("aucun avertissement de plafond attendu sous la limite, obtenu: %q", result.Reply)
	}
}

// TestOrchestratorAgent_MaxActionsPerTurnRejectsWholeBatch vérifie qu'un
// dépassement rejette le lot entier plutôt que d'en conserver un préfixe
// arbitraire : l'utilisateur ne doit jamais confirmer un sous-ensemble
// silencieux de ce que l'agent a annoncé (PLAN.md §9.4, §10.3).
func TestOrchestratorAgent_MaxActionsPerTurnRejectsWholeBatch(t *testing.T) {
	todo := proposingSpecialist(4)
	client := scriptDelegateThenReply("J'ai préparé 4 tâches.")

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"todo": todo}, 5).
		WithMaxActionsPerTurn(2)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Crée mes tâches"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if got := len(result.ProposedActions); got != 0 {
		t.Fatalf("actions proposées: got %d, expected 0 (lot entier rejeté)", got)
	}

	// La réponse doit rester exploitable et expliquer le refus.
	if !strings.Contains(result.Reply, "J'ai préparé 4 tâches.") {
		t.Errorf("le texte de réponse du modèle devrait être conservé, obtenu: %q", result.Reply)
	}
	if !strings.Contains(result.Reply, "4 actions") || !strings.Contains(result.Reply, "limite de 2") {
		t.Errorf("la réponse devrait expliquer le dépassement, obtenu: %q", result.Reply)
	}
}

// TestOrchestratorAgent_MaxActionsPerTurnUnsetIsUnbounded documente le
// comportement par défaut : sans plafond configuré, aucune action n'est
// écartée.
func TestOrchestratorAgent_MaxActionsPerTurnUnsetIsUnbounded(t *testing.T) {
	todo := proposingSpecialist(12)
	client := scriptDelegateThenReply("J'ai préparé 12 tâches.")

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"todo": todo}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Crée mes tâches"})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if got := len(result.ProposedActions); got != 12 {
		t.Fatalf("actions proposées: got %d, expected 12", got)
	}
}

// scriptTwoDelegationsThenReply pilote le modèle simulé pour déléguer deux
// fois de suite (un tour chacun), puis répondre. collect reçoit, au dernier
// tour, le contenu de tous les messages de rôle "tool" vus par le modèle.
func scriptTwoDelegationsThenReply(collect *[]string) *fakeCompletionClient {
	return &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			switch turn {
			case 0:
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_research", `{"goal":"Première recherche"}`)), nil
			case 1:
				return scriptedToolCallResponse(llm.NewToolCall("call-2", "delegate_to_research", `{"goal":"Seconde recherche"}`)), nil
			default:
				for _, m := range opts.Messages {
					if m.Role() == llm.RoleTool {
						*collect = append(*collect, m.Content())
					}
				}

				return scriptedFinalResponse("Synthèse finale."), nil
			}
		},
	}
}

// verboseSpecialist retourne un spécialiste dont le résumé fait size octets,
// pour éprouver le budget de contexte.
func verboseSpecialist(size int) *fakeSpecialist {
	return &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: strings.Repeat("a", size)}, nil
		},
	}
}

// TestOrchestratorAgent_MaxToolContextBytesBoundsCumulativeResults vérifie le
// budget CUMULÉ des résultats d'outils (PLAN.md §9.4) : plusieurs appels
// tenant chacun sous le plafond unitaire ne doivent pas, ensemble, dépasser
// le budget de contexte déclaré. La réduction est signalée au modèle plutôt
// que silencieuse, sans quoi il lirait un résultat vide comme une absence de
// données.
func TestOrchestratorAgent_MaxToolContextBytesBoundsCumulativeResults(t *testing.T) {
	const budget = 150

	var toolContents []string

	research := verboseSpecialist(200)
	client := scriptTwoDelegationsThenReply(&toolContents)

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"research": research}, 5).
		WithMaxToolContextBytes(budget)

	if _, err := a.Execute(context.Background(), agent.Request{Input: "Cherche"}); err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if len(toolContents) != 2 {
		t.Fatalf("résultats d'outils vus par le modèle: got %d, expected 2", len(toolContents))
	}

	// Premier résultat : tronqué au budget, avec mention explicite.
	if !strings.Contains(toolContents[0], "tronqué") {
		t.Errorf("le premier résultat devrait signaler la troncature, obtenu: %q", toolContents[0])
	}

	// Second résultat : budget déjà épuisé, contenu non transmis mais annoncé.
	if !strings.Contains(toolContents[1], "budget de contexte d'outils épuisé") {
		t.Errorf("le second résultat devrait signaler l'épuisement du budget, obtenu: %q", toolContents[1])
	}

	// Le texte utile réinjecté reste borné par le budget : seules les notes
	// de signalement, courtes et de taille connue, s'y ajoutent. Le contenu
	// utile est le préfixe de "a" produit par le spécialiste, mesuré ici sans
	// les libellés des notes (qui contiennent eux aussi la lettre "a").
	var payload int
	for _, c := range toolContents {
		payload += len(c) - len(strings.TrimLeft(c, "a"))
	}
	if payload > budget {
		t.Errorf("contenu utile cumulé = %d octets, au-delà du budget de %d", payload, budget)
	}
}

// TestOrchestratorAgent_MaxToolContextBytesUnsetIsUnbounded documente le
// comportement par défaut : sans budget configuré, les résultats d'outils
// sont transmis intégralement.
func TestOrchestratorAgent_MaxToolContextBytesUnsetIsUnbounded(t *testing.T) {
	var toolContents []string

	research := verboseSpecialist(200)
	client := scriptTwoDelegationsThenReply(&toolContents)

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"research": research}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{Input: "Cherche"}); err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if len(toolContents) != 2 {
		t.Fatalf("résultats d'outils vus par le modèle: got %d, expected 2", len(toolContents))
	}

	for i, c := range toolContents {
		if got := len(c) - len(strings.TrimLeft(c, "a")); got != 200 {
			t.Errorf("résultat %d: contenu attendu intégral (200 octets), obtenu %d", i+1, got)
		}
	}
}

func TestOrchestratorAgent_SpecialistError(t *testing.T) {
	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{}, errors.New("service calendrier indisponible")
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_agenda", `{"goal":"Vérifier l'agenda"}`)), nil
			}

			var sawErrorResult bool
			for _, m := range opts.Messages {
				if m.Role() == llm.RoleTool && strings.Contains(m.Content(), "échoué") {
					sawErrorResult = true
				}
			}
			if !sawErrorResult {
				t.Error("le résultat d'outil transmis au modèle devrait décrire l'échec du spécialiste")
			}

			return scriptedFinalResponse("Je n'ai pas pu consulter votre agenda pour le moment."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, 5)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Qu'ai-je aujourd'hui ?"})
	if err != nil {
		t.Fatalf("Execute ne devrait pas échouer sur une erreur de spécialiste: %v", err)
	}

	if result.Reply == "" {
		t.Fatal("une réponse finale exploitable était attendue malgré l'échec du spécialiste")
	}
}

func TestOrchestratorAgent_MaxDelegationsReached(t *testing.T) {
	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "ok"}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			// Le modèle simulé redemande indéfiniment le même outil : sans
			// plafond, la boucle ne terminerait jamais.
			return scriptedToolCallResponse(llm.NewToolCall("call-x", "delegate_to_agenda", `{"goal":"boucle"}`)), nil
		},
	}

	const maxSequentialToolCalls = 3

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, maxSequentialToolCalls)

	_, err := a.Execute(context.Background(), agent.Request{Input: "Boucle"})
	if !errors.Is(err, agent.ErrMaxDelegationsReached) {
		t.Fatalf("erreur ErrMaxDelegationsReached attendue, obtenu: %v", err)
	}

	// 3 tours de boucle + 3 tentatives de conclusion (le modèle simulé
	// redemande un outil à chacune) : l'erreur ne tombe qu'après épuisement
	// des replis, jamais avant d'avoir tenté de sauver le tour.
	const conclusionAttempts = 3
	if client.callCount() != maxSequentialToolCalls+conclusionAttempts {
		t.Fatalf("nombre d'appels de complétion attendu %d, obtenu %d", maxSequentialToolCalls+conclusionAttempts, client.callCount())
	}
}

func TestOrchestratorAgent_MainHistoryNotForwardedToSpecialist(t *testing.T) {
	const secretMarker = "SECRET_MARKER_123"

	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			if strings.Contains(req.Goal, secretMarker) || strings.Contains(req.RelevantInput, secretMarker) {
				t.Errorf("le marqueur secret de l'historique principal ne devrait jamais atteindre le spécialiste: goal=%q relevant_input=%q", req.Goal, req.RelevantInput)
			}
			return delegation.Result{Summary: "Rien de prévu."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_agenda", `{"goal":"Vérifier l'agenda de demain"}`)), nil
			}
			return scriptedFinalResponse("Rien de prévu demain."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, 5)

	_, err := a.Execute(context.Background(), agent.Request{
		History: []agent.Message{
			{Role: "user", Content: "Mon code secret est " + secretMarker},
			{Role: "assistant", Content: "Noté, je ne le répéterai pas."},
		},
		Input: "Qu'ai-je prévu demain ?",
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if agenda.callCount() != 1 {
		t.Fatalf("le spécialiste agenda aurait dû être appelé une fois, appelé %d fois", agenda.callCount())
	}
}

// La description du spécialiste (agents.<nom>.description) doit atteindre le
// modèle : c'est sur elle qu'il décide de déléguer. Sans elle, un petit
// modèle répond « je ne sais pas faire » devant un outil dont il ne connaît
// que le nom.
func TestOrchestrator_SpecialistDescriptionReachesTool(t *testing.T) {
	var seen []llm.Tool

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			seen = opts.Tools
			return scriptedFinalResponse("ok"), nil
		},
	}

	specialists := map[string]delegation.Specialist{
		"research": &fakeSpecialist{},
		"todo":     &fakeSpecialist{},
	}

	orchestrator := agent.NewOrchestratorAgent(client, "system", "main", specialists, 3).
		WithSpecialistDescriptions(map[string]string{
			"research": "cherche des informations à jour sur Internet",
		})

	if _, err := orchestrator.Execute(context.Background(), agent.Request{Input: "bonjour"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	descriptions := map[string]string{}
	for _, tool := range seen {
		descriptions[tool.Name()] = tool.Description()
	}

	research, ok := descriptions["delegate_to_research"]
	if !ok {
		t.Fatalf("outils exposés = %v, attendu delegate_to_research", descriptions)
	}
	if !strings.Contains(research, "cherche des informations à jour sur Internet") {
		t.Errorf("description de delegate_to_research = %q, la description du spécialiste manque", research)
	}

	// Un spécialiste sans description garde la formulation générique plutôt
	// que d'hériter de celle d'un autre.
	todo, ok := descriptions["delegate_to_todo"]
	if !ok {
		t.Fatalf("outils exposés = %v, attendu delegate_to_todo", descriptions)
	}
	if strings.Contains(todo, "Internet") {
		t.Errorf("description de delegate_to_todo = %q, contaminée par celle d'un autre spécialiste", todo)
	}
}

// Le prompt système se choisit par requête, selon l'organisation du canal :
// le même agent sert la famille et l'équipe professionnelle, et chacune doit
// voir sa propre personnalité — jamais celle de l'autre.
func TestOrchestrator_UsesOrganizationSystemPrompt(t *testing.T) {
	var systemSeen string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, m := range opts.Messages {
				if m.Role() == llm.RoleSystem {
					systemSeen = m.Content()
				}
			}
			return scriptedFinalResponse("ok"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "family prompt", "main", map[string]delegation.Specialist{}, 5).
		WithOrgSystemPrompts(map[string]string{"work": "team prompt"})

	for _, test := range []struct {
		orgID    model.OrgID
		expected string
	}{
		{orgID: "work", expected: "team prompt"},
		{orgID: "home", expected: "family prompt"},
	} {
		_, err := a.Execute(context.Background(), agent.Request{
			Identity: model.ExecutionIdentity{PrincipalID: "alice", OrgID: test.orgID},
			Input:    "salut",
		})
		if err != nil {
			t.Fatalf("Execute(%s): %v", test.orgID, err)
		}

		if !strings.HasPrefix(systemSeen, test.expected) {
			t.Errorf("org %s : le message system commence par %q, attendu %q", test.orgID, systemSeen[:min(40, len(systemSeen))], test.expected)
		}
	}
}

// fakeReportingSpecialist simule un spécialiste sachant rapporter ses
// capacités du moment, disponible ou non.
type fakeReportingSpecialist struct {
	report delegation.CapabilityReport
}

func (f *fakeReportingSpecialist) Execute(ctx context.Context, req delegation.Request) (delegation.Result, error) {
	return delegation.Result{Summary: "ok"}, nil
}

func (f *fakeReportingSpecialist) ReportCapabilities(ctx context.Context, identity model.ExecutionIdentity) delegation.CapabilityReport {
	return f.report
}

// describe_capabilities restitue l'état RÉEL du moment : les outils du tour
// et le statut vivant de chaque spécialiste — dont l'indisponibilité, qui
// est précisément ce que l'introspection doit révéler.
func TestOrchestrator_DescribeCapabilitiesReportsLiveStatus(t *testing.T) {
	var toolResultSeen string

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "describe_capabilities", `{}`)), nil
			}
			for _, m := range opts.Messages {
				if m.Role() == llm.RoleTool {
					toolResultSeen = m.Content()
				}
			}
			return scriptedFinalResponse("Voilà ce que je sais faire."), nil
		},
	}

	specialists := map[string]delegation.Specialist{
		"research": &fakeReportingSpecialist{report: delegation.CapabilityReport{Available: true, Tools: []string{"web_search", "web_url_read"}}},
		"imagine":  &fakeReportingSpecialist{report: delegation.CapabilityReport{Available: false, Detail: "one of its services is currently unreachable"}},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", specialists, 5).
		WithSpecialistDescriptions(map[string]string{"research": "searches the web", "imagine": "generates images"})

	_, err := a.Execute(context.Background(), agent.Request{
		Identity: model.ExecutionIdentity{PrincipalID: "alice", OrgID: "home"},
		Input:    "que sais-tu faire ?",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, expected := range []string{
		"delegate_to_research",
		"web_search, web_url_read",
		"UNAVAILABLE right now",
		"one of its services is currently unreachable",
	} {
		if !strings.Contains(toolResultSeen, expected) {
			t.Errorf("le rapport ne contient pas %q\nrapport:\n%s", expected, toolResultSeen)
		}
	}

	if strings.Contains(toolResultSeen, "- describe_capabilities") {
		t.Error("le rapport ne doit pas décrire l'outil d'introspection lui-même")
	}
}

// Quand le plafond tombe, la matière déjà récoltée ne doit pas être jetée :
// une consigne de conclusion est injectée et le modèle répond avec ce qu'il
// a. Le tour réussit — l'erreur de plafond est le dernier recours, pas la
// première réponse.
func TestOrchestratorAgent_CapForcesConclusionInsteadOfFailing(t *testing.T) {
	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "réunion à 10h"}, nil
		},
	}

	const maxSequentialToolCalls = 2

	var conclusionMessages []llm.Message
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn < maxSequentialToolCalls {
				return scriptedToolCallResponse(llm.NewToolCall(fmt.Sprintf("call-%d", turn), "delegate_to_agenda", `{"goal":"encore"}`)), nil
			}
			conclusionMessages = opts.Messages
			return scriptedFinalResponse("D'après ce que j'ai trouvé : réunion à 10h."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, maxSequentialToolCalls)

	res, err := a.Execute(context.Background(), agent.Request{Input: "Mon planning ?"})
	if err != nil {
		t.Fatalf("Execute aurait dû réussir par la conclusion forcée: %v", err)
	}
	if res.Reply != "D'après ce que j'ai trouvé : réunion à 10h." {
		t.Errorf("reply = %q", res.Reply)
	}

	// La consigne de conclusion doit être le dernier message utilisateur
	// envoyé au modèle, et l'historique doit conserver les résultats
	// d'outils déjà obtenus.
	var sawInstruction, sawToolResult bool
	for _, m := range conclusionMessages {
		if m.Role() == llm.RoleUser && strings.Contains(m.Content(), "Do not call any further tool") {
			sawInstruction = true
		}
		if m.Role() == llm.RoleTool {
			sawToolResult = true
		}
	}
	if !sawInstruction {
		t.Error("la consigne de conclusion n'a pas été injectée dans l'historique")
	}
	if !sawToolResult {
		t.Error("les résultats d'outils déjà obtenus doivent rester dans l'historique de conclusion")
	}
}

// Si le modèle ignore la consigne textuelle et redemande un outil, le repli
// suivant interdit les outils au niveau du protocole (ToolChoiceNone) — et
// c'est cette variante qui sauve le tour.
func TestOrchestratorAgent_ConclusionFallsBackToToolChoiceNone(t *testing.T) {
	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "ok"}, nil
		},
	}

	const maxSequentialToolCalls = 2

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if opts.ToolChoice == llm.ToolChoiceNone {
				return scriptedFinalResponse("Voici ma synthèse."), nil
			}
			return scriptedToolCallResponse(llm.NewToolCall(fmt.Sprintf("call-%d", turn), "delegate_to_agenda", `{"goal":"encore"}`)), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{"agenda": agenda}, maxSequentialToolCalls)

	res, err := a.Execute(context.Background(), agent.Request{Input: "Boucle"})
	if err != nil {
		t.Fatalf("Execute aurait dû réussir par le repli ToolChoiceNone: %v", err)
	}
	if res.Reply != "Voici ma synthèse." {
		t.Errorf("reply = %q", res.Reply)
	}
}

// Une réponse vide du modèle (aucun appel d'outil, aucun texte) passe aussi
// par la conclusion forcée avant de remonter ErrEmptyReply.
func TestOrchestratorAgent_EmptyReplyGetsConclusionSecondChance(t *testing.T) {
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedFinalResponse(""), nil
			}
			return scriptedFinalResponse("Réponse rattrapée."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main", map[string]delegation.Specialist{}, 5)

	res, err := a.Execute(context.Background(), agent.Request{Input: "Salut"})
	if err != nil {
		t.Fatalf("Execute aurait dû être rattrapé par la conclusion: %v", err)
	}
	if res.Reply != "Réponse rattrapée." {
		t.Errorf("reply = %q", res.Reply)
	}
}

// fileCapableSpecialist déclare savoir manipuler des fichiers : c'est la
// capacité que l'orchestrateur interroge pour décider à qui transmettre
// les fichiers déjà reçus.
type fileCapableSpecialist struct {
	fakeSpecialist
	supports bool
}

func (s *fileCapableSpecialist) SupportsFiles() bool { return s.supports }

var _ delegation.FileCapable = &fileCapableSpecialist{}

// historyWithToolFile fabrique un historique où l'utilisateur a envoyé un
// fichier « outillage seulement » à un message précédent, puis a écrit sa
// demande au message suivant.
func historyWithToolFile(filename string) []agent.Message {
	return []agent.Message{
		{
			Role:    "user",
			Content: "Voici la vidéo.",
			Attachments: []media.Media{{
				Kind:     media.KindVideo,
				MimeType: "video/mp4",
				Filename: filename,
				Data:     []byte("octets"),
				ToolOnly: true,
			}},
		},
		{Role: "assistant", Content: "Bien reçue."},
	}
}

func delegateOnceClient(toolName string) *fakeCompletionClient {
	return &fakeCompletionClient{
		responseFunc: func(turn int, _ *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("c1", toolName, `{"goal":"Remove the logo"}`)), nil
			}
			return scriptedFinalResponse("C'est fait."), nil
		},
	}
}

// Un fichier envoyé à un message précédent reste atteignable par le
// délégué qui sait manipuler des fichiers.
func TestOrchestratorAgent_PassesEarlierFilesToFileCapableSpecialists(t *testing.T) {
	specialist := &fileCapableSpecialist{supports: true}
	specialist.executeFunc = func(context.Context, delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "fait"}, nil
	}

	a := agent.NewOrchestratorAgent(delegateOnceClient("delegate_to_workspace"), "system", "main",
		map[string]delegation.Specialist{"workspace": specialist}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "Enlève le logo de la vidéo",
		History: historyWithToolFile("video.mp4"),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if specialist.callCount() != 1 {
		t.Fatalf("le spécialiste devait être appelé une fois, %d appels", specialist.callCount())
	}
	got := specialist.calls[0].RecentAttachments
	if len(got) != 1 || got[0].Filename != "video.mp4" {
		t.Fatalf("fichiers antérieurs transmis = %+v", got)
	}
}

// Un délégué qui ne manipule pas de fichiers n'en reçoit aucun : la
// garantie d'isolation ne s'élargit que pour ceux qui en ont l'usage.
func TestOrchestratorAgent_WithholdsEarlierFilesFromOtherSpecialists(t *testing.T) {
	plain := &fakeSpecialist{
		executeFunc: func(context.Context, delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "vu"}, nil
		},
	}
	declining := &fileCapableSpecialist{supports: false}
	declining.executeFunc = func(context.Context, delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "vu"}, nil
	}

	for name, specialist := range map[string]delegation.Specialist{"vision": plain, "workspace": declining} {
		a := agent.NewOrchestratorAgent(delegateOnceClient("delegate_to_"+name), "system", "main",
			map[string]delegation.Specialist{name: specialist}, 5)

		if _, err := a.Execute(context.Background(), agent.Request{
			Input:   "Regarde ça",
			History: historyWithToolFile("video.mp4"),
		}); err != nil {
			t.Fatalf("Execute (%s): %v", name, err)
		}
	}

	if len(plain.calls) != 1 || len(plain.calls[0].RecentAttachments) != 0 {
		t.Fatalf("un spécialiste sans capacité fichiers ne doit rien recevoir: %+v", plain.calls)
	}
	if len(declining.calls) != 1 || len(declining.calls[0].RecentAttachments) != 0 {
		t.Fatalf("un spécialiste qui décline la capacité ne doit rien recevoir: %+v", declining.calls)
	}
}

// Une photo ordinaire — visible du modèle, donc non marquée « outillage
// seulement » — reste un fichier atteignable par un délégué qui sait
// éditer des fichiers. Ne retenir que les pièces d'outils condamnait
// « voici une photo » puis « enlève le logo » à travailler sur le mauvais
// fichier.
func TestOrchestratorAgent_EarlierFilesIncludeOrdinaryAttachments(t *testing.T) {
	specialist := &fileCapableSpecialist{supports: true}
	specialist.executeFunc = func(context.Context, delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "fait"}, nil
	}

	history := historyWithToolFile("video.mp4")
	history[0].Attachments = append(history[0].Attachments, media.Media{
		Kind:     media.KindImage,
		MimeType: "image/jpeg",
		Filename: "photo.jpg",
		Data:     []byte("jpeg"),
	})

	a := agent.NewOrchestratorAgent(delegateOnceClient("delegate_to_workspace"), "system", "main",
		map[string]delegation.Specialist{"workspace": specialist}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "Enlève le logo de la photo",
		History: history,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var names []string
	for _, m := range specialist.calls[0].RecentAttachments {
		names = append(names, m.Filename)
	}
	if len(names) != 2 {
		t.Fatalf("les deux fichiers de la conversation devaient être atteignables, obtenu %v", names)
	}
	if !strings.Contains(strings.Join(names, ","), "photo.jpg") {
		t.Fatalf("la photo doit être atteignable: %v", names)
	}
}

// L'orchestrateur doit savoir qu'un fichier reçu plus tôt reste traitable :
// sans cela, « voici une photo » puis « enlève le logo » lui présente un
// message sans pièce jointe, et il répond n'avoir rien reçu.
func TestOrchestratorAgent_AnnouncesEarlierFilesToTheModel(t *testing.T) {
	specialist := &fileCapableSpecialist{supports: true}
	specialist.executeFunc = func(context.Context, delegation.Request) (delegation.Result, error) {
		return delegation.Result{Summary: "fait"}, nil
	}

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
		map[string]delegation.Specialist{"workspace": specialist}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "Enlève le logo de la photo",
		History: historyWithToolFile("video.mp4"),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(input, "video.mp4") {
		t.Fatalf("les fichiers déjà reçus doivent être annoncés: %q", input)
	}
	if !strings.Contains(input, "delegate") {
		t.Fatalf("l'annonce doit orienter vers la délégation: %q", input)
	}
}

// Rien n'est annoncé quand aucun spécialiste ne sait ouvrir ces fichiers :
// promettre à l'utilisateur ce que le tour ne peut pas tenir est pire que
// se taire.
func TestOrchestratorAgent_AnnouncesNothingWithoutAFileSpecialist(t *testing.T) {
	plain := &fakeSpecialist{
		executeFunc: func(context.Context, delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "vu"}, nil
		},
	}

	var input string
	client := &fakeCompletionClient{
		responseFunc: func(_ int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleUser {
					input = msg.Content()
				}
			}
			return scriptedFinalResponse("Rien à faire."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"vision": plain}, 5)

	if _, err := a.Execute(context.Background(), agent.Request{
		Input:   "Enlève le logo",
		History: historyWithToolFile("video.mp4"),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(input, "video.mp4") {
		t.Fatalf("aucun fichier ne doit être annoncé sans spécialiste capable: %q", input)
	}
}

// Un spécialiste ne garde aucun état d'une délégation à l'autre : le
// re-solliciter indéfiniment épuise le délai du pipeline sans rien produire.
func TestOrchestratorAgent_StopsRepeatedDelegationsToTheSameAgent(t *testing.T) {
	specialist := &fakeSpecialist{
		executeFunc: func(context.Context, delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "je n'ai pas fini"}, nil
		},
	}

	var lastToolResult string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			for _, msg := range opts.Messages {
				if msg.Role() == llm.RoleTool {
					lastToolResult = msg.Content()
				}
			}
			if turn < 4 {
				return scriptedToolCallResponse(llm.NewToolCall("c", "delegate_to_workspace", `{"goal":"encore"}`)), nil
			}
			return scriptedFinalResponse("Je m'arrête là."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"workspace": specialist}, 8)

	if _, err := a.Execute(context.Background(), agent.Request{Input: "Enlève le logo"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if specialist.callCount() != 2 {
		t.Fatalf("le spécialiste devait être sollicité 2 fois au plus, %d appels", specialist.callCount())
	}
	if !strings.Contains(lastToolResult, "repartirait de zéro") {
		t.Fatalf("le refus doit expliquer pourquoi: %q", lastToolResult)
	}
}
