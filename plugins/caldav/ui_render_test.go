package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// Le mot de passe n'est JAMAIS relu vers l'interface : seul un booléen
// « défini » traverse. Une régression ici exposerait le mot de passe de
// l'agenda dans le HTML d'une iframe.
func TestMemberTemplate_NeverEchoesThePassword(t *testing.T) {
	var sb strings.Builder
	err := uiTemplate.Execute(&sb, uiData{
		Base:         "/base/",
		MemberScoped: true,
		HasPassword:  true,
		Cfg: memberConfig{
			ServerURL: "https://exemple.fr/dav",
			Username:  "cam@exemple.fr",
			AllowRead: true,
		},
	})
	if err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	if !strings.Contains(html, "Laissez vide pour le conserver") {
		t.Error("l'écran ne dit pas que le mot de passe est déjà défini")
	}
	// Le champ mot de passe ne doit porter aucune valeur.
	if strings.Contains(html, `type="password" name="password" value=`) {
		t.Error("le champ mot de passe porte une valeur")
	}
}

// Confier ses rappels à un agenda distant déplace leur texte hors de la
// base chiffrée d'Automata. Ce n'est pas un détail de réglage : l'écran
// doit le dire avant que la case soit cochée, pas après.
func TestMemberTemplate_WarnsBeforeDelegatingTheReminders(t *testing.T) {
	var sb strings.Builder
	if err := uiTemplate.Execute(&sb, uiData{Base: "/base/", MemberScoped: true}); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	if !strings.Contains(html, "Ranger mes rappels dans cet agenda") {
		t.Fatal("la case de reprise du magasin est absente")
	}
	for _, needle := range []string{"chez votre fournisseur d'agenda", "en clair", "chiffré"} {
		if !strings.Contains(html, needle) {
			t.Errorf("l'avertissement sur le chiffrement ne mentionne pas %q", needle)
		}
	}
}

// Rien ne part dans l'agenda sans accord : l'écran doit le dire, parce que
// les interrupteurs de lecture et d'écriture se lisent facilement comme
// des permissions d'agir seul.
func TestMemberTemplate_StatesTheConfirmationRule(t *testing.T) {
	var sb strings.Builder
	if err := uiTemplate.Execute(&sb, uiData{Base: "/base/", MemberScoped: true}); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	if !strings.Contains(sb.String(), "sans votre accord") {
		t.Error("la règle de confirmation n'est pas rappelée")
	}
}

// Vu depuis l'administration, il n'y a rien à régler : l'agenda et les
// identifiants appartiennent à la personne.
func TestAdminView_OffersNothingToConfigure(t *testing.T) {
	var sb strings.Builder
	if err := uiTemplate.Execute(&sb, uiData{Base: "/base/"}); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	if strings.Contains(html, `name="password"`) {
		t.Error("un champ mot de passe est proposé hors du contexte d'un membre")
	}
	if !strings.Contains(html, "profil de chaque personne") {
		t.Error("l'écran d'administration n'explique pas où se configure le plugin")
	}
}

// Après un échec de certificat, l'écran montre CE QUE le serveur présente
// — sujet, émetteur, empreinte — pour que la personne compare avant
// d'accepter. Un bouton « accepter » sans rien à comparer ne serait pas
// une décision.
func TestMemberTemplate_ShowsTheCertificateBeforeAcceptingIt(t *testing.T) {
	info := pluginsdk.CertificateInfo{
		Subject:     "CN=agenda.exemple.fr",
		Issuer:      "CN=agenda.exemple.fr",
		SelfSigned:  true,
		Fingerprint: strings.Repeat("ab", 32),
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
		CertFingerprint: info.FormattedFingerprint(),
		CertExpiry:      "02/01/2027 03:04",
		Cfg:             memberConfig{ServerURL: "https://agenda.exemple.fr/dav"},
	})
	if err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	for _, needle := range []string{
		"agenda.exemple.fr",
		"auto-signé",
		info.FormattedFingerprint(),
		"Accepter ce certificat",
		"certificate signed by unknown authority",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("l'écran ne montre pas %q", needle)
		}
	}

	// La portée de l'exception est dite : c'est ce qui distingue un
	// épinglage d'un « ne rien vérifier ».
	if !strings.Contains(html, "un autre restera refusé") {
		t.Error("l'écran ne dit pas que l'exception ne vaut que pour ce certificat")
	}
}

// L'exception enregistrée se voit, et se retire.
func TestMemberTemplate_ShowsAndRemovesAnExistingException(t *testing.T) {
	var sb strings.Builder
	err := uiTemplate.Execute(&sb, uiData{
		Base:         "/base/",
		MemberScoped: true,
		HasException: true,
		Cfg:          memberConfig{ServerURL: "https://agenda.exemple.fr/dav"},
	})
	if err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	if !strings.Contains(html, "exception de certificat est enregistrée") {
		t.Error("l'exception en vigueur n'est pas signalée")
	}
	if !strings.Contains(html, "Retirer l'exception") {
		t.Error("aucun moyen de retirer l'exception")
	}
}
