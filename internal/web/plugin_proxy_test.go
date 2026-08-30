package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/plugin"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// fakePluginManager simule le gestionnaire : un backend httptest tient
// lieu d'interface embarquée du plugin.
type fakePluginManager struct {
	port     uint32
	token    string
	files    map[string][]byte
	mimeType string
}

func (f *fakePluginManager) Statuses() []plugin.Status {
	return []plugin.Status{{Name: "echo", Version: "0.0.1", Running: true, HasUI: f.port != 0}}
}
func (f *fakePluginManager) Restart(context.Context, string) bool { return true }

// files, s'il est renseigné, est servi par OpenFile quel que soit le
// chemin demandé — de quoi éprouver la route /f/ sans plugin réel.
func (f *fakePluginManager) OpenFile(_ context.Context, _ string, _ plugin.CallContext, path string) (plugin.FileMeta, io.ReadCloser, error) {
	data, ok := f.files[path]
	if !ok {
		return plugin.FileMeta{}, nil, errors.New("fichier introuvable")
	}

	return plugin.FileMeta{
			Filename: path,
			MimeType: f.mimeType,
			Size:     int64(len(data)),
		},
		io.NopCloser(bytes.NewReader(data)), nil
}
func (f *fakePluginManager) UIEndpoint(name string) (uint32, string, bool) {
	if name != "echo" || f.port == 0 {
		return 0, "", false
	}
	return f.port, f.token, true
}

func seedPluginProfile(t *testing.T, server *Server) (linkPath string) {
	t.Helper()

	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 1000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	now := time.Now().UTC()
	err := server.DB.WithTx(context.Background(), func(tx *sql.Tx) error {
		return persistence.NewPluginActivationRepository().Upsert(context.Background(), tx, persistence.PluginActivation{
			PluginName: "echo", OrgID: "org-a", Enabled: true, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("activation: %v", err)
	}

	return createProfileLink(t, server, "cam", 15*time.Minute)
}

// Le proxy réécrit le chemin, injecte le contrat d'identité et le jeton
// d'UI, retire les Set-Cookie du plugin et force frame-ancestors.
func TestPluginProxy_InjectsContractAndStripsCookies(t *testing.T) {
	var got http.Header
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotPath = r.URL.Path
		http.SetCookie(w, &http.Cookie{Name: "plugin_session", Value: "x"})
		fmt.Fprint(w, "plugin-ui")
	}))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)
	port := portOf(t, backendURL)

	server, ts, _ := testServer(t)
	server.PluginMgr = &fakePluginManager{port: port, token: "jeton-ui-secret"}
	seedPluginProfile(t, server)

	uiPath := core.PluginUIPrefix + server.PluginUIToken(core.PluginViewMember, "org-a", "cam", "echo", server.Now())

	// SANS cookie, délibérément : une iframe sandbouclée n'en transporte
	// aucun. C'est la régression du 2026-08-23 — le proxy rendait alors
	// « ce lien a déjà servi » à l'intérieur du cadre.
	resp, err := (&http.Client{}).Get(ts.URL + uiPath + "/settings")
	if err != nil {
		t.Fatalf("GET interface: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("interface en %d, attendu 200", resp.StatusCode)
	}
	if gotPath != "/settings" {
		t.Errorf("chemin relayé %q, attendu /settings", gotPath)
	}
	for header, want := range map[string]string{
		"X-Automata-Org-Id":           "org-a",
		"X-Automata-Member-Id":        "cam",
		"X-Automata-View":             "member",
		"X-Automata-Ui-Token":         "jeton-ui-secret",
		"X-Automata-Plugin-Base-Path": uiPath + "/",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, attendu %q", header, got.Get(header), want)
		}
	}

	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("un Set-Cookie du plugin a traversé le proxy")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Errorf("CSP absente ou incomplète: %q", csp)
	}
}

// Un plugin inactif pour l'organisation du membre n'existe pas : 404,
// aucune requête vers le port du plugin.
func TestPluginProxy_HiddenWhenInactive(t *testing.T) {
	backendHit := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
	}))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)

	server, ts, client := testServer(t)
	server.PluginMgr = &fakePluginManager{port: portOf(t, backendURL), token: "t"}

	seedOrg(t, server, persistence.Organization{ID: "org-b", DisplayName: "Org B"}, 0)
	seedMember(t, server, persistence.Member{ID: "zoe", OrgID: "org-b", DisplayName: "Zoé", Role: "member"})

	// Le jeton est valide : c'est l'activation, revérifiée à chaque
	// requête, qui doit refuser. Un plugin désactivé après l'émission
	// devient injoignable sur-le-champ.
	uiPath := core.PluginUIPrefix + server.PluginUIToken(core.PluginViewMember, "org-b", "zoe", "echo", server.Now())

	resp, err := client.Get(ts.URL + uiPath + "/")
	if err != nil {
		t.Fatalf("GET interface: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("plugin inactif servi en %d, attendu 404", resp.StatusCode)
	}
	if backendHit {
		t.Error("le port du plugin a été contacté pour une organisation inactive")
	}
}

// La sandbox de l'iframe est une garantie de sécurité : sa disparition
// serait une régression, pas un détail de style.
func TestProfileIframe_IsSandboxed(t *testing.T) {
	// Depuis le 2026-08-23, l'interface d'un plugin a son propre onglet :
	// c'est cette page qui porte l'iframe.
	echo := view.ProfilePluginUI{Name: "echo", Title: "echo", Src: "/p/abc/plugins/echo/ui/"}
	page := view.ProfilePluginPage{
		LinkID:    "abc",
		Current:   echo,
		PluginUIs: []view.ProfilePluginUI{echo},
	}

	var sb strings.Builder
	if err := view.ProfilePlugin(page).Render(context.Background(), &sb); err != nil {
		t.Fatalf("rendu: %v", err)
	}
	html := sb.String()

	// allow-downloads (2026-08-29) : un téléchargement exige un geste de
	// l'utilisateur, et le contenu vient d'UI de plugins proxifiées et
	// authentifiées — c'est ce qui permet l'archive zip du plugin pages.
	if !strings.Contains(html, `sandbox="allow-forms allow-scripts allow-downloads"`) {
		t.Error("iframe sans sandbox, ou sandbox affaiblie")
	}
	if strings.Contains(html, "allow-same-origin") {
		t.Error("allow-same-origin donnerait au plugin l'accès aux cookies de l'application")
	}
	if strings.Contains(html, "allow-popups") {
		t.Error("allow-popups laisserait une UI de plugin ouvrir des fenêtres hors sandbox")
	}
}

func portOf(t *testing.T, u *url.URL) uint32 {
	t.Helper()
	var port uint32
	if _, err := fmt.Sscanf(u.Port(), "%d", &port); err != nil {
		t.Fatalf("port du backend: %v", err)
	}
	return port
}

var _ = io.Discard

// L'exemption CSRF doit être exactement aussi large que nécessaire : les
// chemins d'UI de plugin, et rien d'autre.
func TestIsPluginUIPath(t *testing.T) {
	exempt := []string{
		"/plugin-ui/jeton/",
		"/plugin-ui/jeton/save",
		"/plugin-ui/jeton/oauth/callback",
	}
	for _, path := range exempt {
		if !isPluginUIPath(path) {
			t.Errorf("%q devrait être exempté", path)
		}
	}

	protected := []string{
		"/admin/orgs/org-a/plugins",
		"/admin/orgs/org-a/grant",
		"/admin/plugins/email/restart",
		"/admin/orgs",
		"/admin/pricing/settings",
		"/plugin-ui/jeton/../../admin/grant",
		// Un jeton sans rien derrière ne désigne aucune interface.
		"/plugin-ui/jeton",
		"/plugin-ui//save",
	}
	for _, path := range protected {
		if isPluginUIPath(path) {
			t.Errorf("%q ne doit PAS être exempté du contrôle CSRF", path)
		}
	}
}

// Une écoute en boucle locale dans un conteneur est injoignable : ni le
// proxy de l'hôte, ni la sonde de démarrage n'y accèdent, et le message
// obtenu est un « connection refused » qui ne dit pas sa cause.
func TestIsLoopbackAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:5000", "localhost:8080", "[::1]:5000"}
	for _, addr := range loopback {
		if !isLoopbackAddr(addr) {
			t.Errorf("%q devrait être reconnue comme boucle locale", addr)
		}
	}

	reachable := []string{"0.0.0.0:5000", ":5000", "192.168.1.10:5000", "[::]:5000"}
	for _, addr := range reachable {
		if isLoopbackAddr(addr) {
			t.Errorf("%q ne doit pas être prise pour une boucle locale", addr)
		}
	}
}

// Un jeton d'interface ne vaut que pour ce qu'il désigne. Sans ces refus,
// il suffirait d'un jeton quelconque pour atteindre n'importe quelle
// interface de n'importe quelle organisation.
func TestPluginUIToken_ScopeIsEnforced(t *testing.T) {
	backendHit := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
	}))
	defer backend.Close()
	backendURL, _ := url.Parse(backend.URL)

	server, ts, _ := testServer(t)
	server.PluginMgr = &fakePluginManager{port: portOf(t, backendURL), token: "t"}
	seedPluginProfile(t, server)

	now := server.Now()
	valid := server.PluginUIToken(core.PluginViewMember, "org-a", "cam", "echo", now)

	cases := map[string]string{
		// Émis pour un autre plugin : le nom voyage DANS le jeton, il ne
		// peut pas être changé en chemin.
		"autre plugin": server.PluginUIToken(core.PluginViewMember, "org-a", "cam", "autre", now),
		// Émis il y a plus longtemps que sa durée de vie.
		"périmé": server.PluginUIToken(core.PluginViewMember, "org-a", "cam", "echo", now.Add(-core.PluginUITokenTTL-time.Minute)),
		// Signature invalide : un octet modifié suffit.
		"falsifié": valid[:len(valid)-3] + "AAA",
		// Pas un jeton du tout.
		"informe": "n-importe-quoi",
	}

	for name, token := range cases {
		resp, err := (&http.Client{}).Get(ts.URL + core.PluginUIPrefix + token + "/")
		if err != nil {
			t.Fatalf("%s: GET: %v", name, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("jeton %s servi en %d, attendu 404", name, resp.StatusCode)
		}
	}

	if backendHit {
		t.Error("le port du plugin a été contacté avec un jeton hors de sa portée")
	}
}

// Un jeton de profil ne devient pas un accès d'opérateur, et
// réciproquement : la vue est scellée dans le jeton.
func TestPluginUIToken_ViewIsSealed(t *testing.T) {
	server, _, _ := testServer(t)
	now := server.Now()

	token := server.PluginUIToken(core.PluginViewMember, "org-a", "cam", "echo", now)
	view, orgID, memberID, name, ok := server.ParsePluginUIToken(token, now)
	if !ok {
		t.Fatal("un jeton fraîchement émis doit être accepté")
	}
	if view != core.PluginViewMember || orgID != "org-a" || memberID != "cam" || name != "echo" {
		t.Errorf("jeton mal relu: %q %q %q %q", view, orgID, memberID, name)
	}

	// Un cookie de session signé par la même clé n'est pas un jeton
	// d'interface : le « kind » les sépare.
	session := server.Signer.Sign(core.SessionPayload("profile", "cam/lien", now.Add(time.Hour)))
	if _, _, _, _, ok := server.ParsePluginUIToken(base64.RawURLEncoding.EncodeToString([]byte(session)), now); ok {
		t.Error("un cookie de session a été accepté comme jeton d'interface")
	}
}
