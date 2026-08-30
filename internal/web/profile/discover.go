package profile

import (
	"database/sql"
	"net/http"
	"slices"

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
	member, minutes, ok := h.resolveProfile(w, r)
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
		Capabilities: discoverCapabilities(enabled),
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
func discoverCapabilities(enabledPlugins []string) []view.DiscoverCapability {
	capabilities := []view.DiscoverCapability{
		{
			Title:   "Retenir ce qui compte",
			Detail:  "Dites-moi ce que je dois garder en tête, et je m'en souviendrai dans nos prochaines conversations.",
			Example: "Retiens que mon associée s'appelle Lina et qu'elle gère la comptabilité.",
		},
		{
			Title:   "Vous rappeler quelque chose",
			Detail:  "Une échéance, un rendez-vous, une habitude : je vous préviens au bon moment.",
			Example: "Rappelle-moi vendredi matin d'envoyer la facture à Lina.",
		},
		{
			Title:   "Travailler sur vos fichiers",
			Detail:  "Envoyez-moi un document, une image ou une vidéo : je peux les convertir, les découper, en extraire ce qu'il vous faut.",
			Example: "Envoyez une photo, puis : « recadre-la en carré et allège-la ».",
		},
		{
			Title:   "Garder vos documents",
			Detail:  "Ce que je garde pour vous reste disponible des mois plus tard, contrairement aux fichiers de passage.",
			Example: "Garde ce contrat, j'en aurai besoin le mois prochain.",
		},
		{
			Title:   "Chercher pour vous",
			Detail:  "Une question dont la réponse est sur le web, et je vous rapporte l'essentiel plutôt qu'une liste de liens.",
			Example: "Quels sont les horaires de la déchetterie de Meylan ?",
		},
	}

	// Les plugins ajoutent leurs propres verbes. Chacun n'apparaît que s'il
	// est activé pour l'organisation.
	if slices.Contains(enabledPlugins, "email") {
		capabilities = append(capabilities, view.DiscoverCapability{
			Title:   "Lire et écrire vos courriels",
			Detail:  "Une fois votre boîte reliée, je peux la relever et rédiger vos réponses — vous les validez avant tout envoi.",
			Example: "Qu'est-ce que j'ai reçu d'important aujourd'hui ?",
		})
	}
	if slices.Contains(enabledPlugins, "caldav") {
		capabilities = append(capabilities, view.DiscoverCapability{
			Title:   "Tenir votre agenda",
			Detail:  "Je consulte vos disponibilités et je place vos rendez-vous, après confirmation.",
			Example: "Suis-je libre jeudi après-midi ?",
		})
	}
	if slices.Contains(enabledPlugins, "pages") {
		capabilities = append(capabilities, view.DiscoverCapability{
			Title:   "Publier une page",
			Detail:  "Un compte rendu, une affiche, un formulaire : je les mets en ligne et vous donne le lien.",
			Example: "Fais-moi une page avec le programme de la fête de quartier.",
		})
	}

	return capabilities
}
