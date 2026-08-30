// Package admin sert les écrans d'exploitation, sous /admin : tableau de
// bord, organisations et membres, comptes de messagerie, modèles,
// compétences, plugins, tarifs et consommation.
//
// Tout y passe par RequireAdmin — session d'opérateur et jeton CSRF — sauf
// le reverse proxy des interfaces de plugins, qui porte sa propre
// authentification par jeton signé (voir plugin_proxy.go).
package admin

import "github.com/bornholm/automata/internal/web/core"

// Handlers sert les écrans d'administration.
type Handlers struct {
	*core.Deps
}

// New construit les handlers d'administration.
func New(deps *core.Deps) *Handlers {
	return &Handlers{Deps: deps}
}
