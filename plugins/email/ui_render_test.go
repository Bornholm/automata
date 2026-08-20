package main

import (
	"strings"
	"testing"
)

// Le gabarit membre a deux visages : compte Google connecté (aucun champ
// serveur, aucun mot de passe) ou configuration manuelle. Une régression
// de branche donnerait un écran vide, pas une erreur de compilation.
func TestMemberTemplate_RendersBothModes(t *testing.T) {
	cases := []struct {
		name     string
		data     uiData
		want     []string
		unwanted []string
	}{
		{
			name: "google connecté",
			data: uiData{
				Base: "/base/", MemberScoped: true, GoogleAvailable: true, GoogleConnected: true,
				Cfg: memberConfig{Username: "cam@gmail.test", AuthMode: authModeOAuth, AllowRead: true},
			},
			want:     []string{"Compte Google connecté", "cam@gmail.test", "Déconnecter", "peut lire mes courriels"},
			unwanted: []string{"Serveur IMAP", "Tester la connexion"},
		},
		{
			name: "manuel avec Google proposé",
			data: uiData{Base: "/base/", MemberScoped: true, GoogleAvailable: true},
			want: []string{"Connecter Gmail", "Serveur IMAP", "Tester la connexion"},
		},
		{
			name:     "manuel sans application Google",
			data:     uiData{Base: "/base/", MemberScoped: true},
			want:     []string{"Serveur IMAP"},
			unwanted: []string{"Connecter Gmail"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			if err := uiTemplate.Execute(&sb, tc.data); err != nil {
				t.Fatalf("rendu: %v", err)
			}
			html := sb.String()
			for _, needle := range tc.want {
				if !strings.Contains(html, needle) {
					t.Errorf("écran sans %q", needle)
				}
			}
			for _, needle := range tc.unwanted {
				if strings.Contains(html, needle) {
					t.Errorf("écran avec %q alors qu'il ne devrait pas", needle)
				}
			}
		})
	}
}

// L'écran d'administration montre l'URI de redirection à déclarer chez
// Google et ne révèle jamais le secret client.
func TestAdminTemplate_ShowsRedirectURIWithoutSecret(t *testing.T) {
	var sb strings.Builder
	err := adminTemplate.Execute(&sb, adminData{
		Base:        "/admin/base/",
		RedirectURI: "https://automata.test/plugins/email/oauth/callback",
		App:         oauthApp{ClientID: "abc.apps.googleusercontent.com", ClientSecret: "SECRET-CLIENT"},
		HasSecret:   true,
	})
	if err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	if !strings.Contains(html, "https://automata.test/plugins/email/oauth/callback") {
		t.Error("URI de redirection absente")
	}
	if strings.Contains(html, "SECRET-CLIENT") {
		t.Error("le secret client apparaît dans la page")
	}
}
