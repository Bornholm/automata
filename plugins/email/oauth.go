package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OAuth2 pour les boîtes Google. Trois secrets d'un compte connecté ainsi :
// le jeton de rafraîchissement (durable), le jeton d'accès (court) et sa
// date d'expiration. Le mot de passe applicatif reste possible : les deux
// modes cohabitent, le champ authMode de la configuration tranche.

const (
	authModePassword = "password"
	authModeOAuth    = "oauth"

	secretKeyRefreshToken = "oauth_refresh_token"
	secretKeyAccessToken  = "oauth_access_token"
	secretKeyExpiry       = "oauth_access_expiry"
	// secretKeyStateSeed signe les paramètres state : un secret par
	// membre, jamais partagé, jamais exposé.
	secretKeyStateSeed = "oauth_state_seed"

	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	// gmailScope couvre la lecture ET l'envoi via IMAP/SMTP.
	gmailScope = "https://mail.google.com/"

	googleIMAPHost = "imap.gmail.com"
	googleIMAPPort = 993
	googleSMTPHost = "smtp.gmail.com"
	googleSMTPPort = 587
)

// accessTokenRefreshMargin renouvelle un peu avant l'échéance : une
// requête partie avec un jeton expirant dans la seconde échouerait.
const accessTokenRefreshMargin = 2 * time.Minute

// oauthApp is the instance-level Google application (client id/secret),
// stored once at organization scope by the administrator. Members never
// see it.
type oauthApp struct {
	ClientID     string `json:"google_client_id"`
	ClientSecret string `json:"google_client_secret"`
}

func (a oauthApp) configured() bool { return a.ClientID != "" && a.ClientSecret != "" }

const secretKeyClientSecret = "google_client_secret"

// tokenResponse is Google's token endpoint payload.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

var oauthHTTP = &http.Client{Timeout: 20 * time.Second}

// googleTokenURLOverride redirige le point d'accès jeton vers un serveur
// d'essai ; vide en production.
var googleTokenURLOverride string

func tokenEndpoint() string {
	if googleTokenURLOverride != "" {
		return googleTokenURLOverride
	}
	return googleTokenURL
}

// exchangeCode trades an authorization code for tokens.
func exchangeCode(ctx context.Context, app oauthApp, code, redirectURI string) (tokenResponse, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {app.ClientID},
		"client_secret": {app.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	return postToken(ctx, form)
}

// refreshAccessToken renews a short-lived access token.
func refreshAccessToken(ctx context.Context, app oauthApp, refreshToken string) (tokenResponse, error) {
	form := url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {app.ClientID},
		"client_secret": {app.ClientSecret},
		"grant_type":    {"refresh_token"},
	}
	return postToken(ctx, form)
}

func postToken(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint unreachable")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token response unreadable")
	}

	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return tokenResponse{}, fmt.Errorf("token response malformed")
	}
	if token.Error != "" {
		// Le message de Google est repris tel quel : il nomme la cause
		// (invalid_grant, redirect_uri_mismatch…) sans jamais contenir de
		// secret.
		return tokenResponse{}, fmt.Errorf("google refused the request: %s", token.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, fmt.Errorf("google refused the request (status %d)", resp.StatusCode)
	}

	return token, nil
}

// authorizationURL builds the consent URL. access_type=offline plus
// prompt=consent is what makes Google return a refresh token — without
// them, a second authorization yields an access token only, and the
// mailbox silently stops working when it expires.
func authorizationURL(app oauthApp, redirectURI, state, loginHint string) string {
	params := url.Values{
		"client_id":     {app.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {gmailScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	if loginHint != "" {
		params.Set("login_hint", loginHint)
	}
	return googleAuthURL + "?" + params.Encode()
}

// oauthState carries the member the callback belongs to. It is signed with
// a per-member seed: the public callback route accepts no identity header,
// so the state IS the proof.
type oauthState struct {
	OrgID    string `json:"o"`
	MemberID string `json:"m"`
	Nonce    string `json:"n"`
	IssuedAt int64  `json:"t"`
}

// stateTTL bounds how long an authorization can stay in flight.
const stateTTL = 15 * time.Minute

func signState(seed string, st oauthState) (string, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)

	mac := hmac.New(sha256.New, []byte(seed))
	mac.Write([]byte(encoded))

	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// parseState decodes the state without verifying it: the seed to verify
// with is only known once the member is known. Verification is mandatory
// right after, through verifyState.
func parseState(raw string) (oauthState, string, string, bool) {
	encoded, sig, found := strings.Cut(raw, ".")
	if !found {
		return oauthState{}, "", "", false
	}

	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return oauthState{}, "", "", false
	}

	var st oauthState
	if err := json.Unmarshal(payload, &st); err != nil {
		return oauthState{}, "", "", false
	}
	return st, encoded, sig, true
}

func verifyState(seed, encoded, sig string, st oauthState, now time.Time) bool {
	mac := hmac.New(sha256.New, []byte(seed))
	mac.Write([]byte(encoded))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return false
	}
	issued := time.Unix(st.IssuedAt, 0)
	return now.Sub(issued) < stateTTL && now.Add(time.Minute).After(issued)
}

func randomToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// xoauth2 builds the SASL XOAUTH2 initial response.
func xoauth2(username, accessToken string) string {
	return fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", username, accessToken)
}

// refreshMu serialises token refreshes per member: two mailbox operations
// starting at once must not each burn a refresh round-trip.
var refreshMu sync.Mutex
