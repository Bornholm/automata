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
	cfg *config.Config
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

	permissions, err := identity.EffectivePermissions(a.cfg, req.Identity.OrgID, req.Identity.PrincipalID)
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
