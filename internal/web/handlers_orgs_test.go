package web

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"path"
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

// Une organisation créée en double s'efface ; une organisation qui a
// vécu, non — son historique déborde de cette base (souvenirs, pièces
// jointes, relevés de consommation).
func TestOrgDelete(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)

	createOrg := func(name string) string {
		if _, err := client.Get(ts.URL + "/admin/orgs/new"); err != nil {
			t.Fatalf("GET création: %v", err)
		}
		resp, err := client.PostForm(ts.URL+"/admin/orgs", url.Values{
			"display_name": {name},
			"csrf_token":   {csrfFrom(t, client, ts.URL)},
		})
		if err != nil {
			t.Fatalf("POST création: %v", err)
		}
		defer resp.Body.Close()
		return path.Base(resp.Request.URL.Path)
	}

	deleteOrg := func(orgID, typed string) *http.Response {
		resp, err := client.PostForm(ts.URL+"/admin/orgs/"+orgID+"/delete", url.Values{
			"confirm_name": {typed},
			"csrf_token":   {csrfFrom(t, client, ts.URL)},
		})
		if err != nil {
			t.Fatalf("POST suppression: %v", err)
		}
		return resp
	}

	orgID := createOrg("Doublon")

	// Un nom mal retapé ne supprime rien.
	resp := deleteOrg(orgID, "Autre chose")
	resp.Body.Close()
	if err := server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, found, err := server.orgs.FindByID(context.Background(), tx, orgID)
		if err != nil {
			return err
		}
		if !found {
			t.Error("l'organisation a été supprimée malgré un nom erroné")
		}
		return nil
	}); err != nil {
		t.Fatalf("vérification: %v", err)
	}

	// Un membre rattaché retient la suppression.
	if err := server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return server.members.Insert(context.Background(), tx, persistence.Member{
			ID: "m-test", OrgID: orgID, DisplayName: "Témoin",
			Role: persistence.MemberRoleMember, CreatedAt: server.now(), UpdatedAt: server.now(),
		}, false)
	}); err != nil {
		t.Fatalf("insertion du membre: %v", err)
	}

	resp = deleteOrg(orgID, "Doublon")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "membre rattaché") {
		t.Error("le refus devrait nommer ce qui retient la suppression")
	}

	// Le membre retiré, la suppression aboutit.
	if err := server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `DELETE FROM members WHERE org_id = ?`, orgID)
		return err
	}); err != nil {
		t.Fatalf("retrait du membre: %v", err)
	}

	resp = deleteOrg(orgID, "doublon")
	resp.Body.Close()

	if err := server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, found, err := server.orgs.FindByID(context.Background(), tx, orgID)
		if err != nil {
			return err
		}
		if found {
			t.Error("l'organisation vide aurait dû être supprimée")
		}
		return nil
	}); err != nil {
		t.Fatalf("vérification finale: %v", err)
	}
}
