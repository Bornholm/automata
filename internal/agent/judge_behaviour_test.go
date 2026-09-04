package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
)

// fakeJudge est un agent.Judge de test : verdict fixe, appels comptés.
type fakeJudge struct {
	mu        sync.Mutex
	grounding agent.Grounding
	err       error
	calls     int
	requests  []string
	replies   []string
}

func (j *fakeJudge) ReviewGrounding(_ context.Context, _ model.OrgID, request, reply string) (agent.Grounding, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.calls++
	j.requests = append(j.requests, request)
	j.replies = append(j.replies, reply)

	return j.grounding, j.err
}

func (j *fakeJudge) callCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

// Un verdict négatif rend le tour au modèle, avec la raison. Reproduit le
// cas de production : « Le service de calendrier est indisponible » écrit
// sans qu'aucun outil ait été appelé.
func TestOrchestratorAgent_UngroundedReplyIsHandedBack(t *testing.T) {
	judge := &fakeJudge{grounding: agent.Grounding{
		Grounded: false,
		Reason:   "you stated the calendar service is unavailable, but you never called a calendar tool",
	}}

	var secondPrompt string
	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedFinalResponse("Le service de calendrier est indisponible. Réessayez plus tard."), nil
			}
			// La relance rejoue la demande, consigne intégrée, et ne
			// remontre jamais le brouillon fautif.
			if n := len(opts.Messages); n > 0 {
				secondPrompt = opts.Messages[n-1].Content()
			}
			for _, m := range opts.Messages {
				if strings.Contains(m.Content(), "Le service de calendrier est indisponible") {
					t.Errorf("le brouillon fautif a été remontré au modèle: %q", m.Content())
				}
			}
			return scriptedFinalResponse("Je n'ai pas consulté ton agenda. Veux-tu que je le fasse ?"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"agenda": &fakeSpecialist{}}, 5).WithJudge(judge)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Quels sont mes rendez-vous ?"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if judge.callCount() != 1 {
		t.Fatalf("le juge aurait dû être consulté une fois, appelé %d fois", judge.callCount())
	}
	if !strings.Contains(secondPrompt, "you never called a calendar tool") {
		t.Errorf("la relance ne transporte pas la raison du juge: %q", secondPrompt)
	}
	// La demande de la personne est rejouée avec la consigne : c'est à elle
	// que le modèle répond, pas au correcteur.
	if !strings.Contains(secondPrompt, "Quels sont mes rendez-vous ?") {
		t.Errorf("la demande n'a pas été rejouée: %q", secondPrompt)
	}
	if !strings.Contains(result.Reply, "pas consulté") {
		t.Errorf("la réponse reprise n'a pas été rendue: %q", result.Reply)
	}
	// Le juge lit la demande et la réponse initiale, rien d'autre.
	if judge.requests[0] != "Quels sont mes rendez-vous ?" {
		t.Errorf("demande transmise au juge inattendue: %q", judge.requests[0])
	}
	if !strings.Contains(judge.replies[0], "indisponible") {
		t.Errorf("réponse transmise au juge inattendue: %q", judge.replies[0])
	}
}

// Un verdict positif ne change rien, et ne coûte aucune complétion de plus.
func TestOrchestratorAgent_GroundedReplyPassesThrough(t *testing.T) {
	judge := &fakeJudge{grounding: agent.Grounding{Grounded: true}}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Bonjour ! Que puis-je pour toi ?"), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"agenda": &fakeSpecialist{}}, 5).WithJudge(judge)

	result, err := a.Execute(context.Background(), agent.Request{Input: "salut"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Reply != "Bonjour ! Que puis-je pour toi ?" {
		t.Errorf("la réponse a été modifiée: %q", result.Reply)
	}
	if client.callCount() != 1 {
		t.Errorf("une seule complétion attendue, obtenu %d", client.callCount())
	}
}

// Un juge en panne ne coûte pas sa réponse à la personne qui attend : le
// tour passe inchangé.
func TestOrchestratorAgent_JudgeFailureLeavesTheReplyAlone(t *testing.T) {
	judge := &fakeJudge{err: errors.New("modèle du rôle judge non résolu")}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			return scriptedFinalResponse("Le service de calendrier est indisponible."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"agenda": &fakeSpecialist{}}, 5).WithJudge(judge)

	result, err := a.Execute(context.Background(), agent.Request{Input: "Quels sont mes rendez-vous ?"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Reply != "Le service de calendrier est indisponible." {
		t.Errorf("la réponse a été modifiée malgré un juge en panne: %q", result.Reply)
	}
	if client.callCount() != 1 {
		t.Errorf("aucune relance attendue, %d complétions", client.callCount())
	}
}

// Un tour qui a appelé un outil a observé quelque chose : le juge n'est pas
// consulté, et la personne ne paie pas cette complétion.
func TestOrchestratorAgent_JudgeIsSkippedWhenAToolWasCalled(t *testing.T) {
	judge := &fakeJudge{grounding: agent.Grounding{Grounded: false, Reason: "ne devrait pas être consulté"}}

	agenda := &fakeSpecialist{
		executeFunc: func(ctx context.Context, req delegation.Request) (delegation.Result, error) {
			return delegation.Result{Summary: "Rien demain."}, nil
		},
	}

	client := &fakeCompletionClient{
		responseFunc: func(turn int, opts *llm.ChatCompletionOptions) (llm.ChatCompletionResponse, error) {
			if turn == 0 {
				return scriptedToolCallResponse(llm.NewToolCall("call-1", "delegate_to_agenda", `{"goal":"agenda","relevant_input":"agenda"}`)), nil
			}
			return scriptedFinalResponse("Rien demain."), nil
		},
	}

	a := agent.NewOrchestratorAgent(client, "system", "main",
		map[string]delegation.Specialist{"agenda": agenda}, 5).WithJudge(judge)

	if _, err := a.Execute(context.Background(), agent.Request{Input: "Quels sont mes rendez-vous ?"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if judge.callCount() != 0 {
		t.Fatalf("le juge a été consulté alors qu'un outil avait été appelé (%d fois)", judge.callCount())
	}
}
