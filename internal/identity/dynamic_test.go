package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/model"
)

// newTestResolver construit un résolveur sur la configuration de test :
// ni l'origine ni le canal des cas ci-dessous n'y figurent, la
// résolution passe donc par la source dynamique.
func newTestResolver(t *testing.T) *identity.Resolver {
	t.Helper()

	resolver, err := identity.NewResolver(testConfig())
	if err != nil {
		t.Fatalf("construction du résolveur: %v", err)
	}

	return resolver
}

// Une même identité de messagerie peut être membre de plusieurs
// organisations : c'est le canal qui désigne lequel de ses profils parle.
// Sans ce filtre, la première ligne venue gagnait et la personne se
// voyait refuser la parole dans le groupe de son autre organisation.
func TestResolveDynamicPicksMemberOfChannelOrg(t *testing.T) {
	source := &multiOrgSource{
		members: map[string]identity.DynamicMember{
			"famille": {ID: "m-famille", OrgID: "famille", DisplayName: "William", Role: "member"},
			"atelier": {ID: "m-atelier", OrgID: "atelier", DisplayName: "William", Role: "member"},
		},
		channel: identity.DynamicChannel{OrgID: "atelier", Kind: model.ChannelGroup, Scope: model.ScopeGroup, DisplayName: "atelier IA"},
	}

	resolver := newTestResolver(t).WithDynamicSource(source)

	execIdentity, _, err := resolver.ResolveMessage(context.Background(), "whatsapp", "175913320902842@lid", "120000000000000001@g.us")
	if err != nil {
		t.Fatalf("résolution refusée: %v", err)
	}
	if execIdentity.PrincipalID != "m-atelier" {
		t.Errorf("profil retenu = %q, attendu m-atelier", execIdentity.PrincipalID)
	}
	if execIdentity.OrgID != "atelier" {
		t.Errorf("organisation = %q, attendue atelier", execIdentity.OrgID)
	}
}

// Une personne connue ailleurs mais sans compte dans l'organisation du
// canal est refusée — et le motif la distingue d'un parfait inconnu.
func TestResolveDynamicRejectsMemberOfAnotherOrg(t *testing.T) {
	source := &multiOrgSource{
		members: map[string]identity.DynamicMember{
			"famille": {ID: "m-famille", OrgID: "famille", DisplayName: "William", Role: "member"},
		},
		channel: identity.DynamicChannel{OrgID: "atelier", Kind: model.ChannelGroup, Scope: model.ScopeGroup},
	}

	resolver := newTestResolver(t).WithDynamicSource(source)

	_, _, err := resolver.ResolveMessage(context.Background(), "whatsapp", "175913320902842@lid", "120000000000000001@g.us")
	if !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("erreur = %v, attendue ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "sans compte dans l'organisation") {
		t.Errorf("motif peu explicite: %v", err)
	}
}

// multiOrgSource sert une même identité de messagerie dans plusieurs
// organisations.
type multiOrgSource struct {
	members map[string]identity.DynamicMember
	channel identity.DynamicChannel
}

func (s *multiOrgSource) FindMemberByOrigin(_ context.Context, _, _, orgID string) (identity.DynamicMember, bool, error) {
	if orgID == "" {
		for _, member := range s.members {
			return member, true, nil
		}
		return identity.DynamicMember{}, false, nil
	}
	member, found := s.members[orgID]
	return member, found, nil
}

func (s *multiOrgSource) FindChannel(context.Context, string, string) (identity.DynamicChannel, bool, error) {
	return s.channel, true, nil
}

func (s *multiOrgSource) OrgDisplayName(context.Context, string) (string, bool, error) {
	return "", false, nil
}
