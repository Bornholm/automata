package core

import (
	"crypto/subtle"
	"github.com/bornholm/automata/internal/weblink"
	"net/http"
)

// La protection CSRF suit le schéma du double cookie : un jeton aléatoire
// posé en cookie (lisible du seul site, SameSite Lax) et répété dans un
// champ caché de chaque formulaire. Un POST n'est accepté que si les deux
// coïncident — un site tiers peut déclencher la requête mais ne peut ni
// lire le cookie ni forger le champ.

// EnsureCSRFCookie retourne le jeton CSRF courant, en le créant au besoin.
func EnsureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie(CSRFCookieName); err == nil && len(cookie.Value) >= 20 {
		return cookie.Value, nil
	}

	token, err := weblink.RandomCrockford(26)
	if err != nil {
		return "", err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	return token, nil
}

// CheckCSRF vérifie qu'un POST porte le jeton du cookie (champ de
// formulaire csrf_token, ou en-tête X-CSRF-Token pour les requêtes htmx).
func CheckCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(CSRFCookieName)
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

// csrfToken retourne le jeton CSRF courant, en le posant au besoin.
func (s *Deps) CSRFToken(w http.ResponseWriter, r *http.Request) string {
	token, err := EnsureCSRFCookie(w, r)
	if err != nil {
		s.Logger.ErrorContext(r.Context(), "web: échec de la création du jeton CSRF", "error", err)
	}
	return token
}

// PluginUIEndpoint est la part du gestionnaire de plugins dont le proxy a
// besoin. Le port et le jeton sont relus à CHAQUE requête : ils changent à
// chaque redémarrage du plugin.
type PluginUIEndpoint interface {
	UIEndpoint(name string) (port uint32, token string, ok bool)
}
