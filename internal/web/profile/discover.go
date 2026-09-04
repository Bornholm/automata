package profile

import (
	"context"
	"database/sql"
	"net/http"
	"slices"

	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/onboarding"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// La page « Découvrir » : le pendant écrit de la visite d'accueil.
//
// Les deux répondent à la même question — « qu'est-ce que je peux te
// demander ? » —, mais pas au même moment. La visite arrive quand la
// personne vient d'être rattachée ; cette page reste, pour celle qui a
// écarté la visite, ou qui revient trois semaines plus tard sans savoir par
// où reprendre.
//
// Ce qui y est listé suit ce que l'organisation a RÉELLEMENT activé : une
// capacité annoncée mais absente se solde par un « je n'ai pas d'outil pour
// ça » quelques secondes après, et fait douter de tout le reste.

// HandleProfileDiscover rend la page de découverte.
func (h *Handlers) HandleProfileDiscover(w http.ResponseWriter, r *http.Request) {
	member, r, minutes, ok := h.resolveProfile(w, r)
	if !ok {
		return
	}

	plugins, ok := h.profilePluginUIs(w, r, member)
	if !ok {
		return
	}

	var enabled []string
	if ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		enabled, err = h.PluginActivations.EnabledPlugins(r.Context(), tx, member.OrgID)
		return err
	}); !ok {
		return
	}

	h.Render(w, r, http.StatusOK, view.DiscoverHome(view.DiscoverPage{
		LinkID:       r.PathValue("link"),
		Header:       h.profileHeader(r, member, minutes),
		PluginUIs:    plugins,
		Capabilities: discoverCapabilities(r.Context(), enabled),
		VisitState:   visitState(member),
	}))
}

// visitState traduit l'état persisté pour la vue. Tout ce qui n'est ni
// terminé, ni écarté, ni jamais proposé est une visite en cours.
func visitState(member persistence.Member) string {
	switch member.OnboardingState {
	case onboarding.StateNone:
		return ""
	case onboarding.StateDone:
		return "done"
	case onboarding.StateSkipped:
		return "skipped"
	default:
		return "running"
	}
}

// discoverCapabilities compose la liste, en commençant par ce dont tout le
// monde dispose. Les exemples sont écrits comme on parle, pas comme on
// documente : ce sont des phrases à recopier.
func discoverCapabilities(ctx context.Context, enabledPlugins []string) []view.DiscoverCapability {
	capability := func(name string) view.DiscoverCapability {
		return view.DiscoverCapability{
			Title:   i18n.TC(ctx, "discover."+name+".title"),
			Detail:  i18n.TC(ctx, "discover."+name+".detail"),
			Example: i18n.TC(ctx, "discover."+name+".example"),
		}
	}

	capabilities := []view.DiscoverCapability{
		capability("remember"),
		capability("remind"),
		capability("files"),
		capability("locker"),
		capability("search"),
	}

	// Les plugins ajoutent leurs propres verbes. Chacun n'apparaît que s'il
	// est activé pour l'organisation.
	if slices.Contains(enabledPlugins, "email") {
		capabilities = append(capabilities, capability("email"))
	}
	if slices.Contains(enabledPlugins, "caldav") {
		capabilities = append(capabilities, capability("calendar"))
	}
	if slices.Contains(enabledPlugins, "pages") {
		capabilities = append(capabilities, capability("pages"))
	}

	return capabilities
}
