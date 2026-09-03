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

// Le gabarit ne laisse aucun élément se placer tout seul. Trois défauts
// signalés le 2026-09-03, chacun visible à l'écran :
//
//   - le textarea des consignes n'était visé par aucune règle : il gardait
//     la police à chasse fixe et la largeur par défaut du navigateur, et se
//     posait à côté de son propre intitulé au lieu d'en dessous ;
//   - les boutons portaient une marge haute individuelle, ce qui les
//     décalait de la phrase qu'ils accompagnent et les renvoyait à la
//     ligne — deux exceptions de certificat donnaient deux boutons
//     désalignés ;
//   - aucun bouton n'était groupé, chacun se débrouillant avec une marge
//     en ligne.
func TestMemberTemplate_LaysOutFieldsAndButtons(t *testing.T) {
	var sb strings.Builder
	data := uiData{
		Base: "/base/", MemberScoped: true,
		Cfg:        memberConfig{IMAPHost: "imap.test", SMTPHost: "smtp.test"},
		Exceptions: []string{"imap", "smtp"},
	}
	if err := uiTemplate.Execute(&sb, data); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	html := sb.String()

	// Le textarea est habillé comme les autres champs, et en bloc : sans
	// cela il se range à côté de son intitulé.
	if !strings.Contains(html, ",textarea{display:block") {
		t.Error("le textarea n'est pas habillé comme les autres champs")
	}
	if !strings.Contains(html, "textarea{min-height") {
		t.Error("le textarea n'a pas de hauteur propre")
	}

	// Aucune marge haute sur le bouton lui-même : ce sont les rangées qui
	// espacent.
	if strings.Contains(html, "button{margin-top") {
		t.Error("les boutons portent encore une marge individuelle")
	}
	if !strings.Contains(html, ".actions{display:flex") {
		t.Error("les boutons ne sont pas groupés en rangée")
	}

	// Les deux exceptions et leur phrase tiennent sur une même rangée
	// alignée, qui se replie si la place manque.
	if !strings.Contains(html, `<div class="exceptions">`) {
		t.Error("les boutons d'exception ne sont pas alignés avec leur phrase")
	}
	if strings.Count(html, "Retirer l'exception") != 2 {
		t.Errorf("deux exceptions attendues, obtenu %d", strings.Count(html, "Retirer l'exception"))
	}
	// Plus de style en ligne sur ces boutons : ils prennent la classe
	// commune, comme tous les boutons secondaires.
	if strings.Contains(html, `style="background:#fff;color:#161c27`) {
		t.Error("un bouton porte encore son style en ligne")
	}

	// La colonne est bornée : dans une iframe large, une ligne de texte
	// traverserait l'écran.
	if !strings.Contains(html, "main{max-width") {
		t.Error("la colonne n'est pas bornée en largeur")
	}
	if !strings.Contains(html, "<main>") || !strings.Contains(html, "</main>") {
		t.Error("le contenu n'est pas dans la colonne")
	}
}
