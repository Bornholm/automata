package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/bornholm/automata/internal/delegation"
)

// AgentSpecialist adapte un Agent existant (typiquement un GenAIAgent
// construit par Registry pour un agent de type "specialist") en
// delegation.Specialist. Elle vit dans le package agent, et non dans
// internal/delegation, pour éviter un cycle d'import : OrchestratorAgent (ce
// package) a besoin de delegation.Specialist pour exposer les délégués comme
// outils, donc delegation ne doit jamais importer agent.
type AgentSpecialist struct {
	agentID string
	agent   Agent
}

// NewAgentSpecialist construit un AgentSpecialist ciblant agentID, en
// déléguant l'exécution à a.
func NewAgentSpecialist(agentID string, a Agent) *AgentSpecialist {
	return &AgentSpecialist{agentID: agentID, agent: a}
}

// Execute implémente delegation.Specialist. Le contexte transmis au
// spécialiste est intentionnellement limité : seuls req.Goal,
// req.RelevantInput et req.Constraints (explicitement fournis par
// l'orchestrateur) composent l'entrée envoyée à l'agent sous-jacent. Aucun
// historique de la conversation principale n'est transmis (Request.History
// reste vide) : c'est la garantie d'isolation exigée par PLAN.md §6.3, "le
// sous-agent ne doit pas recevoir automatiquement l'intégralité de
// l'historique".
func (s *AgentSpecialist) Execute(ctx context.Context, req delegation.Request) (delegation.Result, error) {
	result, err := s.agent.Execute(ctx, Request{
		Identity: req.Identity,
		Input:    buildDelegationInput(req),
		// History volontairement omis : voir le commentaire de la méthode.
	})
	if err != nil {
		return delegation.Result{}, fmt.Errorf("agent: délégation vers %q: %w", s.agentID, err)
	}

	return delegation.Result{Summary: result.Reply}, nil
}

// buildDelegationInput compose le message utilisateur envoyé au spécialiste
// à partir des seuls champs explicitement transmis par l'orchestrateur.
func buildDelegationInput(req delegation.Request) string {
	var b strings.Builder

	b.WriteString("Objectif: ")
	b.WriteString(req.Goal)

	if strings.TrimSpace(req.RelevantInput) != "" {
		b.WriteString("\n\nContexte pertinent:\n")
		b.WriteString(req.RelevantInput)
	}

	if len(req.Constraints) > 0 {
		b.WriteString("\n\nContraintes:\n")
		for _, c := range req.Constraints {
			b.WriteString("- ")
			b.WriteString(c)
			b.WriteString("\n")
		}
	}

	return strings.TrimSpace(b.String())
}

var _ delegation.Specialist = &AgentSpecialist{}
