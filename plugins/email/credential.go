package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// credential résout de quoi s'authentifier : le mot de passe applicatif,
// ou un jeton d'accès Google valide — rafraîchi au besoin, et le nouveau
// jeton persisté pour les appels suivants.
func credential(ctx context.Context, host pluginsdk.HostClient, orgID, memberID string, cfg memberConfig) (string, error) {
	if !cfg.oauth() {
		password, found, err := host.GetSecret(ctx, orgID, memberID, secretKeyPassword)
		if err != nil || !found {
			return "", fmt.Errorf("the mailbox password is not set")
		}
		return password, nil
	}

	return accessToken(ctx, host, orgID, memberID)
}

// accessToken returns a live Google access token, refreshing it when it is
// about to expire. Refreshes are serialised so two concurrent mailbox
// operations do not each spend a round-trip.
func accessToken(ctx context.Context, host pluginsdk.HostClient, orgID, memberID string) (string, error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	token, hasToken, err := host.GetSecret(ctx, orgID, memberID, secretKeyAccessToken)
	if err != nil {
		return "", fmt.Errorf("token lookup failed")
	}
	if hasToken && !expiringSoon(ctx, host, orgID, memberID) {
		return token, nil
	}

	refreshToken, found, err := host.GetSecret(ctx, orgID, memberID, secretKeyRefreshToken)
	if err != nil || !found {
		return "", fmt.Errorf("the Google account is not connected")
	}

	app, err := loadOAuthApp(ctx, host, orgID)
	if err != nil {
		return "", err
	}

	renewed, err := refreshAccessToken(ctx, app, refreshToken)
	if err != nil {
		// Un refus ici est durable (consentement révoqué, application
		// supprimée) : le message doit inviter à reconnecter, pas laisser
		// croire à une panne passagère.
		return "", fmt.Errorf("Google refused to renew the mailbox access (reconnect the account)")
	}

	if err := storeAccessToken(ctx, host, orgID, memberID, renewed); err != nil {
		return "", err
	}
	// Google ne renvoie un refresh_token qu'à la première autorisation ;
	// s'il en fournit un nouveau, il remplace l'ancien.
	if renewed.RefreshToken != "" {
		_ = host.SetSecret(ctx, orgID, memberID, secretKeyRefreshToken, renewed.RefreshToken)
	}

	return renewed.AccessToken, nil
}

// expiringSoon reports whether the stored access token is past its useful
// life. An unreadable or missing expiry is treated as expired.
func expiringSoon(ctx context.Context, host pluginsdk.HostClient, orgID, memberID string) bool {
	raw, found, err := host.GetSecret(ctx, orgID, memberID, secretKeyExpiry)
	if err != nil || !found {
		return true
	}
	unix, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return true
	}
	return time.Now().Add(accessTokenRefreshMargin).After(time.Unix(unix, 0))
}

// storeAccessToken persists a freshly obtained access token and its expiry.
func storeAccessToken(ctx context.Context, host pluginsdk.HostClient, orgID, memberID string, token tokenResponse) error {
	if err := host.SetSecret(ctx, orgID, memberID, secretKeyAccessToken, token.AccessToken); err != nil {
		return fmt.Errorf("token storage failed")
	}
	expiry := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	if err := host.SetSecret(ctx, orgID, memberID, secretKeyExpiry, strconv.FormatInt(expiry.Unix(), 10)); err != nil {
		return fmt.Errorf("token storage failed")
	}
	return nil
}

// loadOAuthApp reads the Google application credentials, stored once by an
// administrator at organization scope (member_id empty).
func loadOAuthApp(ctx context.Context, host pluginsdk.HostClient, orgID string) (oauthApp, error) {
	raw, found, err := host.GetConfig(ctx, orgID, "")
	if err != nil || !found {
		return oauthApp{}, fmt.Errorf("no Google application configured for this organization")
	}

	var app oauthApp
	if err := unmarshalJSON(raw, &app); err != nil {
		return oauthApp{}, fmt.Errorf("the Google application configuration is unreadable")
	}

	secret, found, err := host.GetSecret(ctx, orgID, "", secretKeyClientSecret)
	if err != nil || !found {
		return oauthApp{}, fmt.Errorf("the Google application secret is not set")
	}
	app.ClientSecret = secret

	if !app.configured() {
		return oauthApp{}, fmt.Errorf("the Google application is incomplete")
	}
	return app, nil
}

// memberStateSeed returns the per-member seed signing OAuth states,
// creating it on first use.
func memberStateSeed(ctx context.Context, host pluginsdk.HostClient, orgID, memberID string) (string, error) {
	seed, found, err := host.GetSecret(ctx, orgID, memberID, secretKeyStateSeed)
	if err != nil {
		return "", fmt.Errorf("state seed lookup failed")
	}
	if found && seed != "" {
		return seed, nil
	}

	seed, err = randomToken()
	if err != nil {
		return "", fmt.Errorf("state seed generation failed")
	}
	if err := host.SetSecret(ctx, orgID, memberID, secretKeyStateSeed, seed); err != nil {
		return "", fmt.Errorf("state seed storage failed")
	}
	return seed, nil
}
