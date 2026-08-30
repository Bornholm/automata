// Package profile sert les pages qu'un membre ouvre depuis sa
// conversation : son profil, ses crédits, son usage, ses données
// personnelles, et le retour de paiement.
//
// L'accès ne passe jamais par un mot de passe : un lien temporaire à usage
// unique ouvre une session courte (voir resolveProfile). Le webhook Stripe
// vit ici aussi — c'est le pendant serveur du paiement lancé depuis ces
// pages.
package profile

import "github.com/bornholm/automata/internal/web/core"

// Handlers sert les pages de profil.
type Handlers struct {
	*core.Deps
}

// New construit les handlers de profil.
func New(deps *core.Deps) *Handlers {
	return &Handlers{Deps: deps}
}
