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
	"github.com/bornholm/automata/internal/privacy"
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
			"display_name": {"Atelier"},
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

// La suppression d'une organisation emporte tout ce qui n'existe que par
// elle, canaux et membres compris ; le nom se retape pour confirmer.
func TestOrgDelete(t *testing.T) {
	server, ts, client := testServer(t)
	server.WithPrivacy(privacy.New(server.DB, nil, nil))
	login(t, ts, client)

	if _, err := client.Get(ts.URL + "/admin/orgs/new"); err != nil {
		t.Fatalf("GET création: %v", err)
	}
	resp, err := client.PostForm(ts.URL+"/admin/orgs", url.Values{
		"display_name": {"Doublon"},
		"csrf_token":   {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST création: %v", err)
	}
	resp.Body.Close()
	orgID := path.Base(resp.Request.URL.Path)

	now := server.Now()
	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := server.Members.Insert(context.Background(), tx, persistence.Member{
			ID: "m-test", OrgID: orgID, DisplayName: "Témoin",
			Role: persistence.MemberRoleMember, CreatedAt: now, UpdatedAt: now,
		}, false); err != nil {
			return err
		}
		return server.Bindings.Upsert(context.Background(), tx, persistence.ChannelBinding{
			Provider: "whatsapp", ChannelID: "120000000000000001@g.us", OrgID: orgID,
			Kind: "group", Scope: "group", DisplayName: "Canal du doublon", CreatedAt: now,
		})
	}); err != nil {
		t.Fatalf("préparation: %v", err)
	}

	deleteOrg := func(typed string) {
		t.Helper()
		resp, err := client.PostForm(ts.URL+"/admin/orgs/"+orgID+"/delete", url.Values{
			"confirm_name": {typed},
			"csrf_token":   {csrfFrom(t, client, ts.URL)},
		})
		if err != nil {
			t.Fatalf("POST suppression: %v", err)
		}
		resp.Body.Close()
	}

	// Un nom mal retapé ne supprime rien.
	deleteOrg("Autre chose")
	if !orgExists(t, server, orgID) {
		t.Fatal("l'organisation a été supprimée malgré un nom erroné")
	}

	// Le bon nom emporte l'organisation, ses membres et ses canaux.
	deleteOrg("doublon")
	if orgExists(t, server, orgID) {
		t.Error("l'organisation aurait dû être supprimée")
	}

	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		members, err := server.Members.ListByOrg(context.Background(), tx, orgID)
		if err != nil {
			return err
		}
		if len(members) != 0 {
			t.Errorf("%d membre(s) survivent à l'organisation", len(members))
		}

		bound, err := server.Bindings.ListByOrg(context.Background(), tx, orgID)
		if err != nil {
			return err
		}
		if len(bound) != 0 {
			t.Errorf("%d canal(aux) survivent à l'organisation", len(bound))
		}

		return nil
	}); err != nil {
		t.Fatalf("vérification: %v", err)
	}
}

// orgExists dit si l'organisation est toujours en base.
func orgExists(t *testing.T, server *Server, orgID string) bool {
	t.Helper()

	var found bool
	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		_, found, err = server.Orgs.FindByID(context.Background(), tx, orgID)
		return err
	}); err != nil {
		t.Fatalf("lecture de l'organisation: %v", err)
	}

	return found
}

// La fiche d'une organisation compte et liste ses canaux rattachés en
// ligne : sans eux, une organisation dont toutes les conversations sont
// liées par jeton s'affichait « Aucun canal rattaché » — et son compteur
// à zéro — alors que l'écran des canaux en listait plusieurs.
func TestOrgPageShowsBoundChannels(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)

	now := server.Now()
	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		if err := server.Orgs.Insert(context.Background(), tx, persistence.Organization{
			ID: "atelier", DisplayName: "Atelier", CreatedAt: now, UpdatedAt: now,
		}, false); err != nil {
			return err
		}
		return server.Bindings.Upsert(context.Background(), tx, persistence.ChannelBinding{
			Provider: "whatsapp", ChannelID: "120000000000000001@g.us", OrgID: "atelier",
			Kind: "group", Scope: "group", DisplayName: "Atelier IA", CreatedAt: now,
		})
	}); err != nil {
		t.Fatalf("préparation: %v", err)
	}

	resp, err := client.Get(ts.URL + "/admin/orgs/atelier?tab=channels")
	if err != nil {
		t.Fatalf("GET fiche: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("lecture: %v", err)
	}
	if !strings.Contains(string(body), "Atelier IA") {
		t.Error("le canal rattaché devrait figurer dans l'onglet Canaux")
	}
	if strings.Contains(string(body), "Aucun canal rattaché") {
		t.Error("l'onglet se dit vide alors qu'un canal est lié")
	}
}

// Détacher un canal le retire de l'organisation sans toucher à la
// conversation, et ne peut pas atteindre le canal d'une autre organisation
// depuis cette fiche. Jusqu'ici la seule issue pour un groupe rattaché au
// mauvais foyer était SQL sur le volume.
func TestOrgChannelUnbind(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)

	now := server.Now()
	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		for _, org := range []string{"atelier", "voisins"} {
			if err := server.Orgs.Insert(context.Background(), tx, persistence.Organization{
				ID: org, DisplayName: org, CreatedAt: now, UpdatedAt: now,
			}, false); err != nil {
				return err
			}
		}
		for _, b := range []persistence.ChannelBinding{
			{Provider: "whatsapp", ChannelID: "120000000000000001@g.us", OrgID: "atelier", Kind: "group", Scope: "group", ScopeID: "120000000000000001@g.us", DisplayName: "Atelier IA", CreatedAt: now},
			{Provider: "whatsapp", ChannelID: "120000000000000002@g.us", OrgID: "voisins", Kind: "group", Scope: "group", ScopeID: "120000000000000002@g.us", DisplayName: "Voisins", CreatedAt: now},
		} {
			if err := server.Bindings.Upsert(context.Background(), tx, b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("préparation: %v", err)
	}

	// Depuis la fiche d'atelier, tenter de détacher le canal des voisins.
	resp, err := client.PostForm(ts.URL+"/admin/orgs/atelier/channels/unbind", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
		"provider":   {"whatsapp"},
		"channel_id": {"120000000000000002@g.us"},
	})
	if err != nil {
		t.Fatalf("POST unbind étranger: %v", err)
	}
	resp.Body.Close()

	// Puis le sien.
	resp, err = client.PostForm(ts.URL+"/admin/orgs/atelier/channels/unbind", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
		"provider":   {"whatsapp"},
		"channel_id": {"120000000000000001@g.us"},
	})
	if err != nil {
		t.Fatalf("POST unbind: %v", err)
	}
	resp.Body.Close()

	var all []persistence.ChannelBinding
	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		all, err = server.Bindings.ListAll(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatalf("relecture: %v", err)
	}
	if len(all) != 1 || all[0].OrgID != "voisins" {
		t.Fatalf("liaisons restantes = %+v : seul le canal des voisins devait survivre", all)
	}

	// La fiche ne liste plus le canal détaché.
	resp, err = client.Get(ts.URL + "/admin/orgs/atelier?tab=channels")
	if err != nil {
		t.Fatalf("GET fiche: %v", err)
	}
	if html := body(t, resp); strings.Contains(html, "Atelier IA") {
		t.Error("le canal détaché figure encore dans l'onglet Canaux")
	}
}
