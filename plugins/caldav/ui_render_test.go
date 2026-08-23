package main

import (
	"strings"
	"testing"
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
