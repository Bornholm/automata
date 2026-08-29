package web

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

// seedFileLinkOrg active un plugin pour une organisation et rend un
// serveur capable de servir un fichier.
func seedFileLinkOrg(t *testing.T, server *Server, files map[string][]byte) {
	t.Helper()

	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 1000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	now := time.Now().UTC()
	err := server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewPluginActivationRepository().Upsert(context.Background(), tx, persistence.PluginActivation{
			PluginName: "workspace", OrgID: "org-a", Enabled: true, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("activation: %v", err)
	}

	server.pluginManager = &fakePluginManager{files: files, mimeType: "video/mp4"}
}

// mintLink fabrique un lien pour le serveur de test.
func mintLink(t *testing.T, server *Server, path string) string {
	t.Helper()

	url, _, err := FileLinkMinter(server.cfg.Web.SessionSecret, "")(
		"workspace", "org-a", "cam", path)
	if err != nil {
		t.Fatalf("FileLinkMinter: %v", err)
	}

	return url
}

// Le lien sert le fichier en pièce jointe, avec les en-têtes qui
// interdisent au navigateur de l'interpréter ou de l'indexer.
func TestFileLink_ServesFileAsAttachment(t *testing.T) {
	server, ts, _ := testServer(t)
	seedFileLinkOrg(t, server, map[string][]byte{"dino.mp4": []byte("des octets de video")})

	resp, err := http.Get(ts.URL + mintLink(t, server, "dino.mp4"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut %d, attendu 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "des octets de video" {
		t.Errorf("corps %q", string(body))
	}

	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") ||
		!strings.Contains(got, "dino.mp4") {
		t.Errorf("Content-Disposition %q", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type %q, attendu video/mp4", got)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Robots-Tag":           "noindex",
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, attendu %q", header, got, want)
		}
	}
}

// Un jeton expiré ne sert plus rien, et ne dit pas pourquoi : il est
// indiscernable d'un lien inconnu.
func TestFileLink_RefusesExpiredToken(t *testing.T) {
	server, ts, _ := testServer(t)
	seedFileLinkOrg(t, server, map[string][]byte{"dino.mp4": []byte("x")})

	link := mintLink(t, server, "dino.mp4")

	// L'horloge du serveur avance au-delà de la durée de vie du lien.
	server.now = func() time.Time { return time.Now().Add(fileLinkTTL + time.Minute) }

	resp, err := http.Get(ts.URL + link)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("statut %d, attendu 404", resp.StatusCode)
	}
}

// Désactiver le plugin coupe ses liens : c'est le coupe-circuit de
// l'opérateur, relu à chaque accès.
func TestFileLink_RefusesWhenPluginDisabled(t *testing.T) {
	server, ts, _ := testServer(t)
	seedFileLinkOrg(t, server, map[string][]byte{"dino.mp4": []byte("x")})

	link := mintLink(t, server, "dino.mp4")

	now := time.Now().UTC()
	err := server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewPluginActivationRepository().Upsert(context.Background(), tx, persistence.PluginActivation{
			PluginName: "workspace", OrgID: "org-a", Enabled: false, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("désactivation: %v", err)
	}

	resp, err := http.Get(ts.URL + link)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("statut %d, attendu 404", resp.StatusCode)
	}
}

// Un fichier disparu du workspace rend 404 : le lien peut survivre à son
// contenu, un espace de travail inactif étant effacé au bout d'un jour.
func TestFileLink_ReturnsNotFoundForMissingFile(t *testing.T) {
	server, ts, _ := testServer(t)
	seedFileLinkOrg(t, server, map[string][]byte{})

	resp, err := http.Get(ts.URL + mintLink(t, server, "parti.mp4"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("statut %d, attendu 404", resp.StatusCode)
	}
}

// Un jeton d'une autre nature ne vaut pas un jeton de fichier : les
// contextes de signature sont cloisonnés.
func TestFileLink_RefusesTokenOfAnotherKind(t *testing.T) {
	server, ts, _ := testServer(t)
	seedFileLinkOrg(t, server, map[string][]byte{"dino.mp4": []byte("x")})

	// Un lien d'aperçu de brouillon, signé du même secret.
	preview, _, err := DraftPreviewMinter(server.cfg.Web.SessionSecret, "")(
		"workspace", "org-a", "cam", "dino.mp4")
	if err != nil {
		t.Fatalf("DraftPreviewMinter: %v", err)
	}
	token := strings.Trim(strings.TrimPrefix(preview, "/d/"), "/")

	resp, err := http.Get(ts.URL + "/f/" + token)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("statut %d, attendu 404", resp.StatusCode)
	}
}
