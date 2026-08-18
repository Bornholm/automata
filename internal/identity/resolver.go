// Package identity résout une identité d'exécution fiable à partir des
// informations extraites d'un message Courier (fournisseur, identifiant
// externe de l'auteur, identifiant du canal). La résolution se fonde
// uniquement sur la configuration chargée et validée : jamais sur du
// contenu fourni par le LLM ou l'utilisateur.
package identity

import (
	"context"
	"fmt"
	"slices"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

// Resolver résout les identités d'exécution à partir d'index précalculés sur
// la configuration.
type Resolver struct {
	cfg *config.Config

	// originIndex associe "provider|externalUserID" au principal résolu.
	originIndex map[string]string

	// channelIndex associe "provider|channelID" au canal configuré.
	channelIndex map[string]config.Channel

	// principalIndex associe l'identifiant de principal au principal
	// configuré.
	principalIndex map[string]config.Principal
}

// NewResolver construit un Resolver et ses index à partir de cfg. cfg doit
// avoir été chargé et validé (voir config.Load).
func NewResolver(cfg *config.Config) (*Resolver, error) {
	if cfg == nil {
		return nil, fmt.Errorf("identity: configuration requise")
	}

	r := &Resolver{
		cfg:            cfg,
		originIndex:    make(map[string]string, len(cfg.Origins)),
		channelIndex:   make(map[string]config.Channel, len(cfg.Channels)),
		principalIndex: make(map[string]config.Principal, len(cfg.Identities.Principals)),
	}

	for _, p := range cfg.Identities.Principals {
		r.principalIndex[p.ID] = p
	}

	for _, o := range cfg.Origins {
		key := originKey(o.Provider, o.ExternalUserID)
		r.originIndex[key] = o.PrincipalID
	}

	for _, ch := range cfg.Channels {
		key := channelKey(ch.Provider, ch.ChannelID)
		r.channelIndex[key] = ch
	}

	return r, nil
}

func originKey(provider, externalUserID string) string {
	return provider + "|" + externalUserID
}

func channelKey(provider, channelID string) string {
	return provider + "|" + channelID
}

// ResolveMessage résout l'identité d'exécution et la conversation
// applicative pour un message entrant identifié par provider (fournisseur
// Courier), externalUserID (identifiant externe de l'auteur, résolu via la
// table origins) et channelID (identifiant externe du canal, résolu via la
// table channels).
//
// La vérification de la mention dans les groupes (règle §3.3 du plan) n'est
// PAS effectuée ici : elle relève de la responsabilité de l'ingress/transport
// (Phase 5), qui a accès au contenu brut du message. La résolution
// d'identité se limite à établir qui parle, sur quel canal, avec quelle
// portée — indépendamment du contenu du message.
func (r *Resolver) ResolveMessage(ctx context.Context, provider, externalUserID, channelID string) (model.ExecutionIdentity, model.Conversation, error) {
	principalID, ok := r.originIndex[originKey(provider, externalUserID)]
	if !ok {
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: origine %s/%s: %w", provider, externalUserID, apperr.ErrUnknownOrigin)
	}

	ch, ok := r.channelIndex[channelKey(provider, channelID)]
	if !ok {
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: canal %s/%s: %w", provider, channelID, apperr.ErrUnknownChannel)
	}

	if ch.Kind == config.ChannelKindGroup {
		if !slices.Contains(ch.Members, principalID) {
			return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: principal %q non membre du groupe %s/%s: %w", principalID, provider, channelID, apperr.ErrUnauthorized)
		}
	}

	var channelKind model.ChannelKind

	switch ch.Kind {
	case config.ChannelKindPrivate:
		channelKind = model.ChannelPrivate
	case config.ChannelKindGroup:
		channelKind = model.ChannelGroup
	default:
		return model.ExecutionIdentity{}, model.Conversation{}, fmt.Errorf("identity: canal %s/%s: type de canal invalide %q: %w", provider, channelID, ch.Kind, apperr.ErrUnknownChannel)
	}

	conversationID := model.ConversationID(provider + ":" + channelID)

	identity := model.ExecutionIdentity{
		Trigger:        model.TriggerMessage,
		PrincipalID:    model.PrincipalID(principalID),
		OrgID:          model.OrgID(ch.OrgID),
		OrgDisplayName: r.cfg.OrganizationDisplayName(ch.OrgID),
		ConversationID: conversationID,
		Provider:       provider,
		ChannelID:      channelID,
		ChannelKind:    channelKind,
		Scope:          model.Scope(ch.Scope),
		ScopeID:        model.ScopeID(ch.ScopeID),
	}

	conversation := model.Conversation{
		ID:          conversationID,
		OrgID:       model.OrgID(ch.OrgID),
		Provider:    provider,
		ChannelID:   channelID,
		Kind:        channelKind,
		Scope:       model.Scope(ch.Scope),
		ScopeID:     model.ScopeID(ch.ScopeID),
		DisplayName: ch.DisplayName,
	}

	return identity, conversation, nil
}

// EffectivePermissions résout l'ensemble des permissions effectives d'un
// principal dans une organisation donnée, à partir des rôles configurés.
//
// orgID doit désigner une organisation déclarée, et le principal doit lui
// appartenir : une instance peut en servir plusieurs, et un principal connu
// d'une organisation n'a aucun droit dans une autre.
func EffectivePermissions(cfg *config.Config, orgID model.OrgID, principalID model.PrincipalID) (map[string]struct{}, error) {
	if cfg == nil {
		return nil, fmt.Errorf("identity: configuration requise")
	}

	if _, ok := cfg.LookupOrganization(string(orgID)); !ok {
		return nil, fmt.Errorf("identity: organisation %q inconnue: %w", orgID, apperr.ErrUnauthorized)
	}

	var principal *config.Principal

	for i := range cfg.Identities.Principals {
		if cfg.Identities.Principals[i].ID == string(principalID) {
			principal = &cfg.Identities.Principals[i]
			break
		}
	}

	if principal == nil {
		return nil, fmt.Errorf("identity: principal %q inconnu: %w", principalID, apperr.ErrUnauthorized)
	}

	if !cfg.PrincipalInOrganization(string(principalID), string(orgID)) {
		return nil, fmt.Errorf("identity: principal %q hors de l'organisation %q: %w", principalID, orgID, apperr.ErrUnauthorized)
	}

	permissions := make(map[string]struct{})

	for _, roleName := range principal.Roles {
		role, ok := cfg.Identities.Roles[roleName]
		if !ok {
			return nil, fmt.Errorf("identity: rôle %q inconnu pour le principal %q: %w", roleName, principalID, apperr.ErrUnauthorized)
		}

		for _, perm := range role.Permissions {
			permissions[perm] = struct{}{}
		}
	}

	return permissions, nil
}
