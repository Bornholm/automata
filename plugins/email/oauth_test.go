package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// Le state signé est la seule preuve d'identité du retour public : sa
// vérification doit refuser une signature étrangère et un state périmé.
func TestState_SignAndVerify(t *testing.T) {
	now := time.Now()
	st := oauthState{OrgID: "atelier", MemberID: "cam", Nonce: "n1", IssuedAt: now.Unix()}

	raw, err := signState("graine-du-membre", st)
	if err != nil {
		t.Fatalf("signState: %v", err)
	}

	parsed, encoded, sig, ok := parseState(raw)
	if !ok || parsed.MemberID != "cam" || parsed.OrgID != "atelier" {
		t.Fatalf("état relu inattendu: %+v", parsed)
	}
	if !verifyState("graine-du-membre", encoded, sig, parsed, now) {
		t.Error("un état valide a été refusé")
	}
	if verifyState("graine-d-un-autre", encoded, sig, parsed, now) {
		t.Error("une signature étrangère a été acceptée")
	}
	if verifyState("graine-du-membre", encoded, sig, parsed, now.Add(stateTTL+time.Minute)) {
		t.Error("un état périmé a été accepté")
	}
	if _, _, _, ok := parseState("pas-un-state"); ok {
		t.Error("un state malformé a été accepté")
	}
}

// XOAUTH2 : la chaîne d'initialisation suit le format attendu par Google.
func TestXOAUTH2Format(t *testing.T) {
	got := xoauth2("cam@gmail.test", "jeton-acces")
	want := "user=cam@gmail.test\x01auth=Bearer jeton-acces\x01\x01"
	if got != want {
		t.Errorf("chaîne XOAUTH2 = %q, attendu %q", got, want)
	}
}

// fakeGoogle sert les points d'accès jeton, en comptant les échanges.
type fakeGoogle struct {
	server    *httptest.Server
	exchanges int
	refreshes int
	// noRefreshToken simule une seconde autorisation sans prompt=consent.
	noRefreshToken bool
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		resp := tokenResponse{AccessToken: "acces-1", ExpiresIn: 3600}

		switch r.FormValue("grant_type") {
		case "authorization_code":
			g.exchanges++
			if !g.noRefreshToken {
				resp.RefreshToken = "refresh-durable"
			}
			if r.FormValue("code") == "mauvais-code" {
				resp = tokenResponse{Error: "invalid_grant"}
			}
		case "refresh_token":
			g.refreshes++
			resp.AccessToken = fmt.Sprintf("acces-renouvele-%d", g.refreshes)
			if r.FormValue("refresh_token") == "revoque" {
				resp = tokenResponse{Error: "invalid_grant"}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(g.server.Close)

	original := googleTokenURLOverride
	googleTokenURLOverride = g.server.URL
	t.Cleanup(func() { googleTokenURLOverride = original })

	return g
}

func seedOAuthApp(t *testing.T, host *fakeHost) {
	t.Helper()
	raw, _ := json.Marshal(oauthApp{ClientID: "client-id"})
	_ = host.SaveConfig(context.Background(), "atelier", "", string(raw))
	_ = host.SetSecret(context.Background(), "atelier", "", secretKeyClientSecret, "client-secret")
}

// Un jeton d'accès encore valide est réutilisé ; expiré, il est renouvelé
// et le nouveau est persisté — sans redemander de consentement.
func TestAccessToken_RefreshesWhenExpired(t *testing.T) {
	google := newFakeGoogle(t)
	host := newFakeHost()
	seedOAuthApp(t, host)

	ctx := context.Background()
	_ = host.SetSecret(ctx, "atelier", "cam", secretKeyRefreshToken, "refresh-durable")
	_ = host.SetSecret(ctx, "atelier", "cam", secretKeyAccessToken, "acces-en-cours")
	_ = host.SetSecret(ctx, "atelier", "cam", secretKeyExpiry, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))

	token, err := accessToken(ctx, host, "atelier", "cam")
	if err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if token != "acces-en-cours" || google.refreshes != 0 {
		t.Errorf("jeton valide non réutilisé (%q, %d renouvellement(s))", token, google.refreshes)
	}

	// Expiré : renouvellement.
	_ = host.SetSecret(ctx, "atelier", "cam", secretKeyExpiry, strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))

	token, err = accessToken(ctx, host, "atelier", "cam")
	if err != nil {
		t.Fatalf("accessToken après expiration: %v", err)
	}
	if token != "acces-renouvele-1" || google.refreshes != 1 {
		t.Errorf("renouvellement inattendu: %q (%d)", token, google.refreshes)
	}

	stored, _, _ := host.GetSecret(ctx, "atelier", "cam", secretKeyAccessToken)
	if stored != "acces-renouvele-1" {
		t.Errorf("nouveau jeton non persisté: %q", stored)
	}
}

// Un consentement révoqué donne un message qui invite à reconnecter, pas
// une panne obscure — et jamais le contenu du jeton.
func TestAccessToken_RevokedConsent(t *testing.T) {
	newFakeGoogle(t)
	host := newFakeHost()
	seedOAuthApp(t, host)

	ctx := context.Background()
	_ = host.SetSecret(ctx, "atelier", "cam", secretKeyRefreshToken, "revoque")

	_, err := accessToken(ctx, host, "atelier", "cam")
	if err == nil {
		t.Fatal("un consentement révoqué a été accepté")
	}
	if !strings.Contains(err.Error(), "reconnect") {
		t.Errorf("message peu actionnable: %v", err)
	}
	if strings.Contains(err.Error(), "revoque") {
		t.Errorf("le jeton apparaît dans l'erreur: %v", err)
	}
}

// Le parcours complet du retour Google : state signé, échange du code,
// jetons rangés, configuration basculée en mode Gmail.
func TestOAuthCallback_ConnectsMailbox(t *testing.T) {
	google := newFakeGoogle(t)
	host := newFakeHost()
	seedOAuthApp(t, host)

	ctx := context.Background()
	_ = host.SaveConfig(ctx, "atelier", "cam", memberConfig{Username: "cam@gmail.test", AllowRead: true}.marshal())

	seed, err := memberStateSeed(ctx, host, "atelier", "cam")
	if err != nil {
		t.Fatalf("graine: %v", err)
	}
	state, _ := signState(seed, oauthState{OrgID: "atelier", MemberID: "cam", Nonce: "n", IssuedAt: time.Now().Unix()})

	rec := callback(t, host, url.Values{"code": {"bon-code"}, "state": {state}})
	if rec.Code != http.StatusOK {
		t.Fatalf("callback en %d: %s", rec.Code, rec.Body.String())
	}
	if google.exchanges != 1 {
		t.Errorf("%d échange(s) de code, attendu 1", google.exchanges)
	}

	if refresh, found, _ := host.GetSecret(ctx, "atelier", "cam", secretKeyRefreshToken); !found || refresh != "refresh-durable" {
		t.Errorf("jeton durable non rangé: %q", refresh)
	}
	raw, _, _ := host.GetConfig(ctx, "atelier", "cam")
	cfg, _ := parseConfig(raw)
	if !cfg.oauth() || cfg.IMAPHost != googleIMAPHost || cfg.SMTPPort != googleSMTPPort {
		t.Errorf("configuration non basculée en Gmail: %+v", cfg)
	}
	if !cfg.AllowRead {
		t.Error("les réglages du membre ont été perdus à la connexion")
	}
}

// Un state forgé ne connecte rien : c'est la seule barrière de la route
// publique.
func TestOAuthCallback_RejectsForgedState(t *testing.T) {
	google := newFakeGoogle(t)
	host := newFakeHost()
	seedOAuthApp(t, host)

	ctx := context.Background()
	if _, err := memberStateSeed(ctx, host, "atelier", "cam"); err != nil {
		t.Fatal(err)
	}
	forged, _ := signState("graine-inventee", oauthState{OrgID: "atelier", MemberID: "cam", Nonce: "n", IssuedAt: time.Now().Unix()})

	rec := callback(t, host, url.Values{"code": {"bon-code"}, "state": {forged}})
	if rec.Code == http.StatusOK {
		t.Fatal("un state forgé a été accepté")
	}
	if google.exchanges != 0 {
		t.Error("le code a été échangé malgré un state invalide")
	}
	if _, found, _ := host.GetSecret(ctx, "atelier", "cam", secretKeyRefreshToken); found {
		t.Error("un jeton a été rangé pour un state forgé")
	}
}

// Sans jeton durable (Google n'en renvoie qu'à la première autorisation),
// connecter serait un piège : la boîte cesserait de fonctionner à
// l'expiration. Le plugin refuse et explique.
func TestOAuthCallback_RequiresRefreshToken(t *testing.T) {
	google := newFakeGoogle(t)
	google.noRefreshToken = true
	host := newFakeHost()
	seedOAuthApp(t, host)

	ctx := context.Background()
	seed, _ := memberStateSeed(ctx, host, "atelier", "cam")
	state, _ := signState(seed, oauthState{OrgID: "atelier", MemberID: "cam", Nonce: "n", IssuedAt: time.Now().Unix()})

	rec := callback(t, host, url.Values{"code": {"bon-code"}, "state": {state}})
	if rec.Code == http.StatusOK {
		t.Fatal("une autorisation sans jeton durable a été acceptée")
	}
	if _, found, _ := host.GetSecret(ctx, "atelier", "cam", secretKeyRefreshToken); found {
		t.Error("un jeton vide a été rangé")
	}
}

// L'URL de consentement demande explicitement un jeton durable.
func TestAuthorizationURL_AsksForOfflineAccess(t *testing.T) {
	raw := authorizationURL(oauthApp{ClientID: "cid", ClientSecret: "cs"},
		"https://automata.test/plugins/email/oauth/callback", "state-1", "cam@gmail.test")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	q := parsed.Query()
	for key, want := range map[string]string{
		"access_type":  "offline",
		"prompt":       "consent",
		"scope":        gmailScope,
		"state":        "state-1",
		"login_hint":   "cam@gmail.test",
		"redirect_uri": "https://automata.test/plugins/email/oauth/callback",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, attendu %q", key, q.Get(key), want)
		}
	}
	if strings.Contains(raw, "cs") && q.Get("client_secret") != "" {
		t.Error("le secret client ne doit jamais partir dans l'URL de consentement")
	}
}

// callback joue le retour de Google sur le handler du plugin.
func callback(t *testing.T, host pluginsdk.HostClient, params url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/oauth/callback?"+params.Encode(), nil)
	r.Header.Set(pluginsdk.HeaderBaseURL, "https://automata.test")
	r = r.WithContext(pluginsdk.ContextWithHostClientForTest(r.Context(), host))

	rec := httptest.NewRecorder()
	handleOAuthCallback(rec, r)
	return rec
}
