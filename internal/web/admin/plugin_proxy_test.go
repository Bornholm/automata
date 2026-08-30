package admin

import "testing"

// L'exemption CSRF doit être exactement aussi large que nécessaire : les
// chemins d'UI de plugin, et rien d'autre.
func TestIsPluginUIPath(t *testing.T) {
	exempt := []string{
		"/plugin-ui/jeton/",
		"/plugin-ui/jeton/save",
		"/plugin-ui/jeton/oauth/callback",
	}
	for _, path := range exempt {
		if !isPluginUIPath(path) {
			t.Errorf("%q devrait être exempté", path)
		}
	}

	protected := []string{
		"/admin/orgs/org-a/plugins",
		"/admin/orgs/org-a/grant",
		"/admin/plugins/email/restart",
		"/admin/orgs",
		"/admin/pricing/settings",
		"/plugin-ui/jeton/../../admin/grant",
		// Un jeton sans rien derrière ne désigne aucune interface.
		"/plugin-ui/jeton",
		"/plugin-ui//save",
	}
	for _, path := range protected {
		if isPluginUIPath(path) {
			t.Errorf("%q ne doit PAS être exempté du contrôle CSRF", path)
		}
	}
}
