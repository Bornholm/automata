// Package resource résout les identifiants de ressources externes déclarés
// dans la configuration pour une portée donnée (plan de conception, §9.2).
//
// Le package ne connaît aucun service par son nom. Un serveur MCP déclare
// quelle clé de ressource il consomme et sous quel paramètre il l'attend
// (config.MCPServer.Resource) ; les canaux déclarent la valeur de cette clé
// pour leur portée (channels[].resources). Brancher un nouveau domaine ne
// demande donc aucune modification ici.
//
// Aucun identifiant de ressource n'est jamais accepté depuis un contenu
// fourni par le modèle (AGENTS.md) : la résolution part exclusivement de la
// portée de la conversation, déjà déterminée par l'application (voir
// internal/identity), et de la configuration chargée au démarrage.
package resource

import (
	"fmt"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/model"
)

// ErrNotConfigured est retournée lorsqu'aucun canal ne déclare la ressource
// demandée pour la portée visée.
type ErrNotConfigured struct {
	Key     string
	Scope   model.Scope
	ScopeID model.ScopeID
}

func (e *ErrNotConfigured) Error() string {
	return fmt.Sprintf("resource: ressource %q non configurée pour cette portée (scope=%s, scope_id=%s)", e.Key, e.Scope, e.ScopeID)
}

// Resolve retourne l'identifiant réel de la ressource key pour la portée
// (scope, scopeID), tel que déclaré par un canal de cfg.Channels dont
// Scope/ScopeID correspondent et dont Resources[key] est non vide.
//
// La portée "org" n'a pas de structure dédiée dans le schéma de
// configuration : il faut qu'un canal déclare scope: org et un scope_id égal
// à l'identifiant d'organisation, exactement comme pour une portée personal
// ou group. C'est une contrainte de configuration, pas un cas particulier de
// code.
//
// Si plusieurs canaux correspondent avec des valeurs différentes, le premier
// rencontré l'emporte. Une configuration réelle ne devrait jamais déclarer
// deux ressources différentes pour la même portée.
//
// Aucune valeur par défaut n'est inventée : sans correspondance, une erreur
// claire (*ErrNotConfigured) est retournée.
func Resolve(cfg *config.Config, key string, scope model.Scope, scopeID model.ScopeID) (string, error) {
	if cfg == nil || key == "" {
		return "", &ErrNotConfigured{Key: key, Scope: scope, ScopeID: scopeID}
	}

	for _, ch := range cfg.Channels {
		if string(ch.Scope) != string(scope) || ch.ScopeID != string(scopeID) {
			continue
		}

		if id := ch.Resources[key]; id != "" {
			return id, nil
		}
	}

	return "", &ErrNotConfigured{Key: key, Scope: scope, ScopeID: scopeID}
}

// InjectResolved retourne args augmenté de l'identifiant de ressource attendu
// par le serveur MCP serverName, en écrasant toute valeur préexistante. Un
// serveur qui ne déclare aucune ressource laisse args inchangé.
//
// Appelée avant chaque appel d'outil, et à nouveau juste avant l'exécution
// d'une action confirmée : entre la proposition et la confirmation, la
// configuration a pu changer, et c'est l'identifiant courant de la portée qui
// fait foi, jamais celui qu'aurait pu contenir l'action persistée (le plan de conception
// §10.5 point 6).
func InjectResolved(cfg *config.Config, serverName string, scope model.Scope, scopeID model.ScopeID, args map[string]any) (map[string]any, error) {
	if cfg == nil {
		return args, nil
	}

	server, ok := cfg.MCPServers[serverName]
	if !ok || server.Resource == nil {
		return args, nil
	}

	value, err := Resolve(cfg, server.Resource.Key, scope, scopeID)
	if err != nil {
		return nil, err
	}

	if args == nil {
		args = make(map[string]any, 1)
	}
	args[server.Resource.Parameter] = value

	return args, nil
}
