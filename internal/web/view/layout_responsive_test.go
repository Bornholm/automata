package view_test

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/bornholm/automata/internal/web/view"
)

// render rend un composant en chaîne, pour inspecter le HTML produit.
func render(t *testing.T, page string) string {
	t.Helper()

	var b strings.Builder
	component := view.AdminLayout("Test", "dashboard",
		[]view.SidebarPlatform{{Name: "WhatsApp", Type: "whatsapp"}}, "jeton")

	if err := component.Render(context.Background(), &b); err != nil {
		t.Fatalf("rendu de %s: %v", page, err)
	}

	return b.String()
}

// L'administration se consulte aussi depuis un téléphone. La barre de
// navigation y occupe toute la largeur en bandeau horizontal ; elle ne
// reprend sa colonne de 216 px qu'à partir de la tablette. Sans ces
// classes, il resterait moins de 160 px au contenu sur un écran courant.
func TestAdminLayout_NavigationAdaptsToSmallScreens(t *testing.T) {
	html := render(t, "AdminLayout")

	for _, want := range []string{
		// Colonne latérale seulement à partir de md.
		"md:w-[216px]",
		// Empilement vertical par défaut, côte à côte ensuite.
		"flex-col md:flex-row",
		// Menu escamotable natif : le navigateur l'ouvre et le ferme seul,
		// sans JavaScript.
		"<details",
		"<summary",
		// Le bouton n'existe que sur téléphone.
		"md:hidden",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("le gabarit d'administration a perdu son adaptation mobile : %q absent", want)
		}
	}

	// Une largeur fixe inconditionnelle rendrait la page inutilisable sur
	// téléphone : c'est la régression que ce test surveille.
	if strings.Contains(html, `class="flex w-[216px]`) {
		t.Error("la barre latérale impose une largeur fixe sans point de rupture")
	}

	// La déconnexion doit rester atteignable depuis un téléphone : le bloc
	// qui la porte est masqué par défaut, et révélé par l'ouverture du
	// menu. Sans cette classe, il resterait invisible sur petit écran.
	logout := strings.Index(html, "/admin/logout")
	if logout < 0 {
		t.Fatal("le bouton de déconnexion a disparu du gabarit")
	}
	if !strings.Contains(html[:logout], "group-has-[details[open]]:flex") {
		t.Error("la déconnexion n'est pas révélée par le menu : inatteignable sur téléphone")
	}
}

// Régression du 2026-08-23 : la navigation avait disparu sur grand écran.
// Elle vivait DANS le <details>, rendu visible par un « md:contents » —
// mais les navigateurs masquent désormais le contenu d'un <details> fermé
// quelle que soit sa valeur de display (::details-content). La navigation
// doit donc vivre hors du <details>, qui ne porte plus que le bouton.
func TestAdminLayout_NavigationStaysVisibleOnDesktop(t *testing.T) {
	html := render(t, "AdminLayout")

	closing := strings.Index(html, "</details>")
	if closing < 0 {
		t.Fatal("le menu escamotable a disparu du gabarit")
	}

	if nav := strings.Index(html, "<nav"); nav < closing {
		t.Error("la navigation est enfermée dans le <details> : escamotée sur grand écran")
	}
	if logout := strings.Index(html, "/admin/logout"); logout < closing {
		t.Error("le pied de la barre est enfermé dans le <details> : escamoté sur grand écran")
	}

	// Et elle s'affiche inconditionnellement à partir du point de rupture.
	if !strings.Contains(html, "group-has-[details[open]]:flex md:mt-0 md:flex") {
		t.Error("la navigation n'est pas dépliée d'office sur grand écran")
	}
}

// Un écran dont un bloc déborde doit le faire défiler dans son propre
// conteneur. Sans cette ceinture sur le contenu, la page entière glisse
// latéralement : l'en-tête et le titre sortent du cadre, et plus rien n'est
// lisible au doigt.
func TestAdminLayout_ContentNeverScrollsThePage(t *testing.T) {
	html := render(t, "AdminLayout")

	if !strings.Contains(html, "overflow-x-hidden") {
		t.Error("le contenu peut déborder et décaler la page entière")
	}
}

// render_ rend un composant quelconque en chaîne.
func render_(t *testing.T, component templ.Component) string {
	t.Helper()

	var b strings.Builder
	if err := component.Render(context.Background(), &b); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	return b.String()
}
