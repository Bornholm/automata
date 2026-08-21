package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/automata/internal/persistence"
)

const testWebhookSecret = "whsec_test_secret"

// signedHeader forge un en-tête Stripe-Signature valide pour payload.
func signedHeader(payload string, at time.Time, secret string) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write([]byte(payload))

	return fmt.Sprintf("t=%s,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

const completedPayload = `{"type":"checkout.session.completed","data":{"object":{"id":"cs_test_1",` +
	`"payment_status":"paid","metadata":{"org_id":"atelier","credits":"2500"}}}}`

func TestVerifyStripeSignature_AcceptsValidEvent(t *testing.T) {
	now := time.Now()

	event, err := verifyStripeSignature([]byte(completedPayload), signedHeader(completedPayload, now, testWebhookSecret), testWebhookSecret, now)
	if err != nil {
		t.Fatalf("signature valide refusée: %v", err)
	}

	if event.Type != "checkout.session.completed" || event.Data.Object.ID != "cs_test_1" {
		t.Errorf("événement mal décodé: %+v", event)
	}
	if event.Data.Object.Metadata.OrgID != "atelier" || event.Data.Object.Metadata.Credits != "2500" {
		t.Errorf("métadonnées mal décodées: %+v", event.Data.Object.Metadata)
	}
}

// Sans vérification stricte, n'importe qui pourrait créditer un
// portefeuille : chaque forme d'altération doit être refusée.
func TestVerifyStripeSignature_RejectsForgedOrStaleEvents(t *testing.T) {
	now := time.Now()

	cases := map[string]struct {
		payload string
		header  string
	}{
		"signature d'un autre secret": {completedPayload, signedHeader(completedPayload, now, "whsec_autre")},
		"corps modifié après signature": {
			`{"type":"checkout.session.completed","data":{"object":{"id":"cs_test_1","payment_status":"paid","metadata":{"org_id":"atelier","credits":"999999"}}}}`,
			signedHeader(completedPayload, now, testWebhookSecret),
		},
		"capture rejouée hors tolérance": {completedPayload, signedHeader(completedPayload, now.Add(-time.Hour), testWebhookSecret)},
		"en-tête absent":                 {completedPayload, ""},
		"en-tête sans signature":         {completedPayload, "t=" + strconv.FormatInt(now.Unix(), 10)},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifyStripeSignature([]byte(tc.payload), tc.header, testWebhookSecret, now); err == nil {
				t.Error("événement accepté alors qu'il aurait dû être refusé")
			}
		})
	}
}

// TestCheckoutSession_SendsTaxCode : Stripe refuse la création de session
// dès que Stripe Tax est activé sur le compte et que le produit créé à la
// volée n'est pas classé fiscalement. Le formulaire doit donc porter le
// code, sans quoi le paiement échoue en production seulement.
func TestCheckoutSession_SendsTaxCode(t *testing.T) {
	var form url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("formulaire illisible: %v", err)
		}
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"url":"https://checkout.stripe.test/session"}`)
	}))
	defer server.Close()

	client := newStripeClient("sk_test", "txcd_10103000")
	client.baseURL = server.URL

	checkoutURL, err := client.checkoutSession(context.Background(), "acme", "cam", 4400, 35, "https://exemple/ok", "https://exemple/ko")
	if err != nil {
		t.Fatalf("création de session: %v", err)
	}
	if checkoutURL != "https://checkout.stripe.test/session" {
		t.Fatalf("URL de paiement inattendue: %q", checkoutURL)
	}

	if got := form.Get("line_items[0][price_data][product_data][tax_code]"); got != "txcd_10103000" {
		t.Errorf("code fiscal transmis = %q, attendu txcd_10103000", got)
	}
	if got := form.Get("line_items[0][price_data][unit_amount]"); got != "3500" {
		t.Errorf("montant transmis = %q, attendu 3500", got)
	}
	if got := form.Get("metadata[credits]"); got != "4400" {
		t.Errorf("crédits transmis = %q, attendu 4400", got)
	}
	// Le prix revient par les métadonnées : c'est la seule recette que le
	// webhook pourra inscrire au portefeuille.
	if got := form.Get("metadata[price_eur]"); got != "35" {
		t.Errorf("prix transmis = %q, attendu 35", got)
	}
	// Sans le membre, la confirmation d'achat ne saurait à qui parler.
	if got := form.Get("metadata[member_id]"); got != "cam" {
		t.Errorf("membre transmis = %q, attendu cam", got)
	}
}

// TestCheckout_ReturnLinkSurvivesTheOriginalLink : le lien de profil est à
// usage unique, donc déjà consommé quand Stripe renvoie le client sur
// success_url. Sans lien de retour neuf, l'écran « ce lien a déjà servi »
// s'affiche juste après le paiement — exactement là où il faut confirmer
// l'achat.
func TestCheckout_ReturnLinkSurvivesTheOriginalLink(t *testing.T) {
	var form url.Values

	stripeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"url":"https://checkout.stripe.test/session"}`)
	}))
	defer stripeAPI.Close()

	server, ts, client := testServer(t)
	server.cfg.Web.BaseURL = ts.URL
	server.stripe = newStripeClient("sk_test", "txcd_10000000")
	server.stripe.baseURL = stripeAPI.URL

	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 3000)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	path := createProfileLink(t, server, "cam", 15*time.Minute)
	openProfileLink(t, ts, client, path).Body.Close()

	if _, err := client.Get(ts.URL + path + "/credits"); err != nil {
		t.Fatalf("GET crédits: %v", err)
	}

	// Le client refuse de suivre la redirection vers Stripe : seule
	// l'URL de retour transmise nous intéresse.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.PostForm(ts.URL+path+"/checkout", url.Values{
		"pack":       {"0"},
		"csrf_token": {csrfFrom(t, client, ts.URL)},
	})
	if err != nil {
		t.Fatalf("POST checkout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("redirection vers Stripe attendue, obtenu %d", resp.StatusCode)
	}

	successURL := form.Get("success_url")
	if successURL == "" {
		t.Fatal("aucune URL de retour transmise à Stripe")
	}
	if strings.Contains(successURL, path) {
		t.Fatalf("le retour vise le lien d'origine, déjà consommé: %s", successURL)
	}

	// Un navigateur sans cookie — l'onglet rouvert, un autre appareil —
	// doit pouvoir consommer le lien de retour.
	jar, _ := cookiejar.New(nil)
	fresh := &http.Client{Jar: jar}
	returned, err := fresh.Get(successURL)
	if err != nil {
		t.Fatalf("GET retour de paiement: %v", err)
	}
	if returned.StatusCode != http.StatusOK {
		t.Fatalf("retour de paiement en %d, attendu 200", returned.StatusCode)
	}
	if page := body(t, returned); strings.Contains(page, "déjà servi") {
		t.Error("le retour de paiement affiche « ce lien a déjà servi »")
	}
}

// notifierEspion enregistre les confirmations d'achat demandées.
type notifierEspion struct {
	memberID       string
	credits, solde int64
	appels         int
	err            error
}

func (n *notifierEspion) NotifyPurchase(_ context.Context, memberID string, credits, balance int64) error {
	n.memberID, n.credits, n.solde = memberID, credits, balance
	n.appels++
	return n.err
}

// TestStripeWebhook_ConfirmsPurchaseToBuyer : l'écran de retour peut être
// fermé, le paiement fait sur un autre appareil, ou la banque prendre son
// temps. La confirmation part donc là où la conversation a lieu, et une
// seule fois — un événement rejoué ne doit pas la répéter.
func TestStripeWebhook_ConfirmsPurchaseToBuyer(t *testing.T) {
	server, ts, _ := testServer(t)
	server.cfg.Web.Stripe.WebhookSecret = testWebhookSecret
	server.stripe = newStripeClient("sk_test", "")

	espion := &notifierEspion{}
	server.WithPurchaseNotifier(espion)

	seedOrg(t, server, persistence.Organization{ID: "org-a", DisplayName: "Org A"}, 500)
	seedMember(t, server, persistence.Member{ID: "cam", OrgID: "org-a", DisplayName: "Camille", Role: "member"})

	payload := `{"type":"checkout.session.completed","data":{"object":{"id":"cs_test_9",` +
		`"payment_status":"paid","metadata":{"org_id":"org-a","member_id":"cam","credits":"1000","price_eur":"9"}}}}`

	envoyer := func() int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/stripe/webhook", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("requête webhook: %v", err)
		}
		req.Header.Set("Stripe-Signature", signedHeader(payload, time.Now(), testWebhookSecret))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST webhook: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := envoyer(); code != http.StatusOK {
		t.Fatalf("webhook en %d, attendu 200", code)
	}

	if espion.appels != 1 {
		t.Fatalf("%d confirmation(s) envoyée(s), attendu 1", espion.appels)
	}
	if espion.memberID != "cam" {
		t.Errorf("confirmation envoyée à %q, attendu cam", espion.memberID)
	}
	if espion.credits != 1000 {
		t.Errorf("crédits annoncés = %d, attendu 1000", espion.credits)
	}
	// Le solde annoncé est celui d'après l'achat : 500 offerts + 1000
	// achetés.
	if espion.solde != 1500 {
		t.Errorf("solde annoncé = %d, attendu 1500", espion.solde)
	}

	if code := envoyer(); code != http.StatusOK {
		t.Fatalf("rejeu du webhook en %d, attendu 200", code)
	}
	if espion.appels != 1 {
		t.Errorf("l'événement rejoué a renvoyé une confirmation (%d appels)", espion.appels)
	}
}
