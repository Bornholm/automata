package web

import (
	"context"
	"database/sql"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
)

// seedPublicSite publie une petite page pour org-a/cam sous le slug donné.
func seedPublicSite(t *testing.T, server *Server, slug string, enabled bool) {
	t.Helper()

	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 0)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	now := time.Now().UTC()
	err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		ctx := context.Background()
		if err := persistence.NewPluginActivationRepository().Upsert(ctx, tx, persistence.PluginActivation{
			PluginName: "pages", OrgID: "org-a", Enabled: enabled, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}

		objects := persistence.NewPluginObjectRepository()
		for key, content := range map[string]string{
			"index.html": "<html><body>bienvenue</body></html>",
			"style.css":  "body{margin:0}",
			"video.mp4":  strings.Repeat("v", 100),
			"page.xyz":   "inconnu",
		} {
			if err := objects.Upsert(ctx, tx, persistence.PluginObject{
				PluginName: "pages", OrgID: "org-a", MemberID: "cam",
				Collection: "spaces/demo/live", Key: key,
				Size: int64(len(content)), Data: []byte(content),
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}

		return persistence.NewPluginPublicSiteRepository().Insert(ctx, tx, persistence.PluginPublicSite{
			Slug: slug, PluginName: "pages", OrgID: "org-a", MemberID: "cam",
			Collection: "spaces/demo/live", PublishedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("semis de la page publique: %v", err)
	}
}

// noRedirectClient n'suit pas les redirections : on veut voir le 301.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// La racine sans slash redirige (liens relatifs), l'index est servi avec
// les en-têtes d'isolement, et les assets suivent.
func TestPublicSite_ServesPublishedContent(t *testing.T) {
	server, ts, _ := testServer(t)
	seedPublicSite(t, server, "x7k2m9p4qr", true)
	client := noRedirectClient()

	resp, err := client.Get(ts.URL + "/s/x7k2m9p4qr")
	if err != nil {
		t.Fatalf("GET racine: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/s/x7k2m9p4qr/" {
		t.Errorf("racine = (%d, %q)", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.Get(ts.URL + "/s/x7k2m9p4qr/")
	if err != nil {
		t.Fatalf("GET index: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "bienvenue") {
		t.Fatalf("index = (%d, %q)", resp.StatusCode, body)
	}
	for header, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Security-Policy": "sandbox allow-scripts",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Robots-Tag":            "noindex",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, attendu %q", header, got, want)
		}
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Error("aucun cookie ne doit être posé sur une page publique")
	}

	resp, err = client.Get(ts.URL + "/s/x7k2m9p4qr/style.css")
	if err != nil {
		t.Fatalf("GET css: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("css = (%d, %q)", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// Les vidéos sont servies par plages : sans Range, la lecture mobile ne
// démarre pas.
func TestPublicSite_ServesRangeRequests(t *testing.T) {
	server, ts, _ := testServer(t)
	seedPublicSite(t, server, "x7k2m9p4qr", true)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/s/x7k2m9p4qr/video.mp4", nil)
	req.Header.Set("Range", "bytes=0-9")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET Range: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 {
		t.Errorf("Range = (%d, %d octets)", resp.StatusCode, len(body))
	}
	if resp.Header.Get("Content-Type") != "video/mp4" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
}

// Une extension hors allowlist part en téléchargement : le navigateur ne
// doit jamais deviner un type exécutable.
func TestPublicSite_UnknownExtensionIsAttachment(t *testing.T) {
	server, ts, _ := testServer(t)
	seedPublicSite(t, server, "x7k2m9p4qr", true)

	resp, err := noRedirectClient().Get(ts.URL + "/s/x7k2m9p4qr/page.xyz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if resp.Header.Get("Content-Type") != "application/octet-stream" ||
		resp.Header.Get("Content-Disposition") != "attachment" {
		t.Errorf("en-têtes = (%q, %q)", resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
	}
}

// Slug inconnu, fichier absent, chemin suspect : 404 nets, sans fuite.
func TestPublicSite_NotFound(t *testing.T) {
	server, ts, _ := testServer(t)
	seedPublicSite(t, server, "x7k2m9p4qr", true)
	client := noRedirectClient()

	// Un « .. » littéral n'atteint jamais le handler (le mux canonicalise
	// et redirige) ; la forme encodée, elle, doit être refusée par le
	// handler lui-même.
	for _, url := range []string{
		"/s/inconnu000/",
		"/s/x7k2m9p4qr/absent.html",
		"/s/x7k2m9p4qr/%2e%2e/admin/dashboard",
	} {
		resp, err := client.Get(ts.URL + url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, attendu 404", url, resp.StatusCode)
		}
	}
}

// seedDraft dépose un brouillon (non publié) pour org-a/cam.
func seedDraft(t *testing.T, server *Server, enabled bool) {
	t.Helper()

	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 0)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	now := time.Now().UTC()
	err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		ctx := context.Background()
		if err := persistence.NewPluginActivationRepository().Upsert(ctx, tx, persistence.PluginActivation{
			PluginName: "pages", OrgID: "org-a", Enabled: enabled, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		return persistence.NewPluginObjectRepository().Upsert(ctx, tx, persistence.PluginObject{
			PluginName: "pages", OrgID: "org-a", MemberID: "cam",
			Collection: "spaces/demo/draft", Key: "index.html",
			Size: 22, Data: []byte("<html>brouillon</html>"),
			CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("semis du brouillon: %v", err)
	}
}

// Le lien de prévisualisation signé sert le brouillon — jamais mis en
// cache — et meurt avec son échéance ou une signature invalide.
func TestDraftPreview_ServesTheDraftBehindASignedToken(t *testing.T) {
	server, ts, _ := testServer(t)
	seedDraft(t, server, true)
	client := noRedirectClient()

	mint := core.DraftPreviewMinter(server.Cfg.Web.SessionSecret, server.Cfg.Web.BaseURL)
	url, _, err := mint("pages", "org-a", "cam", "spaces/demo/draft")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(url, server.Cfg.Web.BaseURL+"/d/"), "/")

	// Racine sans slash : redirection, comme la route publique.
	resp, err := client.Get(ts.URL + "/d/" + token)
	if err != nil {
		t.Fatalf("GET racine: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("racine = %d, attendu 301", resp.StatusCode)
	}

	resp, err = client.Get(ts.URL + "/d/" + token + "/")
	if err != nil {
		t.Fatalf("GET brouillon: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "brouillon") {
		t.Fatalf("brouillon = (%d, %q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, attendu no-store", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "sandbox allow-scripts" {
		t.Errorf("CSP = %q", got)
	}

	// Jeton expiré : 404 indistinct.
	sig := core.NewSigner(server.Cfg.Web.SessionSecret)
	expired := base64.RawURLEncoding.EncodeToString([]byte(sig.Sign(
		core.SessionPayload("draft-preview", "pages/org-a/cam/spaces/demo/draft", time.Now().Add(-time.Minute)))))
	resp, err = client.Get(ts.URL + "/d/" + expired + "/")
	if err != nil {
		t.Fatalf("GET expiré: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("jeton expiré = %d, attendu 404", resp.StatusCode)
	}

	// Jeton forgé (mauvais secret) : 404 aussi.
	forged := base64.RawURLEncoding.EncodeToString([]byte(core.NewSigner("mauvais-secret-de-32-octets-....").Sign(
		core.SessionPayload("draft-preview", "pages/org-a/cam/spaces/demo/draft", time.Now().Add(time.Hour)))))
	resp, err = client.Get(ts.URL + "/d/" + forged + "/")
	if err != nil {
		t.Fatalf("GET forgé: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("jeton forgé = %d, attendu 404", resp.StatusCode)
	}
}

// Le coupe-circuit opérateur vaut aussi pour les prévisualisations.
func TestDraftPreview_DisabledPluginIs404(t *testing.T) {
	server, ts, _ := testServer(t)
	seedDraft(t, server, false)

	mint := core.DraftPreviewMinter(server.Cfg.Web.SessionSecret, server.Cfg.Web.BaseURL)
	url, _, err := mint("pages", "org-a", "cam", "spaces/demo/draft")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(url, server.Cfg.Web.BaseURL+"/d/"), "/")

	resp, err := noRedirectClient().Get(ts.URL + "/d/" + token + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("plugin désactivé = %d, attendu 404", resp.StatusCode)
	}
}

// Désactiver le plugin pour l'organisation coupe la page : c'est le
// coupe-circuit de l'opérateur.
func TestPublicSite_DisabledPluginIs404(t *testing.T) {
	server, ts, _ := testServer(t)
	seedPublicSite(t, server, "x7k2m9p4qr", false)

	resp, err := noRedirectClient().Get(ts.URL + "/s/x7k2m9p4qr/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("plugin désactivé = %d, attendu 404", resp.StatusCode)
	}
}
