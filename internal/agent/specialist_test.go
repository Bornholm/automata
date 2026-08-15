package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/agent"
	"github.com/bornholm/automata/internal/delegation"
)

// recordingAgent est un agent.Agent de test qui enregistre la dernière
// Request reçue, pour vérifier ce que AgentSpecialist lui transmet
// réellement.
type recordingAgent struct {
	lastReq agent.Request
	reply   string
}

func (a *recordingAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	a.lastReq = req
	return agent.Result{Reply: a.reply}, nil
}

var _ agent.Agent = &recordingAgent{}

func TestAgentSpecialist_Execute_DoesNotForwardHistory(t *testing.T) {
	underlying := &recordingAgent{reply: "Rien de prévu."}
	specialist := agent.NewAgentSpecialist("agenda", underlying)

	result, err := specialist.Execute(context.Background(), delegation.Request{
		AgentID:       "agenda",
		Goal:          "Vérifier l'agenda de demain",
		RelevantInput: "L'utilisateur veut savoir s'il a des rendez-vous demain.",
		Constraints:   []string{"répondre en une phrase"},
	})
	if err != nil {
		t.Fatalf("Execute a échoué: %v", err)
	}

	if result.Summary != "Rien de prévu." {
		t.Fatalf("Summary inattendu: %q", result.Summary)
	}

	if len(underlying.lastReq.History) != 0 {
		t.Fatalf("aucun historique ne devrait être transmis au spécialiste, obtenu %d entrées", len(underlying.lastReq.History))
	}

	if !strings.Contains(underlying.lastReq.Input, "Vérifier l'agenda de demain") {
		t.Errorf("l'input transmis devrait contenir le goal, obtenu: %q", underlying.lastReq.Input)
	}
	if !strings.Contains(underlying.lastReq.Input, "rendez-vous demain") {
		t.Errorf("l'input transmis devrait contenir le relevant_input, obtenu: %q", underlying.lastReq.Input)
	}
	if !strings.Contains(underlying.lastReq.Input, "répondre en une phrase") {
		t.Errorf("l'input transmis devrait contenir les contraintes, obtenu: %q", underlying.lastReq.Input)
	}
}

func TestAgentSpecialist_Execute_WrapsUnderlyingError(t *testing.T) {
	wantErr := errors.New("boom")
	specialist := agent.NewAgentSpecialist("agenda", failingAgent{err: wantErr})

	_, err := specialist.Execute(context.Background(), delegation.Request{Goal: "test"})
	if err == nil {
		t.Fatal("erreur attendue")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("l'erreur devrait envelopper %v, obtenu: %v", wantErr, err)
	}
	if !strings.Contains(err.Error(), "agenda") {
		t.Errorf("l'erreur devrait mentionner l'agentID, obtenu: %v", err)
	}
}

type failingAgent struct {
	err error
}

func (f failingAgent) Execute(ctx context.Context, req agent.Request) (agent.Result, error) {
	return agent.Result{}, f.err
}
