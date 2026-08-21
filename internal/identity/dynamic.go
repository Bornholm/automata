package identity

import (
	"context"
	"fmt"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/model"
)

// DynamicMember décrit un membre rattaché en ligne (tables du socle SaaS)
// plutôt que déclaré dans la configuration YAML.
type DynamicMember struct {
	ID          string
	OrgID       string
	DisplayName string
	// Role : member | owner | readonly (voir persistence.MemberRole*).
	Role string
}

// DynamicChannel décrit un canal rattaché en ligne par consommation d'un
// jeton de liaison.
type DynamicChannel struct {
	OrgID       string
	Kind        model.ChannelKind
	Scope       model.Scope
	ScopeID     model.ScopeID
	DisplayName string
	// MemberID n'est renseigné que pour un canal privé.
	MemberID string
}

// DynamicSource donne accès aux tenants enregistrés en base. Implémentée
// dans internal/registry sur la persistance ; nil dans les configurations
// purement YAML (tests, instances non migrées).
//
// Les deux méthodes retournent (_, false, nil) quand rien ne correspond :
// l'absence n'est pas une erreur, la configuration reste la source
// prioritaire et le pipeline d'ingress traite le cas « inconnu ».
type DynamicSource interface {
	// FindMemberByOrigin retourne le membre lié à une identité de
	// messagerie (provider + identifiant externe) dans une organisation
	// donnée. orgID vide cherche dans toutes les organisations : une
	// même personne peut être membre de plusieurs.
	FindMemberByOrigin(ctx context.Context, provider, externalUserID, orgID string) (DynamicMember, bool, error)
	// FindChannel retourne la liaison d'un canal.
	FindChannel(ctx context.Context, provider, channelID string) (DynamicChannel, bool, error)
	// OrgDisplayName retourne le nom affiché d'une organisation en base.
	OrgDisplayName(ctx context.Context, orgID string) (string, bool, error)
}

// WithDynamicSource attache une source dynamique au résolveur. Sans elle,
// le comportement historique (configuration seule) est strictement
// préservé.
func (r *Resolver) WithDynamicSource(source DynamicSource) *Resolver {
	r.dynamic = source
	return r
}

// resolveDynamic tente la résolution en base, après échec de la
// configuration. La frontière d'organisation est vérifiée ici comme
// ailleurs : un membre ne peut jamais parler dans le canal d'une autre
// organisation que la sienne.
func (r *Resolver) resolveDynamic(ctx context.Context, provider, externalUserID, channelID string) (model.ExecutionIdentity, model.Conversation, error) {
	// Le canal d'abord : c'est lui qui désigne l'organisation, et donc
	// lequel des profils de la personne prend la parole. Une même
	// identité de messagerie peut être membre de plusieurs
	// organisations — le groupe familial et celui de l'employeur, sur le
	// même compte.
	channel, found, err := r.dynamic.FindChannel(ctx, provider, channelID)
	if err != nil {
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: recherche du canal %s/%s: %w", provider, channelID, err)
	}
	if !found {
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: canal %s/%s: %w", provider, channelID, apperr.ErrUnknownChannel)
	}

	member, found, err := r.dynamic.FindMemberByOrigin(ctx, provider, externalUserID, channel.OrgID)
	if err != nil {
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: recherche du membre %s/%s: %w", provider, externalUserID, err)
	}
	if !found {
		// La personne est peut-être connue ailleurs : le dire au journal
		// évite de confondre « inconnu au bataillon » avec « pas de
		// compte dans CETTE organisation », deux situations qui
		// n'appellent pas le même geste.
		if _, elsewhere, err := r.dynamic.FindMemberByOrigin(ctx, provider, externalUserID, ""); err == nil && elsewhere {
			return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf(
				"identity: origine %s/%s connue, mais sans compte dans l'organisation %q du canal: %w",
				provider, externalUserID, channel.OrgID, apperr.ErrUnauthorized)
		}

		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: origine %s/%s: %w", provider, externalUserID, apperr.ErrUnknownOrigin)
	}

	// Un canal privé n'appartient qu'à son propriétaire : quelqu'un d'autre
	// qui écrirait dans ce fil ne doit pas hériter de sa portée personnelle.
	if channel.Kind == model.ChannelPrivate && channel.MemberID != "" && channel.MemberID != member.ID {
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: canal privé %s/%s d'un autre membre: %w", provider, channelID, apperr.ErrUnauthorized)
	}

	orgDisplayName := r.cfg.OrganizationDisplayName(member.OrgID)
	if name, found, err := r.dynamic.OrgDisplayName(ctx, member.OrgID); err == nil && found {
		orgDisplayName = name
	}

	// La portée personnelle est toujours celle du membre qui parle, jamais
	// celle enregistrée sur le canal : deux membres d'une même organisation
	// ne partagent pas leur mémoire personnelle.
	scope, scopeID := channel.Scope, channel.ScopeID
	if scope == model.ScopePersonal {
		scopeID = model.ScopeID(member.ID)
	}

	conversationID := model.ConversationID(provider + ":" + channelID)

	identity := model.ExecutionIdentity{
		Trigger:              model.TriggerMessage,
		PrincipalID:          model.PrincipalID(member.ID),
		PrincipalDisplayName: member.DisplayName,
		OrgID:                model.OrgID(member.OrgID),
		OrgDisplayName:       orgDisplayName,
		ConversationID:       conversationID,
		Provider:             provider,
		ChannelID:            channelID,
		ChannelKind:          channel.Kind,
		Scope:                scope,
		ScopeID:              scopeID,
	}

	conversation := model.Conversation{
		ID:          conversationID,
		OrgID:       model.OrgID(member.OrgID),
		Provider:    provider,
		ChannelID:   channelID,
		Kind:        channel.Kind,
		Scope:       scope,
		ScopeID:     scopeID,
		DisplayName: channel.DisplayName,
	}

	return identity, conversation, nil
}

// DynamicRolePermissions donne le jeu de permissions d'un rôle web. Les
// membres rattachés en ligne n'ont pas de rôle configuré : leurs droits
// sont figés en code, délibérément — un rôle web est un contrat produit
// (ce que le client a acheté), pas un réglage d'exploitation.
func DynamicRolePermissions(role string) map[string]struct{} {
	base := []string{
		"memory.personal.read", "memory.personal.write", "memory.personal.delete",
		"memory.group.read", "memory.group.write",
		"reminder.personal.read", "reminder.personal.write", "reminder.personal.delete",
		"reminder.group.read", "reminder.group.write",
	}

	var perms []string
	switch role {
	case "readonly":
		perms = []string{"memory.personal.read", "memory.group.read", "reminder.personal.read", "reminder.group.read"}
	case "owner":
		perms = append(append([]string{}, base...),
			"memory.group.delete", "memory.org.read", "memory.org.write",
			"reminder.group.delete", "reminder.org.read")
	default:
		perms = base
	}

	set := make(map[string]struct{}, len(perms))
	for _, perm := range perms {
		set[perm] = struct{}{}
	}

	return set
}
