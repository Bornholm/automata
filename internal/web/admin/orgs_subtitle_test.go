package admin

import (
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
)

// La liste des organisations compte les canaux rattachés en ligne, pas
// seulement ceux déclarés en configuration : sans cela, une organisation
// dont toutes les conversations sont liées s'affichait « Aucun canal
// lié », en contradiction avec l'écran des canaux.
func TestOrgSubtitleCountsBoundChannels(t *testing.T) {
	server := &Handlers{Deps: &core.Deps{Cfg: &config.Config{}}}

	subtitle := server.orgSubtitle("atelier", nil)
	if subtitle != "Aucun canal lié" {
		t.Errorf("sans canal : %q", subtitle)
	}

	bound := []persistence.ChannelBinding{
		{Provider: "whatsapp", ChannelID: "120000000000000001@g.us", OrgID: "atelier"},
		{Provider: "whatsapp", ChannelID: "autre@g.us", OrgID: "autre-org"},
	}
	subtitle = server.orgSubtitle("atelier", bound)
	if !strings.Contains(subtitle, "1 canal") {
		t.Errorf("avec un canal rattaché : %q", subtitle)
	}
}
