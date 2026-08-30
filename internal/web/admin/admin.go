package admin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// RequireAdmin protège les routes d'administration : session absente ou
// périmée → retour à ADM-00 avec l'état « session expirée » si un cookie
// existait. Vérifie aussi le jeton CSRF de tout POST.
func (h *Handlers) RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(core.AdminCookieName)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}

		if _, _, ok := h.Signer.ParseSession(cookie.Value, "admin", h.Now()); !ok {
			core.ClearSessionCookie(w, core.AdminCookieName)
			http.Redirect(w, r, "/admin/login?expired=1", http.StatusFound)
			return
		}

		// Les POST proxifiés vers l'interface d'un plugin sont exemptés :
		// le formulaire vit dans le document du plugin, qui ne peut pas
		// porter notre jeton (origine opaque de la sandbox). La
		// protection vient de la session, du jeton d'UI connu du seul
		// proxy, et de cette même sandbox qui interdit à un site tiers de
		// lire quoi que ce soit de la réponse.
		if r.Method == http.MethodPost && !isPluginUIPath(r.URL.Path) && !core.CheckCSRF(r) {
			http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

func (h *Handlers) HandleLoginForm(w http.ResponseWriter, r *http.Request) {
	page := view.LoginPage{CSRFToken: h.CSRFToken(w, r)}
	if r.URL.Query().Get("expired") == "1" {
		page.Notice = "Votre session a expiré. Reconnectez-vous pour reprendre là où vous en étiez."
	}
	h.Render(w, r, http.StatusOK, view.AdminLogin(page))
}

func (h *Handlers) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	now := h.Now()
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")

	if h.Limiter.Remaining(now) <= 0 {
		h.Render(w, r, http.StatusTooManyRequests, view.AdminLogin(view.LoginPage{
			Email:     email,
			Error:     "Trop de tentatives. Réessayez dans quelques minutes.",
			CSRFToken: h.CSRFToken(w, r),
		}))
		return
	}

	// La comparaison bcrypt s'exécute même si l'adresse ne correspond pas :
	// pas d'oracle temporel sur l'existence du compte.
	emailOK := strings.EqualFold(email, h.Cfg.Web.Admin.Email)
	passwordOK := core.CheckPassword(h.Cfg.Web.Admin.PasswordHash, password)

	if !emailOK || !passwordOK {
		remaining := h.Limiter.RecordFailure(now)
		// Volontairement sans l'adresse saisie : identifiants et compteurs
		// seulement dans les journaux.
		h.Logger.WarnContext(r.Context(), "web: échec de connexion opérateur", "remaining_attempts", remaining)

		message := "Ces identifiants ne correspondent pas."
		if remaining > 0 {
			message = fmt.Sprintf("Ces identifiants ne correspondent pas. Il vous reste %d tentatives.", remaining)
		}
		h.Render(w, r, http.StatusUnauthorized, view.AdminLogin(view.LoginPage{
			Email:     email,
			Error:     message,
			CSRFToken: h.CSRFToken(w, r),
		}))
		return
	}

	h.Limiter.Reset()

	expires := now.Add(core.AdminSessionTTL)
	core.SetSessionCookie(w, core.AdminCookieName, h.Signer.Sign(core.SessionPayload("admin", email, expires)), expires)
	h.Logger.InfoContext(r.Context(), "web: connexion opérateur réussie")

	http.Redirect(w, r, "/admin/dashboard", http.StatusFound)
}

func (h *Handlers) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}
	core.ClearSessionCookie(w, core.AdminCookieName)
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}
