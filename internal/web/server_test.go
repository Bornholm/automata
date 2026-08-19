package web

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

const testPassword = "mot-de-passe-de-test"

// testPasswordHash est calculé une fois : bcrypt est volontairement lent.
var testPasswordHash string

func init() {
	hash, err := HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	testPasswordHash = hash
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()

	return &config.Config{
		Storage: config.Storage{Application: config.StorageApplication{
			Driver: "sqlite3",
			Path:   filepath.Join(t.TempDir(), "automata.sqlite"),
		}},
		Web: config.Web{
			Enabled:       true,
			Addr:          "127.0.0.1:0",
			BaseURL:       "https://automata.test",
			SessionSecret: strings.Repeat("s", 32),
			Admin:         config.WebAdmin{Email: "op@test.fr", PasswordHash: testPasswordHash},
			Credits: config.WebCredits{Packs: []config.WebCreditPack{
				{Credits: 1000, PriceEUR: 9},
				{Credits: 4400, PriceEUR: 35, Featured: true},
			}},
		},
	}
}

// testServer démarre un serveur complet sur base réelle, avec un client à
// jar de cookies.
func testServer(t *testing.T) (*Server, *httptest.Server, *http.Client) {
	t.Helper()

	cfg := testConfig(t)
	db, err := persistence.Open(context.Background(), cfg.Storage.Application)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	server := NewServer(cfg, db, nil, nil)
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	return server, ts, client
}

// csrfFrom extrait le jeton CSRF du jar du client.
func csrfFrom(t *testing.T, client *http.Client, base string) string {
	t.Helper()

	u, _ := url.Parse(base)
	for _, cookie := range client.Jar.Cookies(u) {
		if cookie.Name == csrfCookieName {
			return cookie.Value
		}
	}
	t.Fatal("aucun cookie CSRF posé")
	return ""
}

// login authentifie le client comme opérateur.
func login(t *testing.T, ts *httptest.Server, client *http.Client) {
	t.Helper()

	if _, err := client.Get(ts.URL + "/admin/login"); err != nil {
		t.Fatalf("GET login: %v", err)
	}

	resp, err := client.PostForm(ts.URL+"/admin/login", url.Values{
		"email":      {"op@test.fr"},
		"password":   {testPassword},
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	defer resp.Body.Close()
	// La connexion mène au tableau de bord : c'est l'écran de supervision
	// quotidienne (ADM-01).
	if resp.Request.URL.Path != "/admin/dashboard" {
		t.Fatalf("connexion attendue vers /admin/dashboard, arrivé sur %s", resp.Request.URL.Path)
	}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("lecture du corps: %v", err)
	}
	return string(raw)
}

func seedOrg(t *testing.T, s *Server, org persistence.Organization, credits int64) {
	t.Helper()

	err := s.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if org.CreatedAt.IsZero() {
			org.CreatedAt = time.Now()
			org.UpdatedAt = org.CreatedAt
		}
		if err := s.orgs.Insert(context.Background(), tx, org, false); err != nil {
			return err
		}
		if credits != 0 {
			return s.wallet.Insert(context.Background(), tx, persistence.WalletEntry{
				OrgID: org.ID, Kind: persistence.WalletKindWelcome,
				Label: "Crédits de bienvenue", Amount: credits, CreatedAt: time.Now(),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seedOrg: %v", err)
	}
}

func seedMember(t *testing.T, s *Server, member persistence.Member) {
	t.Helper()

	err := s.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if member.CreatedAt.IsZero() {
			member.CreatedAt = time.Now()
			member.UpdatedAt = member.CreatedAt
		}
		return s.members.Insert(context.Background(), tx, member, false)
	})
	if err != nil {
		t.Fatalf("seedMember: %v", err)
	}
}

func TestLogin_WrongPasswordShowsRemainingAttempts(t *testing.T) {
	_, ts, client := testServer(t)

	if _, err := client.Get(ts.URL + "/admin/login"); err != nil {
		t.Fatalf("GET login: %v", err)
	}
	resp, err := client.PostForm(ts.URL+"/admin/login", url.Values{
		"email":      {"op@test.fr"},
		"password":   {"faux"},
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	html := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statut %d, attendu 401", resp.StatusCode)
	}
	if !strings.Contains(html, "Ces identifiants ne correspondent pas. Il vous reste 4 tentatives.") {
		t.Error("le message d'échec avec tentatives restantes doit apparaître")
	}
}

func TestAdmin_RequiresSession(t *testing.T) {
	_, ts, client := testServer(t)

	resp, err := client.Get(ts.URL + "/admin/orgs")
	if err != nil {
		t.Fatalf("GET orgs: %v", err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Path != "/admin/login" {
		t.Fatalf("attendu une redirection vers /admin/login, arrivé sur %s", resp.Request.URL.Path)
	}
}

func TestAdmin_PostWithoutCSRFIsRejected(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 500)

	resp, err := client.PostForm(ts.URL+"/admin/orgs/org-a/grant", url.Values{
		"amount": {"100"}, "label": {"test"},
	})
	if err != nil {
		t.Fatalf("POST grant: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("statut %d, attendu 403 sans jeton CSRF", resp.StatusCode)
	}
}

func TestOrgs_RendersStatesAndBalances(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)

	seedOrg(t, server, persistence.Organization{ID: "creditee", DisplayName: "Famille Créditée"}, 3000)
	seedOrg(t, server, persistence.Organization{ID: "epuisee", DisplayName: "Cabinet Épuisé"}, 0)
	seedOrg(t, server, persistence.Organization{ID: "offerte", DisplayName: "Famille Offerte", Offered: true, MonthlyAllowance: 600}, 0)

	resp, err := client.Get(ts.URL + "/admin/orgs")
	if err != nil {
		t.Fatalf("GET orgs: %v", err)
	}
	html := body(t, resp)

	for _, expected := range []string{
		"Famille Créditée", "Créditée",
		"Cabinet Épuisé", "En pause — solde épuisé",
		"Famille Offerte", "Offerte",
		view.FormatInt(3000),
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("la liste doit contenir %q", expected)
		}
	}
}

var tokenPattern = regexp.MustCompile(`atm_[0-9A-Z]{4} · [0-9A-Z]{4} · [0-9A-Z]{4} · [0-9A-Z]{4}`)

func TestMemberToken_VisibleOnceThenNeverAgain(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 500)

	// Création du membre → redirection vers la fiche avec le jeton révélé.
	resp, err := client.PostForm(ts.URL+"/admin/orgs/org-a/members", url.Values{
		"display_name": {"Camille Roux"},
		"role":         {"member"},
		"csrf_token":   {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST membre: %v", err)
	}
	memberURL := resp.Request.URL
	html := body(t, resp)

	if !tokenPattern.MatchString(html) {
		t.Fatal("le jeton doit être affiché en quatre blocs à la création")
	}
	if !strings.Contains(html, "Visible une seule fois") {
		t.Error("la mention « Visible une seule fois » doit accompagner le jeton")
	}

	// Revisite de la fiche : le jeton n'est plus jamais affiché.
	memberPath := memberURL.Path
	resp2, err := client.Get(ts.URL + memberPath)
	if err != nil {
		t.Fatalf("GET fiche: %v", err)
	}
	html2 := body(t, resp2)
	if tokenPattern.MatchString(html2) {
		t.Fatal("le jeton ne doit plus être affiché après la première visite")
	}
	if !strings.Contains(html2, "seulement régénérable") {
		t.Error("la fiche doit expliquer que le code n'est plus affichable")
	}
}

func TestMemberToken_RegenerationRevokesPrevious(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 500)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	csrf := url.Values{"csrf_token": {csrfFrom(t, client, ts.URL)}}
	if _, err := client.PostForm(ts.URL+"/admin/members/cam/token", csrf); err != nil {
		t.Fatalf("POST jeton 1: %v", err)
	}

	var firstHash string
	_ = server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		token, _, err := server.linkTokens.LatestByMember(context.Background(), tx, "cam")
		firstHash = token.TokenHash
		return err
	})

	if _, err := client.PostForm(ts.URL+"/admin/members/cam/token", csrf); err != nil {
		t.Fatalf("POST jeton 2: %v", err)
	}

	_ = server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		// L'ancien jeton ne doit plus être consommable.
		_, found, err := server.linkTokens.FindPendingByHash(context.Background(), tx, firstHash, time.Now())
		if err != nil {
			return err
		}
		if found {
			t.Error("l'ancien jeton doit être révoqué par la régénération")
		}
		return nil
	})
}

// createProfileLink insère un lien de profil et retourne son chemin.
func createProfileLink(t *testing.T, s *Server, memberID string, expiresIn time.Duration) string {
	t.Helper()

	id, secretHash, urlPath, err := weblink.NewProfileLink()
	if err != nil {
		t.Fatalf("NewProfileLink: %v", err)
	}
	err = s.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		return s.profileLinks.Insert(context.Background(), tx, persistence.ProfileLink{
			ID: id, MemberID: memberID, TokenHash: secretHash,
			Status:    persistence.ProfileLinkStatusPending,
			ExpiresAt: time.Now().Add(expiresIn), CreatedAt: time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("insertion du lien: %v", err)
	}

	return "/p/" + urlPath
}

func TestProfileLink_SingleUseAcrossClients(t *testing.T) {
	server, ts, _ := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	path := createProfileLink(t, server, "cam", 15*time.Minute)

	// Premier client : ouverture réussie.
	jar1, _ := cookiejar.New(nil)
	client1 := &http.Client{Jar: jar1}
	resp, err := client1.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET lien: %v", err)
	}
	html := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(html, "Votre profil") {
		t.Fatalf("première ouverture attendue en 200 avec la page profil, statut %d", resp.StatusCode)
	}
	if !strings.Contains(html, "Camille") || !strings.Contains(html, "Org A") {
		t.Error("l'en-tête doit identifier la personne et son organisation")
	}

	// Le même client navigue : la session courte le porte.
	resp2, err := client1.Get(ts.URL + path + "/credits")
	if err != nil {
		t.Fatalf("GET crédits: %v", err)
	}
	if html2 := body(t, resp2); !strings.Contains(html2, "Vos crédits") {
		t.Error("la navigation sur session courte doit fonctionner après consommation du lien")
	}

	// Un autre client (sans cookie) rejoue le lien : déjà servi.
	client2 := &http.Client{}
	resp3, err := client2.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET relecture: %v", err)
	}
	if html3 := body(t, resp3); resp3.StatusCode != http.StatusGone || !strings.Contains(html3, "Ce lien a déjà servi") {
		t.Fatalf("relecture attendue en 410 « déjà servi », statut %d", resp3.StatusCode)
	}
}

func TestProfileLink_ExpiredRendersExpiredState(t *testing.T) {
	server, ts, client := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	path := createProfileLink(t, server, "cam", -time.Minute)

	resp, err := client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET lien: %v", err)
	}
	if html := body(t, resp); resp.StatusCode != http.StatusGone || !strings.Contains(html, "Ce lien a expiré") {
		t.Fatalf("lien périmé attendu en 410 « expiré », statut %d", resp.StatusCode)
	}
}

func TestProfileCredits_OfferedHidesPurchase(t *testing.T) {
	server, ts, client := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "offerte", DisplayName: "Famille Offerte", Offered: true, MonthlyAllowance: 600}, 0)
	seedMember(t, server, persistence.Member{ID: "denise", OrgID: "offerte", DisplayName: "Denise", Role: "member"})

	path := createProfileLink(t, server, "denise", 15*time.Minute)
	resp, err := client.Get(ts.URL + path + "/credits")
	if err != nil {
		t.Fatalf("GET crédits: %v", err)
	}
	html := body(t, resp)

	if !strings.Contains(html, "offert par Automata") {
		t.Error("la variante offerte doit porter la mention « offert par Automata »")
	}
	if strings.Contains(html, "Payer") || strings.Contains(html, "Ajouter des crédits") {
		t.Error("la variante offerte ne doit afficher aucun bouton d'achat")
	}
}

func TestProfileCredits_EmptyBalanceShowsPause(t *testing.T) {
	server, ts, client := testServer(t)
	seedOrg(t, server, persistence.Organization{ID: "vide", DisplayName: "Cabinet Vide"}, 0)
	seedMember(t, server, persistence.Member{ID: "marc", OrgID: "vide", DisplayName: "Marc", Role: "member"})

	path := createProfileLink(t, server, "marc", 15*time.Minute)
	resp, err := client.Get(ts.URL + path + "/credits")
	if err != nil {
		t.Fatalf("GET crédits: %v", err)
	}
	if html := body(t, resp); !strings.Contains(html, "Automata vous attend") {
		t.Error("un solde épuisé doit rendre la carte « service en pause »")
	}
}

func TestOrgGrant_AppendsLedgerEntry(t *testing.T) {
	server, ts, client := testServer(t)
	login(t, ts, client)
	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 500)

	resp, err := client.PostForm(ts.URL+"/admin/orgs/org-a/grant", url.Values{
		"amount":     {"300"},
		"label":      {"Geste commercial — incident"},
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST grant: %v", err)
	}
	html := body(t, resp)
	if !strings.Contains(html, "Geste commercial — incident") || !strings.Contains(html, "+300") {
		t.Error("le mouvement offert doit apparaître dans le portefeuille")
	}

	var balance int64
	_ = server.db.WithTx(context.Background(), func(tx *sql.Tx) error {
		var err error
		balance, err = server.wallet.Balance(context.Background(), tx, "org-a")
		return err
	})
	if balance != 800 {
		t.Fatalf("solde attendu 800, obtenu %d", balance)
	}
}
