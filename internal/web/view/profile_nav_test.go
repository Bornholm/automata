package view_test

import (
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/web/view"
)

// Les plugins activés pour l'organisation sont des onglets du profil, au
// même rang que les pages fixes. Le nombre d'onglets n'est pas borné :
// c'est pourquoi la navigation se déplie au lieu de s'étaler en rangée.
func TestProfileNav_ListsPluginsAsTabs(t *testing.T) {
	plugins := []view.ProfilePluginUI{
		{Name: "email", Title: "Courriel", Src: "/p/abc/plugins/email/ui/"},
		{Name: "agenda", Title: "Agenda", Src: "/p/abc/plugins/agenda/ui/"},
	}

	tabs := view.ProfileTabs("abc", "plugin:agenda", plugins)
	if len(tabs) != 6 {
		t.Fatalf("quatre pages fixes et deux plugins attendus, %d onglets obtenus", len(tabs))
	}

	agenda := tabs[len(tabs)-1]
	if agenda.Label != "Agenda" || agenda.Href != "/p/abc/plugins/agenda" {
		t.Errorf("onglet de plugin mal formé : %+v", agenda)
	}
	if !agenda.Active {
		t.Error("l'onglet du plugin affiché n'est pas marqué courant")
	}

	var active int
	for _, tab := range tabs {
		if tab.Active {
			active++
		}
	}
	if active != 1 {
		t.Errorf("%d onglets courants, un seul attendu", active)
	}
}

// La navigation apparaît sur toutes les pages du profil, plugins compris :
// sans cela, ouvrir un onglet de plugin serait un cul-de-sac.
func TestProfilePages_AllCarryTheNavigation(t *testing.T) {
	plugins := []view.ProfilePluginUI{{Name: "email", Title: "Courriel", Src: "/p/abc/plugins/email/ui/"}}

	rendered := map[string]string{
		"profil":  render_(t, view.ProfileHome(view.ProfilePage{LinkID: "abc", PluginUIs: plugins})),
		"crédits": render_(t, view.ProfileCredits(view.CreditsPage{LinkID: "abc", PluginUIs: plugins})),
		"usage":   render_(t, view.ProfileUsage(view.ProfileUsagePage{LinkID: "abc", PluginUIs: plugins})),
		"données": render_(t, view.ProfilePrivacy(view.PrivacyPage{LinkID: "abc", PluginUIs: plugins})),
		"plugin":  render_(t, view.ProfilePlugin(view.ProfilePluginPage{LinkID: "abc", Current: plugins[0], PluginUIs: plugins})),
	}

	for name, html := range rendered {
		if !strings.Contains(html, "/p/abc/plugins/email") {
			t.Errorf("page %s : l'onglet du plugin est absent de la navigation", name)
		}
		if !strings.Contains(html, "<summary") {
			t.Errorf("page %s : la navigation dépliante a disparu", name)
		}
	}
}
