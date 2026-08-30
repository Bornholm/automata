package web

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/model"
	"github.com/bornholm/automata/internal/persistence"
)

// fakeMemory tient lieu de mémoire, avec le cloisonnement par portée que le
// store réel applique — et, comme lui, un Forget qui n'en applique AUCUN.
// C'est cette asymétrie que les tests ci-dessous surveillent.
type fakeMemory struct {
	memories []memory.Memory
	forgot   []string
}

func (m *fakeMemory) ListByScope(_ context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID) ([]memory.Memory, error) {
	var scoped []memory.Memory
	for _, mem := range m.memories {
		if mem.Metadata["org_id"] == string(orgID) &&
			mem.Metadata["scope"] == string(scope) &&
			mem.Metadata["scope_id"] == string(scopeID) {
			scoped = append(scoped, mem)
		}
	}
	return scoped, nil
}

func (m *fakeMemory) GetByID(ctx context.Context, orgID model.OrgID, scope model.Scope, scopeID model.ScopeID, id string) (memory.Memory, bool, error) {
	scoped, _ := m.ListByScope(ctx, orgID, scope, scopeID)
	for _, mem := range scoped {
		if mem.ID == id {
			return mem, true, nil
		}
	}
	return memory.Memory{}, false, nil
}

func (m *fakeMemory) Remember(_ context.Context, mem memory.NewMemory) (memory.Memory, error) {
	created := mem.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	stored := memory.Memory{
		ID:        "mem-" + mem.Content,
		Content:   mem.Content,
		CreatedAt: created,
		Metadata: map[string]string{
			"org_id":   string(mem.OrgID),
			"scope":    string(mem.Scope),
			"scope_id": string(mem.ScopeID),
			"origin":   mem.Origin,
		},
	}
	m.memories = append(m.memories, stored)
	return stored, nil
}

// Forget efface par identifiant, sans le moindre contrôle de portée : c'est
// exactement ce que fait le store réel, et la raison pour laquelle les
// handlers doivent vérifier la portée avant d'appeler.
func (m *fakeMemory) Forget(_ context.Context, id string) error {
	m.forgot = append(m.forgot, id)
	for i, mem := range m.memories {
		if mem.ID == id {
			m.memories = append(m.memories[:i], m.memories[i+1:]...)
			return nil
		}
	}
	return nil
}

func personalMemory(id, orgID, memberID, content, origin string) memory.Memory {
	return memory.Memory{
		ID:        id,
		Content:   content,
		CreatedAt: time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"org_id":   orgID,
			"scope":    string(model.ScopePersonal),
			"scope_id": memberID,
			"origin":   origin,
		},
	}
}

// openMemories ouvre une session de profil et rend le client prêt à agir.
func openMemories(t *testing.T, server *Server, ts *httptest.Server, memberID string) (*http.Client, string) {
	t.Helper()

	path := createProfileLink(t, server, memberID, 15*time.Minute)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp := openProfileLink(t, ts, client, path)
	resp.Body.Close()

	return client, strings.TrimPrefix(path, "/p/")
}

// La page montre les souvenirs personnels du membre, et rien d'autre.
func TestMemories_ShowsOwnPersonalMemoriesOnly(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})
	seedMember(t, server, persistence.Member{ID: "lina", OrgID: "org-a", DisplayName: "Lina", Role: "member"})

	mem := &fakeMemory{memories: []memory.Memory{
		personalMemory("m1", "org-a", "cam", "Camille aime le thé", ""),
		personalMemory("m2", "org-a", "lina", "Lina gère la compta", ""),
		{ID: "m3", Content: "le groupe se réunit le mardi", Metadata: map[string]string{
			"org_id": "org-a", "scope": string(model.ScopeGroup), "scope_id": "atelier",
		}},
	}}
	server.WithMemory(mem)

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.Get(ts.URL + "/p/" + link + "/memories")
	if err != nil {
		t.Fatalf("GET souvenirs: %v", err)
	}
	html := body(t, resp)

	if !strings.Contains(html, "Camille aime le thé") {
		t.Error("le souvenir du membre devrait s'afficher")
	}
	if strings.Contains(html, "Lina gère la compta") {
		t.Error("FUITE : le souvenir d'un autre membre est affiché")
	}
	if strings.Contains(html, "le groupe se réunit le mardi") {
		t.Error("FUITE : un souvenir de groupe est affiché sur la page personnelle")
	}
}

// LE test qui compte : Forget n'applique aucun cloisonnement, c'est donc au
// handler de refuser. Sans cette vérification, un identifiant recopié
// effacerait le souvenir de quelqu'un d'autre.
func TestMemories_CannotDeleteSomeoneElsesMemory(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})
	seedMember(t, server, persistence.Member{ID: "lina", OrgID: "org-a", DisplayName: "Lina", Role: "member"})

	mem := &fakeMemory{memories: []memory.Memory{
		personalMemory("m1", "org-a", "cam", "Camille aime le thé", ""),
		personalMemory("m2", "org-a", "lina", "Lina gère la compta", ""),
	}}
	server.WithMemory(mem)

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.PostForm(ts.URL+"/p/"+link+"/memories/m2/delete", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST suppression: %v", err)
	}
	resp.Body.Close()

	if len(mem.forgot) != 0 {
		t.Fatalf("Forget appelé sur %v : le souvenir d'un autre membre a été effacé", mem.forgot)
	}
	if _, found, _ := mem.GetByID(context.Background(), "org-a", model.ScopePersonal, "lina", "m2"); !found {
		t.Error("le souvenir de l'autre membre a disparu")
	}
}

// Effacer son propre souvenir fonctionne.
func TestMemories_DeleteOwnMemory(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	mem := &fakeMemory{memories: []memory.Memory{
		personalMemory("m1", "org-a", "cam", "Camille aime le thé", ""),
	}}
	server.WithMemory(mem)

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.PostForm(ts.URL+"/p/"+link+"/memories/m1/delete", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST suppression: %v", err)
	}
	resp.Body.Close()

	if len(mem.memories) != 0 {
		t.Errorf("le souvenir devrait avoir disparu, reste %d", len(mem.memories))
	}
}

// Corriger un souvenir préserve sa date d'acquisition : réécrire le texte ne
// doit pas laisser croire qu'Automata vient de l'apprendre.
func TestMemories_EditKeepsTheOriginalDate(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	original := personalMemory("m1", "org-a", "cam", "Camille aime le thé", "onboarding")
	mem := &fakeMemory{memories: []memory.Memory{original}}
	server.WithMemory(mem)

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.PostForm(ts.URL+"/p/"+link+"/memories/m1", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
		"content":    {"Camille préfère le café"},
	})
	if err != nil {
		t.Fatalf("POST correction: %v", err)
	}
	resp.Body.Close()

	if len(mem.memories) != 1 {
		t.Fatalf("un seul souvenir attendu après correction, %d obtenus", len(mem.memories))
	}
	updated := mem.memories[0]
	if updated.Content != "Camille préfère le café" {
		t.Errorf("contenu = %q, attendu la correction", updated.Content)
	}
	if !updated.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("date = %s, attendu la date d'origine %s", updated.CreatedAt, original.CreatedAt)
	}
	if updated.Metadata["origin"] != "onboarding" {
		t.Errorf("origine = %q, attendu qu'elle soit préservée", updated.Metadata["origin"])
	}
}

// Une correction sans jeton CSRF ne passe pas : la page est ouverte par un
// lien qui circule sur une messagerie.
func TestMemories_EditRequiresCSRF(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	mem := &fakeMemory{memories: []memory.Memory{
		personalMemory("m1", "org-a", "cam", "Camille aime le thé", ""),
	}}
	server.WithMemory(mem)

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.PostForm(ts.URL+"/p/"+link+"/memories/m1", url.Values{
		"content": {"n'importe quoi"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("statut = %d, attendu 403", resp.StatusCode)
	}
	if mem.memories[0].Content != "Camille aime le thé" {
		t.Error("le souvenir a été modifié sans jeton CSRF")
	}
}
