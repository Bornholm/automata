package agent

import (
	"context"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/usage"
)

// withUsageAttribution attache au contexte l'attribution comptable du tour
// (voir internal/usage) : chaque implémentation d'Agent l'appelle en tête
// d'Execute avec son propre nom. Un spécialiste délégué remplace ainsi
// l'attribution de l'orchestrateur le temps de sa délégation — ses appels
// LLM lui sont attribués, pas à l'orchestrateur qui a délégué.
func withUsageAttribution(ctx context.Context, identity model.ExecutionIdentity, agentName string) context.Context {
	return usage.ContextWithAttribution(ctx, usage.Attribution{
		OrgID:          string(identity.OrgID),
		PrincipalID:    string(identity.PrincipalID),
		ConversationID: string(identity.ConversationID),
		Component:      usage.ComponentAgent,
		Agent:          agentName,
	})
}
