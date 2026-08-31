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

// Les membres rattachés en ligne doivent pouvoir programmer une tâche.
//
// Ils ne le pouvaient pas : task.* manquait à ce jeu, et comme la
// configuration des rôles a migré en base, plus personne ne pouvait
// l'accorder. L'agent proposait l'outil, annonçait « task.group.write
// refusée », et personne — pas même l'exploitant — n'avait de moyen de
// lever le refus. Une capacité annoncée que rien ne peut débloquer est pire
// qu'une capacité absente.
func TestDynamicRolePermissions_MembersCanScheduleTasks(t *testing.T) {
	member := identity.DynamicRolePermissions("member")

	for _, permission := range []string{
		"task.personal.read", "task.personal.write", "task.personal.delete",
		"task.group.read", "task.group.write",
	} {
		if _, ok := member[permission]; !ok {
			t.Errorf("permission %q absente du rôle « member »", permission)
		}
	}
}

// Qui peut poser une échéance peut la retirer.
//
// La suppression de groupe était réservée au propriétaire : quelqu'un
// programmait une tâche hebdomadaire, se trompait d'heure, la reprogrammait
// — et ne pouvait pas annuler la première. Deux envois chaque lundi, sans
// recours. Pouvoir créer sans pouvoir défaire enferme la personne dans son
// erreur ; le vrai garde-fou est la portée, une échéance n'étant annulable
// que depuis la conversation où elle a été posée.
func TestDynamicRolePermissions_MembersCanCancelWhatTheyScheduled(t *testing.T) {
	member := identity.DynamicRolePermissions("member")

	for _, permission := range []string{"task.group.delete", "reminder.group.delete"} {
		if _, ok := member[permission]; !ok {
			t.Errorf("permission %q absente du rôle « member » : il pourrait créer sans pouvoir annuler", permission)
		}
	}

	// La mémoire de groupe reste protégée : un souvenir accumulé n'est pas
	// une échéance qu'on vient de poser.
	if _, ok := member["memory.group.delete"]; ok {
		t.Error("memory.group.delete ne doit pas être accordé au rôle « member »")
	}
}

// Un readonly lit les tâches sans en créer : la règle vaut ici comme
// partout ailleurs.
func TestDynamicRolePermissions_ReadonlyCannotScheduleTasks(t *testing.T) {
	readonly := identity.DynamicRolePermissions("readonly")

	if _, ok := readonly["task.group.read"]; !ok {
		t.Error("un readonly devrait pouvoir lister les tâches du groupe")
	}
	for _, permission := range []string{"task.group.write", "task.personal.write", "task.group.delete"} {
		if _, ok := readonly[permission]; ok {
			t.Errorf("un readonly ne doit pas obtenir %q", permission)
		}
	}
}

// Le propriétaire garde ce qui lui est propre : la vue d'organisation et la
// suppression des souvenirs du groupe.
func TestDynamicRolePermissions_OwnerKeepsOrgWideRights(t *testing.T) {
	owner := identity.DynamicRolePermissions("owner")

	for _, permission := range []string{
		"task.org.read", "reminder.org.read",
		"memory.group.delete", "memory.org.read", "memory.org.write",
	} {
		if _, ok := owner[permission]; !ok {
			t.Errorf("permission %q absente du rôle « owner »", permission)
		}
	}
}
