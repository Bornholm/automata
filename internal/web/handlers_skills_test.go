package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/skills"
)

// createSkill poste le formulaire de création et rend la réponse.
func createSkill(t *testing.T, ts *httptest.Server, client *http.Client, values url.Values) *http.Response {
	t.Helper()

	if _, err := client.Get(ts.URL + "/admin/skills/new"); err != nil {
		t.Fatalf("GET création: %v", err)
	}
	values.Set("csrf_token", csrfFrom(t, client, ts.URL))

	resp, err := client.PostForm(ts.URL+"/admin/skills", values)
	if err != nil {
		t.Fatalf("POST création: %v", err)
	}
	return resp
}

// Cycle complet : créer, relire, éditer, désactiver, supprimer.
func TestSkillCRUD(t *testing.T) {
	_, ts, client := testServer(t)
	login(t, ts, client)

	resp := createSkill(t, ts, client, url.Values{
		"name":        {"mask-a-logo"},
		"description": {"Mask a logo on a photo"},
		"content":     {"Locate the logo, blur the region."},
		"agents":      {"workspace"},
		"enabled":     {"1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("création: statut %d", resp.StatusCode)
	}

	list, err := client.Get(ts.URL + "/admin/skills")
	if err != nil {
		t.Fatalf("GET liste: %v", err)
	}
	if page := body(t, list); !strings.Contains(page, "mask-a-logo") {
		t.Error("la compétence créée devrait apparaître dans la liste")
	}

	form, err := client.Get(ts.URL + "/admin/skills/mask-a-logo")
	if err != nil {
		t.Fatalf("GET fiche: %v", err)
	}
	page := body(t, form)
	if !strings.Contains(page, "Locate the logo") {
		t.Error("le contenu devrait être rechargé dans le formulaire")
	}
	// Une compétence écrite à la main n'a aucune version d'origine.
	if strings.Contains(page, "Restaurer la version d'origine") {
		t.Error("une compétence non fournie ne devrait pas proposer la restauration")
	}

	update, err := client.PostForm(ts.URL+"/admin/skills/mask-a-logo", url.Values{
		"description": {"Mask a logo, carefully"},
		"content":     {"Blur the region in one command."},
		"agents":      {"workspace, research"},
		"csrf_token":  {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST édition: %v", err)
	}
	update.Body.Close()

	form, err = client.Get(ts.URL + "/admin/skills/mask-a-logo")
	if err != nil {
		t.Fatalf("GET fiche après édition: %v", err)
	}
	page = body(t, form)
	if !strings.Contains(page, "Blur the region in one command.") {
		t.Error("le contenu édité devrait être enregistré")
	}
	if !strings.Contains(page, "Mask a logo, carefully") {
		t.Error("la description éditée devrait être enregistrée")
	}

	// La case « active » absente du formulaire désactive la compétence :
	// le catalogue des agents s'en trouve allégé au message suivant.
	list, err = client.Get(ts.URL + "/admin/skills")
	if err != nil {
		t.Fatalf("GET liste après désactivation: %v", err)
	}
	if page := body(t, list); !strings.Contains(page, "Inactive") {
		t.Error("la compétence devrait être signalée inactive")
	}

	del, err := client.PostForm(ts.URL+"/admin/skills/mask-a-logo/delete", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST suppression: %v", err)
	}
	del.Body.Close()

	list, err = client.Get(ts.URL + "/admin/skills")
	if err != nil {
		t.Fatalf("GET liste après suppression: %v", err)
	}
	if page := body(t, list); strings.Contains(page, "mask-a-logo") {
		t.Error("la compétence supprimée ne devrait plus apparaître")
	}
}

// Le nom est une clé : un doublon est refusé plutôt que d'écraser en
// silence le travail de quelqu'un d'autre.
func TestSkillCreateRefusesDuplicateName(t *testing.T) {
	_, ts, client := testServer(t)
	login(t, ts, client)

	values := url.Values{
		"name":        {"a-skill"},
		"description": {"Something"},
		"content":     {"Do it."},
		"enabled":     {"1"},
	}

	first := createSkill(t, ts, client, values)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("première création: statut %d", first.StatusCode)
	}

	second := createSkill(t, ts, client, values)
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("seconde création: statut %d, attendu %d", second.StatusCode, http.StatusConflict)
	}
}

// Un nom hors kebab-case est refusé : il voyage jusque dans les arguments
// d'outil du modèle.
func TestSkillCreateRefusesInvalidName(t *testing.T) {
	_, ts, client := testServer(t)
	login(t, ts, client)

	resp := createSkill(t, ts, client, url.Values{
		"name":        {"Mask A Logo"},
		"description": {"Something"},
		"content":     {"Do it."},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("statut %d, attendu %d", resp.StatusCode, http.StatusBadRequest)
	}
	if !strings.Contains(body(t, resp), "kebab-case") {
		t.Error("le refus devrait nommer la cause")
	}
}

// Une compétence fournie par le projet se restaure depuis sa version
// embarquée, même après avoir été éditée.
func TestSkillRestoreBuiltin(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)

	// Le semis est le geste du démarrage (internal/registry) : on le
	// rejoue ici pour disposer d'une compétence fournie par le projet.
	if err := skills.Seed(context.Background(), server.db, nil); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	edit, err := client.PostForm(ts.URL+"/admin/skills/mask-logo-in-image", url.Values{
		"description": {"Edited"},
		"content":     {"Edited content."},
		"enabled":     {"1"},
		"csrf_token":  {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST édition: %v", err)
	}
	edit.Body.Close()

	restore, err := client.PostForm(ts.URL+"/admin/skills/mask-logo-in-image/restore", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST restauration: %v", err)
	}
	restore.Body.Close()

	form, err := client.Get(ts.URL + "/admin/skills/mask-logo-in-image")
	if err != nil {
		t.Fatalf("GET fiche: %v", err)
	}
	page := body(t, form)
	if strings.Contains(page, "Edited content.") {
		t.Error("la restauration devrait écraser le contenu édité")
	}
	if !strings.Contains(page, "Restaurer la version d&#39;origine") && !strings.Contains(page, "Restaurer la version d'origine") {
		t.Error("une compétence fournie devrait proposer la restauration")
	}
}
