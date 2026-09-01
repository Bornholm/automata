package web

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

func seedSuggestion(t *testing.T, server *Server, id, memberID string) {
	t.Helper()

	err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewSuggestionRepository().Insert(context.Background(), tx, persistence.Suggestion{
			ID: id, OrgID: "org-a", MemberID: memberID,
			Kind: "automation", Title: "Programmer la météo",
			Body: "Vous la demandez chaque vendredi.", Status: persistence.SuggestionStatusProposed,
			CreatedAt: time.Now().UTC(),
		})
	})
	if err != nil {
		t.Fatalf("semis de la suggestion: %v", err)
	}
}

// La page affiche les suggestions du membre, l'écartement est définitif, et
// une suggestion d'un autre membre reste hors d'atteinte.
func TestSuggestions_PageAndScoping(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})
	seedMember(t, server, persistence.Member{ID: "lina", OrgID: "org-a", DisplayName: "Lina", Role: "member"})
	seedSuggestion(t, server, "sug-cam", "cam")
	seedSuggestion(t, server, "sug-lina", "lina")

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.Get(ts.URL + "/p/" + link + "/suggestions")
	if err != nil {
		t.Fatalf("GET suggestions: %v", err)
	}
	html := body(t, resp)
	if !strings.Contains(html, "Programmer la météo") {
		t.Error("la suggestion du membre devrait s'afficher")
	}

	// Écarter la suggestion d'un AUTRE membre ne fait rien.
	resp, err = client.PostForm(ts.URL+"/p/"+link+"/suggestions/sug-lina/dismiss", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST dismiss étranger: %v", err)
	}
	resp.Body.Close()

	var lina []persistence.Suggestion
	err = server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		lina, err = persistence.NewSuggestionRepository().ListByMember(context.Background(), tx, "org-a", "lina", 10)
		return err
	})
	if err != nil {
		t.Fatalf("relecture: %v", err)
	}
	if lina[0].Status != persistence.SuggestionStatusProposed {
		t.Error("la suggestion d'un autre membre a été modifiée : cloisonnement rompu")
	}

	// La sienne, si.
	resp, err = client.PostForm(ts.URL+"/p/"+link+"/suggestions/sug-cam/dismiss", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST dismiss: %v", err)
	}
	resp.Body.Close()

	var cam []persistence.Suggestion
	err = server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		cam, err = persistence.NewSuggestionRepository().ListByMember(context.Background(), tx, "org-a", "cam", 10)
		return err
	})
	if err != nil {
		t.Fatalf("relecture: %v", err)
	}
	if cam[0].Status != persistence.SuggestionStatusDismissed {
		t.Errorf("statut = %q, attendu dismissed", cam[0].Status)
	}
}

// « Ne plus rien me proposer » persiste sur le membre — c'est ce que
// l'introspection consulte avant toute collecte.
func TestSuggestions_MuteIsPersisted(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.PostForm(ts.URL+"/p/"+link+"/suggestions/mute", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
		"muted":      {"1"},
	})
	if err != nil {
		t.Fatalf("POST mute: %v", err)
	}
	resp.Body.Close()

	var member persistence.Member
	err = server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		member, _, err = server.Members.FindByID(context.Background(), tx, "cam")
		return err
	})
	if err != nil {
		t.Fatalf("relecture: %v", err)
	}
	if !member.SuggestionsMuted {
		t.Error("le mute devrait être persisté sur le membre")
	}
}
