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

// Noms des serveurs MCP porteurs d'une ressource résolue par l'application,
// et nom du paramètre sous lequel chacun l'attend.
//
// Ces conventions sont partagées par le spécialiste qui propose l'action
// (internal/agent) et par le moteur qui l'exécute après confirmation
// (internal/action) : c'est ce qui permet à l'identifiant d'être RETIRÉ des
// arguments au moment de la proposition, puis résolu à nouveau juste avant
// l'exécution réelle (PLAN.md §9.2 et §10.5 point 6). Elles vivent ici, au
// point de résolution, plutôt que d'être dupliquées de part et d'autre.
//
// Aucun serveur MCP réel n'étant disponible dans ce dépôt, ces noms sont
// ceux des serveurs fakes des tests ; ils doivent correspondre aux clés
// déclarées sous mcp_servers dans la configuration.
const (
	CalendarMCPServerName = "google-calendar"
	CalendarIDParam       = "calendar_id"

	TodoMCPServerName = "todo"
	TodoListIDParam   = "list_id"
)

// InjectResolved retourne args augmenté de l'identifiant de ressource attendu
// par mcpServer pour la portée (scope, scopeID), en écrasant toute valeur
// préexistante. Un serveur qui ne porte aucune ressource résolue par
// l'application laisse args inchangé.
//
// Appelée juste avant l'exécution réelle d'une action confirmée : entre la
// proposition et la confirmation, la configuration a pu changer, et c'est
// l'identifiant courant de la portée du plan qui fait foi — jamais celui
// qu'aurait pu contenir l'action persistée.
func InjectResolved(cfg *config.Config, mcpServer string, scope model.Scope, scopeID model.ScopeID, args map[string]any) (map[string]any, error) {
	var (
		param string
		value string
		err   error
	)

	switch mcpServer {
	case CalendarMCPServerName:
		param = CalendarIDParam
		value, err = ResolveCalendarID(cfg, scope, scopeID)
	case TodoMCPServerName:
		param = TodoListIDParam
		value, err = ResolveTodoListID(cfg, scope, scopeID)
	default:
		return args, nil
	}

	if err != nil {
		return nil, err
	}

	if args == nil {
		args = make(map[string]any, 1)
	}
	args[param] = value

	return args, nil
}

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
	id, ok := resolveResourceID(cfg, scope, scopeID, "calendar")
	if !ok {
		return "", &ErrCalendarNotConfigured{Scope: scope, ScopeID: scopeID}
	}
	return id, nil
}

// ErrTodoListNotConfigured est retournée par ResolveTodoListID lorsqu'aucun
// canal de la configuration ne déclare de ressource "todo" pour la portée
// demandée. Même contrat qu'ErrCalendarNotConfigured (voir ci-dessus).
type ErrTodoListNotConfigured struct {
	Scope   model.Scope
	ScopeID model.ScopeID
}

func (e *ErrTodoListNotConfigured) Error() string {
	return fmt.Sprintf("resource: liste de tâches non configurée pour cette portée (scope=%s, scope_id=%s)", e.Scope, e.ScopeID)
}

// ResolveTodoListID retourne l'identifiant de liste de tâches réel associé à
// (scope, scopeID), sur exactement le même modèle que ResolveCalendarID
// (canal de cfg.Channels dont Scope/ScopeID correspondent et dont
// Resources["todo"] est non vide ; premier canal correspondant retenu ;
// aucune valeur par défaut inventée).
func ResolveTodoListID(cfg *config.Config, scope model.Scope, scopeID model.ScopeID) (string, error) {
	id, ok := resolveResourceID(cfg, scope, scopeID, "todo")
	if !ok {
		return "", &ErrTodoListNotConfigured{Scope: scope, ScopeID: scopeID}
	}
	return id, nil
}

// resolveResourceID factorise la recherche commune à ResolveCalendarID et
// ResolveTodoListID : parcourir cfg.Channels à la recherche du premier canal
// dont (Scope, ScopeID) correspond et dont Resources[key] est non vide.
func resolveResourceID(cfg *config.Config, scope model.Scope, scopeID model.ScopeID, key string) (string, bool) {
	if cfg == nil {
		return "", false
	}

	for _, ch := range cfg.Channels {
		if string(ch.Scope) != string(scope) {
			continue
		}
		if ch.ScopeID != string(scopeID) {
			continue
		}

		id := ch.Resources[key]
		if id == "" {
			continue
		}

		return id, true
	}

	return "", false
}
