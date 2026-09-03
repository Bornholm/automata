package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
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

// Après un échec de certificat, l'écran montre ce que le serveur présente,
// et dit lequel des deux serveurs est en cause : la messagerie entrante et
// la sortante peuvent être deux machines.
func TestMemberTemplate_ShowsTheCertificateBeforeAcceptingIt(t *testing.T) {
	info := pluginsdk.CertificateInfo{
		Subject:     "CN=imap.exemple.fr",
		Issuer:      "CN=imap.exemple.fr",
		SelfSigned:  true,
		Fingerprint: strings.Repeat("cd", 32),
		VerifyError: "x509: certificate signed by unknown authority",
		NotAfter:    time.Date(2027, 1, 2, 3, 4, 0, 0, time.UTC),
	}

	var sb strings.Builder
	err := uiTemplate.Execute(&sb, uiData{
		Base:            "/base/",
		MemberScoped:    true,
		Tested:          true,
		TestMessage:     "Connexion impossible : x509: certificate signed by unknown authority",
		Cert:            &info,
		CertProtocol:    "imap",
		CertFingerprint: info.FormattedFingerprint(),
		CertExpiry:      "02/01/2027 03:04",
	})
	if err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	for _, needle := range []string{
		"imap.exemple.fr",
		"auto-signé",
		info.FormattedFingerprint(),
		"Accepter ce certificat",
		"certificate signed by unknown authority",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("l'écran ne montre pas %q", needle)
		}
	}
	// Le protocole part avec l'acceptation : sans lui, l'exception se
	// poserait sur le mauvais serveur.
	if !strings.Contains(html, `name="protocol" value="imap"`) {
		t.Error("l'acceptation ne désigne pas le serveur concerné")
	}
}

// Les exceptions en vigueur se voient, et se retirent une par une.
func TestMemberTemplate_ShowsAndRemovesExistingExceptions(t *testing.T) {
	var sb strings.Builder
	err := uiTemplate.Execute(&sb, uiData{
		Base:         "/base/",
		MemberScoped: true,
		Exceptions:   []string{"imap", "smtp"},
	})
	if err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	if !strings.Contains(html, "Exception de certificat enregistrée pour : imap, smtp") {
		t.Errorf("les exceptions en vigueur ne sont pas listées: %s", html)
	}
	if !strings.Contains(html, "Retirer l'exception imap") || !strings.Contains(html, "Retirer l'exception smtp") {
		t.Error("chaque exception doit pouvoir être retirée séparément")
	}
}
