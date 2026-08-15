// Package resource résout les identifiants de ressources externes (agenda,
// tâches, …) déclarés dans la configuration pour une portée donnée
// (PLAN.md §9.2, Phase 13).
//
// Aucun identifiant de ressource externe n'est jamais accepté depuis un
// contenu fourni par le modèle (AGENTS.md : "ne pas exposer les
// identifiants de ressources externes aux modèles") : la résolution part
// exclusivement de la portée de la conversation courante (model.Scope,
// model.ScopeID), déjà déterminée par l'application (voir
// internal/identity), et de la configuration chargée au démarrage.
package resource

import (
	"fmt"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

// ErrCalendarNotConfigured est retournée par ResolveCalendarID lorsqu'aucun
// canal de la configuration ne déclare de ressource "calendar" pour la
// portée demandée.
type ErrCalendarNotConfigured struct {
	Scope   model.Scope
	ScopeID model.ScopeID
}

func (e *ErrCalendarNotConfigured) Error() string {
	return fmt.Sprintf("resource: agenda non configuré pour cette portée (scope=%s, scope_id=%s)", e.Scope, e.ScopeID)
}

// ResolveCalendarID retourne l'identifiant de calendrier réel associé à
// (scope, scopeID), tel que déclaré par un canal de cfg.Channels dont
// Scope/ScopeID correspondent et dont Resources["calendar"] est non vide.
//
// Il n'existe pas de structure de ressources au niveau organisation dans le
// schéma de configuration actuel (config.Config n'a pas de champ dédié) :
// pour une portée "org", ResolveCalendarID s'appuie sur le même mécanisme
// générique — il faut qu'un canal de la configuration ait scope: org et
// scope_id égal à l'identifiant d'organisation, et déclare
// resources.calendar, exactement comme pour une portée personal ou group.
// C'est une contrainte de configuration, pas un cas particulier de code.
//
// Si plusieurs canaux correspondent à (scope, scopeID) avec des valeurs de
// resources.calendar différentes, le premier rencontré dans cfg.Channels
// l'emporte (config.Validate ne garantit pas l'unicité d'un couple
// scope/scope_id parmi les canaux d'un même provider) : ce comportement
// n'est pas spécifié plus précisément par PLAN.md et n'a pas besoin de
// l'être pour cette phase, une configuration réelle ne devrait de toute
// façon jamais déclarer deux ressources différentes pour la même portée.
//
// Aucune valeur par défaut n'est jamais inventée : si rien ne correspond,
// une erreur claire (*ErrCalendarNotConfigured) est retournée.
func ResolveCalendarID(cfg *config.Config, scope model.Scope, scopeID model.ScopeID) (string, error) {
	if cfg == nil {
		return "", &ErrCalendarNotConfigured{Scope: scope, ScopeID: scopeID}
	}

	for _, ch := range cfg.Channels {
		if string(ch.Scope) != string(scope) {
			continue
		}
		if ch.ScopeID != string(scopeID) {
			continue
		}

		calendarID := ch.Resources["calendar"]
		if calendarID == "" {
			continue
		}

		return calendarID, nil
	}

	return "", &ErrCalendarNotConfigured{Scope: scope, ScopeID: scopeID}
}
