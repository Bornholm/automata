package web

import (
	"crypto/subtle"
	"net/http"
)

// La protection CSRF suit le schéma du double cookie : un jeton aléatoire
// posé en cookie (lisible du seul site, SameSite Lax) et répété dans un
// champ caché de chaque formulaire. Un POST n'est accepté que si les deux
// coïncident — un site tiers peut déclencher la requête mais ne peut ni
// lire le cookie ni forger le champ.

// ensureCSRFCookie retourne le jeton CSRF courant, en le créant au besoin.
func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie(csrfCookieName); err == nil && len(cookie.Value) >= 20 {
		return cookie.Value, nil
	}

	token, err := randomCrockford(26)
	if err != nil {
		return "", err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	return token, nil
}

// checkCSRF vérifie qu'un POST porte le jeton du cookie (champ de
// formulaire csrf_token, ou en-tête X-CSRF-Token pour les requêtes htmx).
func checkCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	submitted := r.PostFormValue("csrf_token")
	if submitted == "" {
		submitted = r.Header.Get("X-CSRF-Token")
	}
	if submitted == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(submitted)) == 1
}
