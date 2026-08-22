package view_test

import (
	"context"
	"strings"
	"testing"

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
		// « md:contents » efface le menu en tant que boîte sur écran large :
		// ses entrées redeviennent la colonne de navigation.
		"md:contents",
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

	// La déconnexion doit rester atteignable depuis un téléphone : elle vit
	// dans le menu, pas dans un pied masqué sous le point de rupture.
	logout := strings.Index(html, "/admin/logout")
	if logout < 0 {
		t.Fatal("le bouton de déconnexion a disparu du gabarit")
	}
	if closing := strings.Index(html, "</details>"); closing >= 0 && closing < logout {
		t.Error("la déconnexion est hors du menu : inatteignable sur téléphone")
	}
}
