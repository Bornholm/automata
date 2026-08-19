package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
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

	checkoutURL, err := client.checkoutSession(context.Background(), "acme", 4400, 35, "https://exemple/ok", "https://exemple/ko")
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
}
