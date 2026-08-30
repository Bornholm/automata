package conversation

import (
	"context"

	"github.com/bornholm/automata/internal/model"
)

// OnboardingService conduit la visite d'accueil des nouveaux membres.
// Implémenté par internal/onboarding.Service ; l'interface vit ici pour que
// le paquet conversation n'en dépende pas.
//
// handled=true : la visite a répondu, le modèle n'est pas consulté pour ce
// tour. handled=false : le message doit suivre son cours normal — c'est le
// cas quand la personne vient de quitter la visite, et son message ne doit
// surtout pas être perdu au passage.
type OnboardingService interface {
	Handle(ctx context.Context, identity model.ExecutionIdentity, text string) (reply string, handled bool, err error)
}

// WithOnboarding active la visite d'accueil. Sans cet appel, aucun membre ne
// se voit proposer de visite : c'est le comportement des instances qui ne
// gèrent pas de membres.
func (h *Handler) WithOnboarding(service OnboardingService) *Handler {
	h.onboarding = service
	return h
}
