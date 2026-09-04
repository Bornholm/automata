package profile

import (
	"testing"

	"github.com/bornholm/automata/internal/i18n"
)

// La page « Découvrir » compose ses clés (« discover. » + nom + « .title »).
// Le test de complétude du paquet i18n ne relève que les clés littérales :
// il ne peut rien dire de celles-ci. Sans ce test, ajouter une capacité et
// oublier une de ses trois clés afficherait « discover.pages.detail » en
// clair sur la page, dans les trois langues.
func TestDiscoverCapabilityKeysExist(t *testing.T) {
	names := []string{"remember", "remind", "files", "locker", "search", "email", "calendar", "pages"}

	for _, name := range names {
		for _, suffix := range []string{".title", ".detail", ".example"} {
			key := "discover." + name + suffix
			if !i18n.Has(key) {
				t.Errorf("clé %q absente du catalogue", key)
			}
		}
	}
}

// Et l'inverse : une capacité retirée du code laisserait ses clés dans les
// trois catalogues, où plus personne ne saurait dire si elles servent.
func TestDiscoverCapabilities_UseEveryDeclaredKey(t *testing.T) {
	declared := map[string]bool{}
	for _, capability := range discoverCapabilities(t.Context(), []string{"email", "caldav", "pages"}) {
		declared[capability.Title] = true
	}

	if len(declared) != 8 {
		t.Errorf("%d capacités distinctes, 8 attendues", len(declared))
	}
}
