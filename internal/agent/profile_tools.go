package agent

import (
	"context"
	"fmt"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/observability"
)

// ProfileLinkGenerator produit un lien de profil temporaire à usage unique
// pour un membre. Implémentée dans internal/registry sur la persistance ;
// sa valeur nil désactive l'outil.
type ProfileLinkGenerator interface {
	// GenerateProfileLink retourne l'URL complète, ou ok à faux si le
	// principal n'est pas un membre enregistré en ligne (les principals
	// déclarés en configuration n'ont pas de profil web).
	GenerateProfileLink(ctx context.Context, orgID, principalID string) (url string, ok bool, err error)
}

// ProfileTools expose l'outil open_profile_link aux agents
// orchestrateurs : l'assistant ouvre lui-même la page de profil de son
// interlocuteur — c'est le pivot du parti pris « conversationnel d'abord »
// (aucun mot de passe, aucun portail à connaître : le canal de messagerie
// est l'authentification).
//
// Valeur zéro = outil désactivé, comme MemoryTools et ReminderTools.
type ProfileTools struct {
	Generator ProfileLinkGenerator
	// Enabled est recroisé avec le drapeau propre à chaque agent
	// (agents.<nom>.profile_link).
	Enabled bool
	Metrics *observability.Metrics
}

func (t ProfileTools) enabled() bool {
	return t.Generator != nil && t.Enabled
}

// buildProfileTools retourne les outils de profil exposés au modèle.
func (t ProfileTools) buildProfileTools(identity model.ExecutionIdentity) []llm.Tool {
	if !t.enabled() {
		return nil
	}

	return []llm.Tool{t.newOpenProfileLinkTool(identity)}
}

// newOpenProfileLinkTool construit open_profile_link. La description est en
// anglais (elle part au modèle) ; le lien retourné, lui, est destiné à être
// recopié tel quel dans une réponse française.
func (t ProfileTools) newOpenProfileLinkTool(identity model.ExecutionIdentity) llm.Tool {
	schema := llm.NewJSONSchema()

	return llm.NewFuncTool(
		"open_profile_link",
		"Give the user a private link to their own profile page, where they can manage their recovery email, "+
			"see their credit balance and top it up. Use it whenever they ask about their account, their credits, "+
			"paying, an invoice, or say the service seems paused. The link is single-use and expires in 15 minutes: "+
			"generate a fresh one every time, never repeat an old one. It opens their own profile only — never "+
			"someone else's, and never an administration page.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			url, ok, err := t.Generator.GenerateProfileLink(ctx, string(identity.OrgID), string(identity.PrincipalID))
			if err != nil {
				return nil, fmt.Errorf("génération du lien de profil: %w", err)
			}
			if !ok {
				return llm.NewToolResult("No profile page is available for this user: their account is not managed online."), nil
			}

			t.Metrics.IncProfileLink()

			return llm.NewToolResult("Private profile link, valid 15 minutes, single use — give it to the user as-is, " +
				"without altering it: " + url), nil
		},
	)
}
