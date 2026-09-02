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

func seedWebMission(t *testing.T, server *Server, id, memberID string) {
	t.Helper()

	now := time.Now().UTC()
	err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewMissionRepository(server.DB.Cipher()).Insert(context.Background(), tx, persistence.Mission{
			ID: id, OrgID: "org-a", PrincipalID: memberID,
			ConversationID: "whatsapp:" + memberID, Provider: "whatsapp", ChannelID: memberID,
			AgentID: "main", Title: "Réclamation parking",
			Objective:   "Suivre la réclamation du parking.",
			Journal:     "2026-09-03: relance envoyée.",
			Status:      persistence.MissionStatusActive,
			NextCheckAt: now.AddDate(0, 0, 7), CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("semis de la mission: %v", err)
	}
}

func webMissionStatus(t *testing.T, server *Server, memberID string) string {
	t.Helper()

	var missions []persistence.Mission
	err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		missions, err = persistence.NewMissionRepository(server.DB.Cipher()).
			ListByMember(context.Background(), tx, "org-a", memberID, 10)
		return err
	})
	if err != nil || len(missions) == 0 {
		t.Fatalf("relecture: %v (missions=%d)", err, len(missions))
	}

	return missions[0].Status
}

// La page affiche les dossiers du membre — objectif et journal mot pour
// mot — et l'abandon reste cloisonné : le dossier d'un autre membre est
// hors d'atteinte, même avec son identifiant exact.
func TestMissions_PageAndScoping(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})
	seedMember(t, server, persistence.Member{ID: "lina", OrgID: "org-a", DisplayName: "Lina", Role: "member"})
	seedWebMission(t, server, "mis-cam", "cam")
	seedWebMission(t, server, "mis-lina", "lina")

	client, link := openMemories(t, server, ts, "cam")

	resp, err := client.Get(ts.URL + "/p/" + link + "/missions")
	if err != nil {
		t.Fatalf("GET missions: %v", err)
	}
	html := body(t, resp)
	if !strings.Contains(html, "Réclamation parking") {
		t.Error("le dossier du membre devrait s'afficher")
	}
	if !strings.Contains(html, "relance envoyée") {
		t.Error("le journal de bord devrait se lire mot pour mot")
	}

	// Abandonner le dossier d'un AUTRE membre ne fait rien.
	resp, err = client.PostForm(ts.URL+"/p/"+link+"/missions/mis-lina/abandon", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST abandon étranger: %v", err)
	}
	resp.Body.Close()

	if got := webMissionStatus(t, server, "lina"); got != persistence.MissionStatusActive {
		t.Errorf("statut = %q : le dossier d'un autre membre a été modifié, cloisonnement rompu", got)
	}

	// Le sien, si.
	resp, err = client.PostForm(ts.URL+"/p/"+link+"/missions/mis-cam/abandon", url.Values{
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST abandon: %v", err)
	}
	resp.Body.Close()

	if got := webMissionStatus(t, server, "cam"); got != persistence.MissionStatusAbandoned {
		t.Errorf("statut = %q, attendu abandoned", got)
	}
}
