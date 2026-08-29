package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client Stripe minimal, en REST direct : deux appels suffisent (créer une
// session Checkout, vérifier la signature d'un événement), et le SDK
// officiel pèserait bien plus lourd que ces quelques lignes dans le
// binaire d'un worker de messagerie.

const (
	stripeAPIBase = "https://api.stripe.com/v1"
	// stripeTolerance borne l'écart d'horodatage accepté sur une signature
	// d'événement : au-delà, une capture rejouée est refusée.
	stripeTolerance = 5 * time.Minute
	stripeTimeout   = 15 * time.Second
)

// StripeClient appelle l'API Stripe avec la clé secrète configurée.
type StripeClient struct {
	secretKey string
	// taxCode classe le produit vendu : Stripe le réclame dès que Stripe
	// Tax est activé sur le compte.
	taxCode string
	http    *http.Client
	// baseURL est surchargée par WithBaseURL (bancs de test, ou passerelle
	// interposée).
	baseURL string
}

// WithBaseURL remplace le point d'entrée de l'API Stripe. Retourne c pour
// permettre le chaînage.
func (c *StripeClient) WithBaseURL(url string) *StripeClient {
	c.baseURL = url
	return c
}

func NewStripeClient(secretKey, taxCode string) *StripeClient {
	return &StripeClient{
		secretKey: secretKey,
		taxCode:   taxCode,
		http:      &http.Client{Timeout: stripeTimeout},
		baseURL:   stripeAPIBase,
	}
}

// CheckoutSession crée une session de paiement pour un pack de crédits et
// retourne l'URL vers laquelle rediriger le navigateur. Le prix est passé
// en ligne (price_data) : les packs vivent dans la configuration
// d'Automata, pas dans un catalogue Stripe à tenir en double.
func (c *StripeClient) CheckoutSession(ctx context.Context, orgID, memberID string, credits int64, priceEUR float64, successURL, cancelURL string) (string, error) {
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", "eur")
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(int64(priceEUR*100+0.5), 10))
	form.Set("line_items[0][price_data][product_data][name]", FormatCreditsProduct(credits))
	if c.taxCode != "" {
		form.Set("line_items[0][price_data][product_data][tax_code]", c.taxCode)
	}
	// Les métadonnées reviennent dans l'événement : c'est ce qui permet de
	// créditer le bon portefeuille sans faire confiance au navigateur.
	form.Set("metadata[org_id]", orgID)
	// Le membre revient avec l'événement : c'est lui qui a payé, c'est à
	// lui que part la confirmation, sur sa conversation privée.
	form.Set("metadata[member_id]", memberID)
	form.Set("metadata[credits]", strconv.FormatInt(credits, 10))
	// Le prix accompagne les crédits : sans lui, l'achat entrerait au
	// portefeuille sans recette en face, et la marge de l'instance se
	// calculerait sur les seuls crédits offerts.
	form.Set("metadata[price_eur]", strconv.FormatFloat(priceEUR, 'f', -1, 64))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("web: requête Stripe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("web: appel Stripe: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("web: lecture de la réponse Stripe: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Le corps d'erreur Stripe ne contient pas de secret, mais il peut
		// être verbeux : on n'en garde que le début.
		return "", fmt.Errorf("web: Stripe a refusé la création de session (code %d): %.200s", resp.StatusCode, body)
	}

	var session struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return "", fmt.Errorf("web: réponse Stripe illisible: %w", err)
	}
	if session.URL == "" {
		return "", fmt.Errorf("web: session Stripe sans URL de paiement")
	}

	return session.URL, nil
}

// FormatCreditsProduct nomme le produit tel qu'il apparaîtra sur la page de
// paiement et la facture.
func FormatCreditsProduct(credits int64) string {
	return fmt.Sprintf("Automata — %d crédits", credits)
}

// StripeEvent est la part d'un événement Stripe qui nous intéresse.
type StripeEvent struct {
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID            string `json:"id"`
			PaymentStatus string `json:"payment_status"`
			Metadata      struct {
				OrgID    string `json:"org_id"`
				MemberID string `json:"member_id"`
				Credits  string `json:"credits"`
				PriceEUR string `json:"price_eur"`
			} `json:"metadata"`
		} `json:"object"`
	} `json:"data"`
}

// VerifyStripeSignature vérifie l'en-tête Stripe-Signature (schéma v1 :
// HMAC-SHA256 de « <timestamp>.<corps> » avec le secret du webhook) et
// retourne l'événement décodé.
func VerifyStripeSignature(payload []byte, header, secret string, now time.Time) (StripeEvent, error) {
	var timestamp string
	var signatures []string

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			signatures = append(signatures, value)
		}
	}

	if timestamp == "" || len(signatures) == 0 {
		return StripeEvent{}, fmt.Errorf("web: en-tête de signature Stripe incomplet")
	}

	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return StripeEvent{}, fmt.Errorf("web: horodatage de signature Stripe invalide")
	}
	if delta := now.Sub(time.Unix(unix, 0)); delta > stripeTolerance || delta < -stripeTolerance {
		return StripeEvent{}, fmt.Errorf("web: signature Stripe hors tolérance temporelle")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	valid := false
	for _, signature := range signatures {
		if hmac.Equal([]byte(signature), []byte(expected)) {
			valid = true
			break
		}
	}
	if !valid {
		return StripeEvent{}, fmt.Errorf("web: signature Stripe invalide")
	}

	var event StripeEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return StripeEvent{}, fmt.Errorf("web: événement Stripe illisible: %w", err)
	}

	return event, nil
}
