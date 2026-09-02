package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/memory"
	"github.com/bornholm/automata/internal/persistence"
)

// TestRenderProfileSnapshot écrit le HTML d'une page de profil sur disque
// quand AUTOMATA_SNAPSHOT_DIR est renseigné, pour la capturer dans un
// navigateur. Ce n'est pas une assertion : c'est l'outil qui permet de
// regarder une page avant de la livrer. Ignoré par défaut.
func TestRenderProfileSnapshot(t *testing.T) {
	dir := os.Getenv("AUTOMATA_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("AUTOMATA_SNAPSHOT_DIR non renseigné")
	}

	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Familia Petit"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})
	server.WithMemory(&fakeMemory{memories: []memory.Memory{
		personalMemory("m1", "org-a", "cam", "Préfère les réponses courtes, sauf pour les recettes.", "onboarding"),
		personalMemory("m2", "org-a", "cam", "Semble planifier ses courses le samedi matin.", "episode_reflection"),
	}})
	if err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewSuggestionRepository().Insert(context.Background(), tx, persistence.Suggestion{
			ID: "s1", OrgID: "org-a", MemberID: "cam", Kind: "automation",
			Title: "Programmer la liste de courses", Body: "Vous la demandez chaque samedi. Je peux vous l'envoyer automatiquement le vendredi soir.",
			Status: persistence.SuggestionStatusProposed, CreatedAt: time.Now(),
		})
	}); err != nil {
		t.Fatal(err)
	}

	path := createProfileLink(t, server, "cam", 15*time.Minute)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp := openProfileLink(t, ts, client, path)
	resp.Body.Close()
	link := strings.TrimPrefix(path, "/p/")

	for _, page := range []string{"", "/memories", "/suggestions", "/discover", "/credits"} {
		resp, err := client.Get(ts.URL + "/p/" + link + page)
		if err != nil {
			t.Fatal(err)
		}
		html := body(t, resp)
		name := strings.Trim(page, "/")
		if name == "" {
			name = "profile"
		}
		if err := os.WriteFile(dir+"/"+name+".html", []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
