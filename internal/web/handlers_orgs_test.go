package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/persistence"
)

// Deux organisations de même nom sont indiscernables dans toutes les
// listes — celle de l'administration comme le sélecteur des jetons de
// liaison. Une création refaite doit donc être refusée, pas suffixée en
// silence.
func TestOrgCreateRefusesDuplicateName(t *testing.T) {
	_, ts, client := testServer(t)
	login(t, ts, client)

	create := func() *http.Response {
		if _, err := client.Get(ts.URL + "/admin/orgs/new"); err != nil {
			t.Fatalf("GET création: %v", err)
		}
		resp, err := client.PostForm(ts.URL+"/admin/orgs", url.Values{
			"display_name": {"atelier"},
			"csrf_token":   {csrfFrom(t, client, ts.URL)},
		})
		if err != nil {
			t.Fatalf("POST création: %v", err)
		}
		return resp
	}

	first := create()
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("première création: statut %d", first.StatusCode)
	}

	second := create()
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("seconde création: statut %d, attendu %d", second.StatusCode, http.StatusConflict)
	}

	body, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatalf("lecture de la réponse: %v", err)
	}
	if !strings.Contains(string(body), "existe déjà") {
		t.Error("le refus devrait nommer la cause")
	}
}

// La liste des organisations compte les canaux rattachés en ligne, pas
// seulement ceux déclarés en configuration : sans cela, une organisation
// dont toutes les conversations sont liées s'affichait « Aucun canal
// lié », en contradiction avec l'écran des canaux.
func TestOrgSubtitleCountsBoundChannels(t *testing.T) {
	server, _, _ := testServer(t)

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
