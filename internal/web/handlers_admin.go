package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/web/view"
)

// requireAdmin protège les routes d'administration : session absente ou
// périmée → retour à ADM-00 avec l'état « session expirée » si un cookie
// existait. Vérifie aussi le jeton CSRF de tout POST.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminCookieName)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}

		if _, _, ok := s.signer.parseSession(cookie.Value, "admin", s.now()); !ok {
			clearSessionCookie(w, adminCookieName)
			http.Redirect(w, r, "/admin/login?expired=1", http.StatusFound)
			return
		}

		// Les POST proxifiés vers l'interface d'un plugin sont exemptés :
		// le formulaire vit dans le document du plugin, qui ne peut pas
		// porter notre jeton (origine opaque de la sandbox). La
		// protection vient de la session, du jeton d'UI connu du seul
		// proxy, et de cette même sandbox qui interdit à un site tiers de
		// lire quoi que ce soit de la réponse.
		if r.Method == http.MethodPost && !isPluginUIPath(r.URL.Path) && !checkCSRF(r) {
			http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// csrfToken retourne le jeton CSRF courant, en le posant au besoin.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	token, err := ensureCSRFCookie(w, r)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "web: échec de la création du jeton CSRF", "error", err)
	}
	return token
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	page := view.LoginPage{CSRFToken: s.csrfToken(w, r)}
	if r.URL.Query().Get("expired") == "1" {
		page.Notice = "Votre session a expiré. Reconnectez-vous pour reprendre là où vous en étiez."
	}
	s.render(w, r, http.StatusOK, view.AdminLogin(page))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !checkCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	now := s.now()
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")

	if s.limiter.remaining(now) <= 0 {
		s.render(w, r, http.StatusTooManyRequests, view.AdminLogin(view.LoginPage{
			Email:     email,
			Error:     "Trop de tentatives. Réessayez dans quelques minutes.",
			CSRFToken: s.csrfToken(w, r),
		}))
		return
	}

	// La comparaison bcrypt s'exécute même si l'adresse ne correspond pas :
	// pas d'oracle temporel sur l'existence du compte.
	emailOK := strings.EqualFold(email, s.cfg.Web.Admin.Email)
	passwordOK := checkPassword(s.cfg.Web.Admin.PasswordHash, password)

	if !emailOK || !passwordOK {
		remaining := s.limiter.recordFailure(now)
		// Volontairement sans l'adresse saisie : identifiants et compteurs
		// seulement dans les journaux.
		s.logger.WarnContext(r.Context(), "web: échec de connexion opérateur", "remaining_attempts", remaining)

		message := "Ces identifiants ne correspondent pas."
		if remaining > 0 {
			message = fmt.Sprintf("Ces identifiants ne correspondent pas. Il vous reste %d tentatives.", remaining)
		}
		s.render(w, r, http.StatusUnauthorized, view.AdminLogin(view.LoginPage{
			Email:     email,
			Error:     message,
			CSRFToken: s.csrfToken(w, r),
		}))
		return
	}

	s.limiter.reset()

	expires := now.Add(adminSessionTTL)
	setSessionCookie(w, adminCookieName, s.signer.sign(sessionPayload("admin", email, expires)), expires)
	s.logger.InfoContext(r.Context(), "web: connexion opérateur réussie")

	http.Redirect(w, r, "/admin/dashboard", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !checkCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}
	clearSessionCookie(w, adminCookieName)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}
