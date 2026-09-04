package view_test

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/bornholm/automata/internal/i18n"
	"github.com/bornholm/automata/internal/web/view"
)

// renderIn rend un composant dans la langue donnée.
func renderIn(t *testing.T, locale i18n.Locale, component templ.Component) string {
	t.Helper()

	var out strings.Builder
	if err := component.Render(i18n.WithLocale(context.Background(), locale), &out); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	return out.String()
}

// Les pages du profil suivent la langue du membre. Le test rend chacune
// dans les trois langues et vérifie qu'elle n'a rien gardé du français —
// une chaîne oubliée en dur passe tous les autres tests, y compris celui
// de complétude des catalogues, et ne se voit qu'à l'écran.
func TestProfilePages_FollowTheLocale(t *testing.T) {
	cases := map[string]struct {
		component templ.Component
		// french est une phrase de la page, en français : elle doit
		// disparaître des rendus anglais et espagnol.
		french string
	}{
		"profil": {
			component: view.ProfileHome(view.ProfilePage{LinkID: "abc", EmailState: "absent", Locale: "fr"}),
			french:    "Votre adresse de secours",
		},
		"découvrir": {
			component: view.DiscoverHome(view.DiscoverPage{LinkID: "abc"}),
			french:    "Ce que vous pouvez me demander",
		},
		"souvenirs": {
			component: view.ProfileMemories(view.MemoriesPage{LinkID: "abc"}),
			french:    "Ce que je retiens de vous",
		},
		"dossiers": {
			component: view.ProfileMissions(view.MissionsPage{LinkID: "abc"}),
			french:    "Mes dossiers",
		},
		"suggestions": {
			component: view.ProfileSuggestions(view.SuggestionsPage{LinkID: "abc"}),
			french:    "Mes suggestions",
		},
		"confidentialité": {
			component: view.ProfilePrivacy(view.PrivacyPage{LinkID: "abc"}),
			french:    "Supprimer définitivement",
		},
		"lien expiré": {
			component: view.ProfileLinkState(view.LinkStatePage{State: "expired"}),
			french:    "Ce lien a expiré",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := renderIn(t, i18n.FR, tc.component); !strings.Contains(got, tc.french) {
				t.Fatalf("le rendu français ne contient pas %q — le test ne prouve plus rien", tc.french)
			}
			for _, locale := range []i18n.Locale{i18n.EN, i18n.ES} {
				if got := renderIn(t, locale, tc.component); strings.Contains(got, tc.french) {
					t.Errorf("%s: %q est resté en français", locale, tc.french)
				}
			}
		})
	}
}

// Les noms de langue du sélecteur s'écrivent DANS leur langue. Quelqu'un
// dont la page s'est ouverte dans une langue qu'il ne lit pas doit pouvoir
// retrouver la sienne : « Español » se reconnaît sur une page anglaise,
// « Spanish » ne se reconnaît pas sur une page française.
func TestProfileLanguageSelector_NamesLanguagesInThemselves(t *testing.T) {
	page := view.ProfilePage{LinkID: "abc", EmailState: "absent", Locale: "es"}

	for _, locale := range i18n.Supported {
		got := renderIn(t, locale, view.ProfileHome(page))
		for _, name := range []string{"Français", "English", "Español"} {
			if !strings.Contains(got, name) {
				t.Errorf("%s: le sélecteur ne propose pas %q", locale, name)
			}
		}
		// La langue en cours est présélectionnée, sinon un enregistrement
		// sans y toucher la remettrait au premier choix de la liste.
		if !strings.Contains(got, `<option value="es" selected>`) {
			t.Errorf("%s: la langue courante n'est pas présélectionnée", locale)
		}
	}
}
