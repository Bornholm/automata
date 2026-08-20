// Package authorization applique les règles de cloisonnement des portées
// (personal, group, org) avant toute connexion externe ou appel LLM. Les
// règles fondamentales appliquées ici sont invariantes : elles ne sont pas
// désactivables par la configuration (voir PLAN.md §2.3 et §3.2).
package authorization

import (
	"context"
	"fmt"
	"strings"

	"github.com/bornholm/automata/internal/apperr"
	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/identity"
	"github.com/bornholm/automata/internal/model"
)

// AuthorizationRequest décrit une demande d'autorisation d'accès à une
// ressource, dans une portée cible, pour l'identité d'exécution courante.
type AuthorizationRequest struct {
	Identity      model.ExecutionIdentity
	Permission    string // ex: "memory.personal.write"
	TargetOrgID   model.OrgID
	TargetScope   model.Scope
	TargetScopeID model.ScopeID
}

// Authorizer vérifie qu'une identité d'exécution est autorisée à effectuer
// une opération donnée, dans une portée cible donnée.
type Authorizer struct {
	pluginDomains PluginDomainSource
	cfg           *config.Config

	// members, s'il est renseigné, donne le rôle web des membres
	// enregistrés en base (voir WithMemberRoles).
	members MemberRoleSource
}

// NewAuthorizer construit un Authorizer à partir de la configuration
// chargée et validée.
func NewAuthorizer(cfg *config.Config) *Authorizer {
	return &Authorizer{cfg: cfg}
}

// Authorize vérifie req selon, dans l'ordre :
//  1. l'absence de traversée d'organisation ;
//  2. la présence de la permission requise dans les permissions effectives
//     du principal, avec un scope cohérent avec la portée cible ;
//  3. les règles fondamentales de cloisonnement, invariantes, selon le type
//     de canal (privé/groupe) ou le déclencheur (cron).
//
// Toute violation retourne une erreur enveloppant apperr.ErrUnauthorized.
func (a *Authorizer) Authorize(ctx context.Context, req AuthorizationRequest) error {
	if req.TargetOrgID != req.Identity.OrgID {
		return fmt.Errorf("authorization: traversée d'organisation refusée (identité: %q, cible: %q): %w", req.Identity.OrgID, req.TargetOrgID, apperr.ErrUnauthorized)
	}

	action, err := permissionScopeAction(req.Permission)
	if err != nil {
		return fmt.Errorf("authorization: %w: %w", err, apperr.ErrUnauthorized)
	}

	permScope, permAction := action.scope, action.action

	if permScope != string(req.TargetScope) {
		return fmt.Errorf("authorization: portée de la permission %q incohérente avec la portée cible %q: %w", req.Permission, req.TargetScope, apperr.ErrUnauthorized)
	}

	permissions, err := a.effectivePermissions(ctx, req.Identity)
	if err != nil {
		return fmt.Errorf("authorization: résolution des permissions effectives: %w", err)
	}

	if _, ok := permissions[req.Permission]; !ok {
		return fmt.Errorf("authorization: permission %q non accordée au principal %q: %w", req.Permission, req.Identity.PrincipalID, apperr.ErrUnauthorized)
	}

	if err := checkInvariantRules(req, permAction); err != nil {
		return err
	}

	return nil
}

// MemberRoleSource retourne le rôle web d'un membre enregistré en base
// (socle SaaS). Implémentée dans internal/registry ; nil dans les
// configurations purement YAML.
type MemberRoleSource interface {
	MemberRole(ctx context.Context, orgID, memberID string) (role string, found bool, err error)
}

// WithMemberRoles attache la source des rôles des membres enregistrés en
// ligne. Sans elle, seuls les principals de la configuration ont des
// droits — le comportement historique.
func (a *Authorizer) WithMemberRoles(source MemberRoleSource) *Authorizer {
	a.members = source
	return a
}

// effectivePermissions résout les permissions du principal : la
// configuration d'abord (rôles déclarés, source de vérité historique),
// puis les rôles web des membres enregistrés en base. Un membre rattaché
// en ligne n'a pas de rôle configurable : son jeu de permissions découle
// de son rôle produit (voir identity.DynamicRolePermissions).
func (a *Authorizer) effectivePermissions(ctx context.Context, execIdentity model.ExecutionIdentity) (map[string]struct{}, error) {
	permissions, role, err := a.basePermissions(ctx, execIdentity)
	if err != nil {
		return nil, err
	}

	return a.withPluginDomains(ctx, execIdentity, permissions, role), nil
}

// basePermissions résout le jeu de base et, pour un membre en ligne, son
// rôle produit ("" pour un principal de la configuration).
func (a *Authorizer) basePermissions(ctx context.Context, execIdentity model.ExecutionIdentity) (map[string]struct{}, string, error) {
	permissions, cfgErr := identity.EffectivePermissions(a.cfg, execIdentity.OrgID, execIdentity.PrincipalID)
	if cfgErr == nil {
		return permissions, "", nil
	}

	if a.members == nil {
		return nil, "", cfgErr
	}

	role, found, err := a.members.MemberRole(ctx, string(execIdentity.OrgID), string(execIdentity.PrincipalID))
	if err != nil {
		return nil, "", fmt.Errorf("recherche du rôle du membre %q: %w", execIdentity.PrincipalID, err)
	}
	if !found {
		return nil, "", cfgErr
	}

	return identity.DynamicRolePermissions(role), role, nil
}

// PluginDomainSource fournit les domaines de permission des plugins actifs
// pour une organisation. Implémentée dans internal/registry sur le
// gestionnaire de plugins ; nil quand le système de plugins est désactivé.
type PluginDomainSource interface {
	ActiveDomains(ctx context.Context, orgID string) []string
}

// WithPluginDomains attache la source des domaines de plugins actifs.
func (a *Authorizer) WithPluginDomains(source PluginDomainSource) *Authorizer {
	a.pluginDomains = source
	return a
}

// withPluginDomains étend le jeu de base avec les permissions des domaines
// de plugins ACTIFS pour l'organisation : activer un plugin, décision
// d'administration par organisation, accorde à ses membres l'usage du
// domaine. La vraie porte des écritures reste la confirmation humaine —
// toute écriture de plugin passe par un plan d'actions, et l'activation
// est elle-même re-vérifiée par l'exécuteur au moment de la confirmation.
// Un rôle readonly ne gagne que la lecture, comme partout ailleurs.
func (a *Authorizer) withPluginDomains(ctx context.Context, execIdentity model.ExecutionIdentity, base map[string]struct{}, role string) map[string]struct{} {
	if a.pluginDomains == nil {
		return base
	}

	domains := a.pluginDomains.ActiveDomains(ctx, string(execIdentity.OrgID))
	if len(domains) == 0 {
		return base
	}

	permissions := make(map[string]struct{}, len(base)+len(domains)*5)
	for p := range base {
		permissions[p] = struct{}{}
	}

	for _, domain := range domains {
		if domain == "" {
			continue
		}
		grants := []string{domain + ".personal.read", domain + ".group.read"}
		if role != "readonly" {
			grants = append(grants,
				domain+".personal.write", domain+".personal.delete",
				domain+".group.write")
		}
		for _, g := range grants {
			permissions[g] = struct{}{}
		}
	}

	return permissions
}

type permissionParts struct {
	domain string
	scope  string
	action string
}

func permissionScopeAction(permission string) (permissionParts, error) {
	parts := strings.Split(permission, ".")
	if len(parts) != 3 {
		return permissionParts{}, fmt.Errorf("format de permission invalide %q (attendu <domaine>.<scope>.<action>)", permission)
	}

	return permissionParts{domain: parts[0], scope: parts[1], action: parts[2]}, nil
}

// checkInvariantRules applique les règles fondamentales de cloisonnement,
// invariantes, non désactivables par la configuration.
func checkInvariantRules(req AuthorizationRequest, action string) error {
	switch req.Identity.Trigger {
	case model.TriggerCron:
		return checkCronRules(req)
	case model.TriggerScheduledTask:
		// Volontairement les règles du canal, pas celles du cron : une tâche
		// planifiée conversationnellement porte l'identité de l'humain qui
		// l'a demandée et retourne dans sa conversation. Lui refuser ses
		// propres données ici alors qu'il y a droit en direct n'ajouterait
		// aucune sécurité — la même question posée deux minutes plus tôt
		// aurait obtenu la réponse.
		switch req.Identity.ChannelKind {
		case model.ChannelPrivate:
			return checkPrivateRules(req, action)
		case model.ChannelGroup:
			return checkGroupRules(req)
		}
	default:
		switch req.Identity.ChannelKind {
		case model.ChannelPrivate:
			return checkPrivateRules(req, action)
		case model.ChannelGroup:
			return checkGroupRules(req)
		}
	}

	return fmt.Errorf("authorization: contexte d'identité invalide: %w", apperr.ErrUnauthorized)
}

func checkPrivateRules(req AuthorizationRequest, action string) error {
	switch req.TargetScope {
	case model.ScopePersonal:
		if req.TargetScopeID != model.ScopeID(req.Identity.PrincipalID) {
			return fmt.Errorf("authorization: conversation privée: accès aux ressources personnelles d'un autre principal refusé: %w", apperr.ErrUnauthorized)
		}
	case model.ScopeGroup:
		return fmt.Errorf("authorization: conversation privée: accès aux ressources de groupe refusé: %w", apperr.ErrUnauthorized)
	case model.ScopeOrg:
		if action == "write" || action == "delete" {
			return fmt.Errorf("authorization: conversation privée: écriture ou suppression sur la portée org refusée: %w", apperr.ErrUnauthorized)
		}
	}

	return nil
}

func checkGroupRules(req AuthorizationRequest) error {
	switch req.TargetScope {
	case model.ScopePersonal:
		return fmt.Errorf("authorization: conversation de groupe: accès aux ressources personnelles refusé: %w", apperr.ErrUnauthorized)
	case model.ScopeGroup:
		if req.TargetScopeID != req.Identity.ScopeID {
			return fmt.Errorf("authorization: conversation de groupe: accès à un autre groupe refusé: %w", apperr.ErrUnauthorized)
		}
	case model.ScopeOrg:
		// Selon permission uniquement, déjà vérifié à l'étape précédente.
	}

	return nil
}

func checkCronRules(req AuthorizationRequest) error {
	switch req.TargetScope {
	case model.ScopePersonal:
		return fmt.Errorf("authorization: tâche planifiée: accès aux ressources personnelles refusé par défaut en V1: %w", apperr.ErrUnauthorized)
	case model.ScopeGroup:
		if req.TargetScopeID != req.Identity.ScopeID {
			return fmt.Errorf("authorization: tâche planifiée: accès à un groupe autre que celui configuré refusé: %w", apperr.ErrUnauthorized)
		}
	case model.ScopeOrg:
		// Selon permission uniquement, déjà vérifié à l'étape précédente.
	}

	return nil
}
