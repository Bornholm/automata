package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
