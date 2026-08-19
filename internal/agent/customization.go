package agent

import (
	"context"
	"strings"

	"github.com/bornholm/genai/llm"
)

// OrgCustomization décrit ce qu'une organisation change au comportement
// standard de l'assistant. Sa valeur zéro est le comportement par défaut.
type OrgCustomization struct {
	// PromptExtra est ajouté au prompt système, après les règles de
	// l'instance : une organisation précise le ton ou le contexte, elle ne
	// réécrit jamais les invariants.
	PromptExtra string
	// DisabledAgents nomme les spécialistes retirés à cette organisation.
	DisabledAgents []string
	// MaxToolCalls resserre le plafond d'appels d'outils ; il ne peut que
	// l'abaisser, jamais le relever au-delà de ce que l'agent autorise.
	MaxToolCalls int
}

// OrgCustomizer fournit la personnalisation d'une organisation.
// Implémenté par internal/registry sur la persistance.
type OrgCustomizer interface {
	CustomizationFor(ctx context.Context, orgID string) (OrgCustomization, error)
}

// delegationToolName donne le nom de l'outil de délégation d'un
// spécialiste — le même que celui construit par newDelegationTool.
func delegationToolName(agentID string) string { return "delegate_to_" + agentID }

// filterDelegationTools retire les outils de délégation des spécialistes
// dont l'organisation ne dispose pas. Les autres outils (mémoire, rappels,
// profil) ne sont pas concernés : ils relèvent des permissions, pas du
// forfait.
func filterDelegationTools(tools []llm.Tool, disabled []string) []llm.Tool {
	if len(disabled) == 0 {
		return tools
	}

	excluded := make(map[string]struct{}, len(disabled))
	for _, name := range disabled {
		name = strings.TrimSpace(name)
		if name != "" {
			excluded[delegationToolName(name)] = struct{}{}
		}
	}

	kept := tools[:0]
	for _, tool := range tools {
		if _, skip := excluded[tool.Name()]; skip {
			continue
		}
		kept = append(kept, tool)
	}

	return kept
}
