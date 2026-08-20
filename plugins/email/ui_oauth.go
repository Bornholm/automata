package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// Écrans OAuth : l'administrateur enregistre l'application Google une fois
// pour l'organisation, le membre connecte sa boîte d'un clic. Le retour de
// Google arrive sur une route publique de l'hôte
// (/plugins/email/oauth/callback) : elle ne porte aucune identité, c'est
// le paramètre state signé qui désigne le membre.

var adminTemplate = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;font-size:14px;color:#161c27;margin:0;padding:16px;background:#fff}
label{display:block;margin-top:10px;font-weight:600;font-size:13px}
input{width:100%;box-sizing:border-box;height:34px;padding:0 10px;margin-top:4px;border:1px solid #d8dce4;border-radius:8px;font-size:14px}
button{margin-top:16px;height:36px;padding:0 16px;border:0;border-radius:8px;background:#5b5bd6;color:#fff;font-weight:600;font-size:14px;cursor:pointer}
.hint{font-weight:400;color:#8a93a5;font-size:12px;margin-top:4px;line-height:1.5}
.flash{margin-top:12px;padding:8px 12px;border-radius:8px;background:#e7f4ef;color:#17795e;font-size:13px}
code{background:#f5f5fe;padding:2px 6px;border-radius:5px;font-size:12px}
</style></head><body>
{{if .Saved}}<div class="flash">Application enregistrée.</div>{{end}}
<p class="hint">Pour que les membres connectent leur boîte Gmail d'un clic, enregistrez ici une application Google (console Google Cloud → identifiants OAuth, type « Application Web »).</p>
<p class="hint">URI de redirection autorisée à déclarer chez Google :<br><code>{{.RedirectURI}}</code></p>
<form method="post" action="{{.Base}}app">
	<label>ID client<input type="text" name="google_client_id" value="{{.App.ClientID}}" placeholder="…apps.googleusercontent.com"></label>
	<label>Secret client<input type="password" name="google_client_secret" placeholder="{{if .HasSecret}}••••••••{{else}}secret de l'application{{end}}">
	<div class="hint">{{if .HasSecret}}Défini. Laissez vide pour le conserver.{{else}}Requis.{{end}}</div></label>
	<button type="submit">Enregistrer</button>
</form>
</body></html>`))

type adminData struct {
	Base        string
	RedirectURI string
	App         oauthApp
	HasSecret   bool
	Saved       bool
}

// redirectURI composes the stable public callback of the instance.
func redirectURI(r *http.Request) string {
	base := pluginsdk.BaseURL(r)
	if base == "" {
		return "(base_url de l'instance non configurée)"
	}
	return base + "/plugins/email/oauth/callback"
}

func handleAdminHome(w http.ResponseWriter, r *http.Request) {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID := pluginsdk.OrgID(r)

	data := adminData{
		Base:        pluginsdk.BasePath(r),
		RedirectURI: redirectURI(r),
		Saved:       r.URL.Query().Get("saved") == "1",
	}

	if raw, found, err := host.GetConfig(r.Context(), orgID, ""); err == nil && found {
		_ = json.Unmarshal([]byte(raw), &data.App)
	}
	if _, found, err := host.GetSecret(r.Context(), orgID, "", secretKeyClientSecret); err == nil && found {
		data.HasSecret = true
	}

	_ = adminTemplate.Execute(w, data)
}

func handleAdminSaveApp(w http.ResponseWriter, r *http.Request) {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID := pluginsdk.OrgID(r)
	if orgID == "" {
		http.Error(w, "organization context required", http.StatusForbidden)
		return
	}

	app := oauthApp{ClientID: r.FormValue("google_client_id")}
	raw, _ := json.Marshal(app)
	if err := host.SaveConfig(r.Context(), orgID, "", string(raw)); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	// Champ vide = secret conservé (même discipline que le mot de passe).
	if secret := r.FormValue("google_client_secret"); secret != "" {
		if err := host.SetSecret(r.Context(), orgID, "", secretKeyClientSecret, secret); err != nil {
			http.Error(w, "secret save failed", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?saved=1", http.StatusSeeOther)
}

// handleConnect starts the consent flow for the current member.
func handleConnect(w http.ResponseWriter, r *http.Request) {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)
	if memberID == "" {
		http.Error(w, "member context required", http.StatusForbidden)
		return
	}

	app, err := loadOAuthApp(r.Context(), host, orgID)
	if err != nil {
		http.Redirect(w, r, pluginsdk.BasePath(r)+"?oauth=noapp", http.StatusSeeOther)
		return
	}

	seed, err := memberStateSeed(r.Context(), host, orgID, memberID)
	if err != nil {
		http.Error(w, "state seed failed", http.StatusInternalServerError)
		return
	}

	nonce, err := randomToken()
	if err != nil {
		http.Error(w, "nonce failed", http.StatusInternalServerError)
		return
	}

	state, err := signState(seed, oauthState{
		OrgID: orgID, MemberID: memberID, Nonce: nonce, IssuedAt: time.Now().Unix(),
	})
	if err != nil {
		http.Error(w, "state failed", http.StatusInternalServerError)
		return
	}

	var hint string
	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		if cfg, err := parseConfig(raw); err == nil {
			hint = cfg.Username
		}
	}

	http.Redirect(w, r, authorizationURL(app, redirectURI(r), state, hint), http.StatusSeeOther)
}

// handleOAuthCallback receives Google's redirect. The route carries no
// identity: the signed state designates the member, and its signature is
// checked with that member's own seed.
func handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	host := pluginsdk.HostClientFromContext(r.Context())

	if reason := r.URL.Query().Get("error"); reason != "" {
		writeCallbackPage(w, false, "Google a refusé l'autorisation ("+reason+").")
		return
	}

	code := r.URL.Query().Get("code")
	rawState := r.URL.Query().Get("state")
	if code == "" || rawState == "" {
		writeCallbackPage(w, false, "Réponse incomplète de Google.")
		return
	}

	st, encoded, sig, ok := parseState(rawState)
	if !ok || st.OrgID == "" || st.MemberID == "" {
		writeCallbackPage(w, false, "Paramètre de sécurité invalide.")
		return
	}

	seed, found, err := host.GetSecret(r.Context(), st.OrgID, st.MemberID, secretKeyStateSeed)
	if err != nil || !found || !verifyState(seed, encoded, sig, st, time.Now()) {
		writeCallbackPage(w, false, "Cette autorisation a expiré ou ne peut pas être vérifiée. Relancez la connexion depuis votre profil.")
		return
	}

	app, err := loadOAuthApp(r.Context(), host, st.OrgID)
	if err != nil {
		writeCallbackPage(w, false, err.Error())
		return
	}

	token, err := exchangeCode(r.Context(), app, code, redirectURI(r))
	if err != nil {
		writeCallbackPage(w, false, err.Error())
		return
	}
	if token.RefreshToken == "" {
		writeCallbackPage(w, false, "Google n'a pas fourni de jeton durable. Retirez l'accès d'Automata dans votre compte Google, puis reconnectez la boîte.")
		return
	}

	if err := host.SetSecret(r.Context(), st.OrgID, st.MemberID, secretKeyRefreshToken, token.RefreshToken); err != nil {
		writeCallbackPage(w, false, "Enregistrement du jeton impossible.")
		return
	}
	if err := storeAccessToken(r.Context(), host, st.OrgID, st.MemberID, token); err != nil {
		writeCallbackPage(w, false, "Enregistrement du jeton impossible.")
		return
	}

	// Bascule la configuration du membre en mode Google : serveurs Gmail
	// et adresse déduits, plus rien à saisir.
	var cfg memberConfig
	if raw, found, err := host.GetConfig(r.Context(), st.OrgID, st.MemberID); err == nil && found {
		cfg, _ = parseConfig(raw)
	}
	cfg = cfg.googleDefaults(cfg.Username)
	if err := host.SaveConfig(r.Context(), st.OrgID, st.MemberID, cfg.marshal()); err != nil {
		writeCallbackPage(w, false, "Enregistrement de la configuration impossible.")
		return
	}

	writeCallbackPage(w, true, "Votre boîte Gmail est connectée. Vous pouvez fermer cette page et revenir à votre profil.")
}

var callbackTemplate = template.Must(template.New("cb").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;margin:0;padding:40px 20px;background:#f7f8fa;color:#161c27;text-align:center}
.card{max-width:420px;margin:0 auto;background:#fff;border-radius:14px;padding:28px 24px;border:1px solid #e2e5eb}
h1{font-size:19px;margin:0 0 8px}
p{font-size:14px;line-height:1.6;color:#626b7d;margin:0}
.ok{color:#17795e}.ko{color:#b42318}
</style></head><body>
<div class="card">
<h1 class="{{if .OK}}ok{{else}}ko{{end}}">{{if .OK}}Boîte connectée{{else}}Connexion impossible{{end}}</h1>
<p>{{.Message}}</p>
</div></body></html>`))

func writeCallbackPage(w http.ResponseWriter, ok bool, message string) {
	status := http.StatusOK
	if !ok {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = callbackTemplate.Execute(w, struct {
		OK      bool
		Message string
	}{ok, message})
}

// handleDisconnect drops the Google tokens and returns to password mode.
func handleDisconnect(w http.ResponseWriter, r *http.Request) {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)
	if memberID == "" {
		http.Error(w, "member context required", http.StatusForbidden)
		return
	}

	for _, key := range []string{secretKeyRefreshToken, secretKeyAccessToken, secretKeyExpiry} {
		_ = host.DeleteSecret(r.Context(), orgID, memberID, key)
	}

	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		if cfg, err := parseConfig(raw); err == nil {
			cfg.AuthMode = authModePassword
			_ = host.SaveConfig(r.Context(), orgID, memberID, cfg.marshal())
		}
	}

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?oauth=disconnected", http.StatusSeeOther)
}

var _ = fmt.Sprintf
