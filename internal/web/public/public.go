// Package public sert les pages accessibles sans session : les sites
// publiés par les plugins (/s/), les aperçus de brouillon (/d/) et les
// liens de téléchargement de fichiers (/f/).
//
// Ces trois routes partagent une règle : rien n'y distingue un jeton
// invalide, expiré, ou visant un plugin désactivé — toutes rendent 404,
// pour ne renseigner personne sur ce qui existe.
package public

import "github.com/bornholm/automata/internal/web/core"

// Handlers sert les pages publiques.
type Handlers struct {
	*core.Deps
}

// New construit les handlers publics.
func New(deps *core.Deps) *Handlers {
	return &Handlers{Deps: deps}
}
