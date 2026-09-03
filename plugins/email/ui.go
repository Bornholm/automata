package main

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// L'interface de configuration, rendue dans l'iframe du profil (et de
// l'admin). Discipline des secrets : le mot de passe n'est JAMAIS relu —
// seul un booléen « défini » traverse, le champ vide laisse le secret
// intact.

var uiTemplate = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;font-size:14px;color:#161c27;margin:0;padding:16px;background:#fff}
label{display:block;margin-top:10px;font-weight:600;font-size:13px}
input[type=text],input[type=password],input[type=number]{width:100%;box-sizing:border-box;height:34px;padding:0 10px;margin-top:4px;border:1px solid #d8dce4;border-radius:8px;font-size:14px}
.row{display:flex;gap:10px}.row>div{flex:1}
.check{display:flex;align-items:center;gap:8px;margin-top:12px;font-weight:600;font-size:13px}
.hint{font-weight:400;color:#8a93a5;font-size:12px;margin-top:2px}
button{margin-top:16px;height:36px;padding:0 16px;border:0;border-radius:8px;background:#5b5bd6;color:#fff;font-weight:600;font-size:14px;cursor:pointer}
.flash{margin-top:12px;padding:8px 12px;border-radius:8px;background:#e7f4ef;color:#17795e;font-size:13px}
.error{background:#fce9e7;color:#b42318}
.notice{margin-top:14px;padding:8px 12px;border-radius:8px;background:#f5f5fe;color:#3f3fa8;font-size:12px;line-height:1.5}
.google{display:flex;align-items:center;gap:12px;justify-content:space-between;margin-top:6px;padding:12px 14px;border:1px solid #d8dce4;border-radius:10px}
.google.connected{border-color:#bfe3d4;background:#f2faf7}
.google button{margin-top:0}
button.ghost{background:#fff;color:#161c27;border:1px solid #d8dce4}
</style></head><body>
{{if .Saved}}<div class="flash">Configuration enregistrée.</div>{{end}}
{{if .Tested}}<div class="flash {{if not .TestOK}}error{{end}}">{{.TestMessage}}</div>{{end}}
{{if .Cert}}
<div class="notice warn">
	<strong>Le serveur {{.CertProtocol}} présente un certificat que la vérification refuse.</strong>
	<div class="hint" style="margin-top:6px">{{.Cert.VerifyError}}</div>
	<div style="margin-top:8px"><span class="hint">Sujet</span> {{.Cert.Subject}}</div>
	<div><span class="hint">Émetteur</span> {{.Cert.Issuer}}{{if .Cert.SelfSigned}} (auto-signé){{end}}</div>
	<div><span class="hint">Valide jusqu'au</span> {{.CertExpiry}}</div>
	<div style="margin-top:6px"><span class="hint">Empreinte SHA-256</span><br><code style="font-size:11px;word-break:break-all">{{.CertFingerprint}}</code></div>
	<div style="margin-top:8px">Comparez cette empreinte à celle de votre serveur avant d'accepter. L'exception ne vaudra que pour <em>ce</em> certificat, sur <em>ce</em> serveur : un autre restera refusé.</div>
	<form method="post" action="{{.Base}}accept-certificate" style="margin:0">
		<input type="hidden" name="protocol" value="{{.CertProtocol}}">
		<input type="hidden" name="fingerprint" value="{{.Cert.Fingerprint}}">
		<button type="submit">Accepter ce certificat</button>
	</form>
</div>
{{end}}
{{if .Exceptions}}
<div class="notice">
	Exception de certificat enregistrée pour : {{range $i, $p := .Exceptions}}{{if $i}}, {{end}}{{$p}}{{end}}.
	{{range .Exceptions}}
	<form method="post" action="{{$.Base}}accept-certificate" style="margin:0;display:inline-block">
		<input type="hidden" name="protocol" value="{{.}}">
		<input type="hidden" name="fingerprint" value="">
		<button type="submit" style="background:#fff;color:#161c27;border:1px solid #d8dce4">Retirer l'exception {{.}}</button>
	</form>
	{{end}}
</div>
{{end}}
{{if .OAuthMessage}}<div class="flash {{if .OAuthError}}error{{end}}">{{.OAuthMessage}}</div>{{end}}
{{if .GoogleConnected}}
<div class="google connected">
	<div><strong>Compte Google connecté</strong><div class="hint">{{.Cfg.Username}} — les serveurs Gmail sont réglés automatiquement, aucun mot de passe à saisir.</div></div>
	<form method="post" action="{{.Base}}disconnect"><button type="submit" class="ghost">Déconnecter</button></form>
</div>
<form method="post" action="{{.Base}}save">
	<input type="hidden" name="imap_host" value="{{.Cfg.IMAPHost}}"><input type="hidden" name="imap_port" value="{{.Cfg.IMAPPort}}">
	<input type="hidden" name="smtp_host" value="{{.Cfg.SMTPHost}}"><input type="hidden" name="smtp_port" value="{{.Cfg.SMTPPort}}">
	<input type="hidden" name="username" value="{{.Cfg.Username}}"><input type="hidden" name="from_address" value="{{.Cfg.From}}">
	<div class="check"><input type="checkbox" id="ar" name="allow_read" {{if .Cfg.AllowRead}}checked{{end}}><label for="ar" style="margin:0">L'agent peut lire mes courriels</label></div>
	<div class="check"><input type="checkbox" id="aw" name="allow_write" {{if .Cfg.AllowWrite}}checked{{end}}><label for="aw" style="margin:0">L'agent peut préparer des envois</label></div>
	<div class="notice">Aucun courriel ne part jamais sans votre accord : chaque envoi préparé par l'agent attend votre « confirmer » dans la conversation. Ce réglage décide seulement de ce que l'agent voit.</div>
	<label>Vos consignes<textarea name="instructions" rows="5" placeholder="Ignore les infolettres, sauf celle du syndicat.&#10;Préviens-moi tout de suite si Lina écrit.&#10;Ne me parle jamais des accusés de réception.">{{.Cfg.Instructions}}</textarea></label>
	<div class="hint">Écrivez-les comme vous les diriez. Elles s'appliquent à chaque courriel reçu et l'emportent sur le jugement de l'agent.</div>
	<label>Marquer les courriels traités<input type="text" name="processed_label" value="{{.Cfg.ProcessedLabel}}" placeholder="Automata"></label>
	<div class="hint">Le mot-clé posé sur un courriel dont l'agent s'est occupé. Vos courriels ne sont jamais marqués comme lus : cet état vous appartient.</div>
	<button type="submit">Enregistrer</button>
</form>
{{else}}
{{if .GoogleAvailable}}
<div class="google">
	<div><strong>Boîte Gmail</strong><div class="hint">Connectez votre compte Google : rien à saisir, et aucun mot de passe conservé.</div></div>
	<form method="post" action="{{.Base}}connect"><button type="submit">Connecter Gmail</button></form>
</div>
<p class="hint" style="margin-top:14px">Ou configurez un autre fournisseur ci-dessous.</p>
{{end}}
<form method="post" action="{{.Base}}save">
	<div class="row">
		<div><label>Serveur IMAP<input type="text" name="imap_host" value="{{.Cfg.IMAPHost}}" placeholder="imap.exemple.fr"></label></div>
		<div><label>Port<input type="number" name="imap_port" value="{{if .Cfg.IMAPPort}}{{.Cfg.IMAPPort}}{{end}}" placeholder="993"></label></div>
	</div>
	<div class="row">
		<div><label>Serveur SMTP<input type="text" name="smtp_host" value="{{.Cfg.SMTPHost}}" placeholder="smtp.exemple.fr"></label></div>
		<div><label>Port<input type="number" name="smtp_port" value="{{if .Cfg.SMTPPort}}{{.Cfg.SMTPPort}}{{end}}" placeholder="465"></label></div>
	</div>
	<label>Identifiant<input type="text" name="username" value="{{.Cfg.Username}}" placeholder="vous@exemple.fr"></label>
	<label>Adresse d'expédition<input type="text" name="from_address" value="{{.Cfg.From}}" placeholder="vous@exemple.fr"></label>
	<label>Mot de passe<input type="password" name="password" placeholder="{{if .HasPassword}}••••••••{{else}}mot de passe du compte{{end}}">
	<div class="hint">{{if .HasPassword}}Défini. Laissez vide pour le conserver.{{else}}Requis pour lire et envoyer.{{end}}</div></label>
	<div class="check"><input type="checkbox" id="ar" name="allow_read" {{if .Cfg.AllowRead}}checked{{end}}><label for="ar" style="margin:0">L'agent peut lire mes courriels</label></div>
	<div class="check"><input type="checkbox" id="aw" name="allow_write" {{if .Cfg.AllowWrite}}checked{{end}}><label for="aw" style="margin:0">L'agent peut préparer des envois</label></div>
	<div class="notice">Aucun courriel ne part jamais sans votre accord : chaque envoi préparé par l'agent attend votre « confirmer » dans la conversation. Ce réglage décide seulement de ce que l'agent voit.</div>
	<label>Vos consignes<textarea name="instructions" rows="5" placeholder="Ignore les infolettres, sauf celle du syndicat.&#10;Préviens-moi tout de suite si Lina écrit.&#10;Ne me parle jamais des accusés de réception.">{{.Cfg.Instructions}}</textarea></label>
	<div class="hint">Écrivez-les comme vous les diriez. Elles s'appliquent à chaque courriel reçu et l'emportent sur le jugement de l'agent.</div>
	<label>Marquer les courriels traités<input type="text" name="processed_label" value="{{.Cfg.ProcessedLabel}}" placeholder="Automata"></label>
	<div class="hint">Le mot-clé posé sur un courriel dont l'agent s'est occupé. Vos courriels ne sont jamais marqués comme lus : cet état vous appartient.</div>
	<button type="submit">Enregistrer</button>
	<button type="submit" formaction="{{.Base}}test" style="background:#fff;color:#161c27;border:1px solid #d8dce4;margin-left:8px">Tester la connexion</button>
</form>
{{end}}
</body></html>`))

type uiData struct {
	Base         string
	MemberScoped bool
	Cfg          memberConfig
	HasPassword  bool
	Saved        bool
	Tested       bool
	TestOK       bool
	TestMessage  string
	// Cert est le certificat que le serveur présente, montré après un
	// échec de vérification pour que la personne décide en connaissance de
	// cause. Nil le reste du temps.
	Cert            *pluginsdk.CertificateInfo
	CertProtocol    string
	CertFingerprint string
	CertExpiry      string
	// Exceptions liste les protocoles pour lesquels une exception est
	// enregistrée ("imap", "smtp").
	Exceptions []string

	// OAuth : état de la connexion Google.
	GoogleAvailable bool
	GoogleConnected bool
	OAuthMessage    string
	OAuthError      bool
}

func newUIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleUIRoot)
	mux.HandleFunc("POST /save", handleUISave)
	mux.HandleFunc("POST /test", handleUITest)
	mux.HandleFunc("POST /accept-certificate", handleUIAcceptCertificate)
	// Connexion Google : consentement, retour, déconnexion.
	mux.HandleFunc("POST /connect", handleConnect)
	mux.HandleFunc("POST /disconnect", handleDisconnect)
	mux.HandleFunc("POST /app", handleAdminSaveApp)
	// Le retour de Google arrive par la route publique de l'hôte, sans
	// contexte de membre : c'est le state signé qui désigne le compte.
	mux.HandleFunc("GET /oauth/callback", handleOAuthCallback)
	return mux
}

// handleUIRoot aiguille : l'administrateur configure l'application
// Google, le membre configure SA boîte.
func handleUIRoot(w http.ResponseWriter, r *http.Request) {
	if pluginsdk.MemberID(r) == "" {
		handleAdminHome(w, r)
		return
	}
	handleUIHome(w, r)
}

func loadUIData(r *http.Request) uiData {
	data := uiData{
		Base:         pluginsdk.BasePath(r),
		MemberScoped: pluginsdk.MemberID(r) != "",
	}
	if !data.MemberScoped {
		return data
	}

	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		if cfg, err := parseConfig(raw); err == nil {
			data.Cfg = cfg
		}
	}
	// Seul le booléen traverse : jamais la valeur.
	if _, found, err := host.GetSecret(r.Context(), orgID, memberID, secretKeyPassword); err == nil && found {
		data.HasPassword = true
	}

	if data.Cfg.IMAPTLSFingerprint != "" {
		data.Exceptions = append(data.Exceptions, "imap")
	}
	if data.Cfg.SMTPTLSFingerprint != "" {
		data.Exceptions = append(data.Exceptions, "smtp")
	}

	// La connexion Google n'est proposée que si l'organisation a
	// enregistré une application.
	if _, err := loadOAuthApp(r.Context(), host, orgID); err == nil {
		data.GoogleAvailable = true
	}
	if _, found, err := host.GetSecret(r.Context(), orgID, memberID, secretKeyRefreshToken); err == nil && found {
		data.GoogleConnected = data.Cfg.oauth()
	}

	return data
}

func handleUIHome(w http.ResponseWriter, r *http.Request) {
	data := loadUIData(r)
	data.Saved = r.URL.Query().Get("saved") == "1"
	switch r.URL.Query().Get("oauth") {
	case "noapp":
		data.OAuthMessage, data.OAuthError = "Aucune application Google n'est enregistrée pour votre organisation : demandez à l'exploitant de la configurer.", true
	case "disconnected":
		data.OAuthMessage = "Compte Google déconnecté."
	}
	if msg := r.URL.Query().Get("tested"); msg != "" {
		data.Tested = true
		data.TestOK = msg == "ok"
		if data.TestOK {
			data.TestMessage = "Connexion réussie, en lecture comme en envoi."
		} else {
			// La cause telle qu'elle remonte du serveur, et pas une phrase
			// passe-partout : « vérifiez le mot de passe » ne dit rien
			// quand le vrai motif est un certificat auto-signé.
			data.TestMessage = "Connexion impossible : " + testFailureCause(r)
		}
	}

	// Après un échec de certificat, on montre ce que le serveur présente.
	// L'inspection ne fait rien confiance : elle regarde.
	if protocol := r.URL.Query().Get("cert"); protocol != "" && data.Tested && !data.TestOK {
		host, port := data.Cfg.IMAPHost, data.Cfg.IMAPPort
		if protocol == "smtp" {
			host, port = data.Cfg.SMTPHost, data.Cfg.SMTPPort
		}
		if info, ok := inspectServerCertificate(r.Context(), protocol, host, port); ok {
			data.Cert = &info
			data.CertProtocol = protocol
			data.CertFingerprint = info.FormattedFingerprint()
			data.CertExpiry = info.NotAfter.Local().Format("02/01/2006 15:04")
		}
	}

	_ = uiTemplate.Execute(w, data)
}

// formConfig relit la configuration existante et applique le formulaire.
func formConfig(r *http.Request) (memberConfig, string, string, string) {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	var cfg memberConfig
	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		cfg, _ = parseConfig(raw)
	}

	cfg.IMAPHost = r.FormValue("imap_host")
	cfg.IMAPPort, _ = strconv.Atoi(r.FormValue("imap_port"))
	cfg.SMTPHost = r.FormValue("smtp_host")
	cfg.SMTPPort, _ = strconv.Atoi(r.FormValue("smtp_port"))
	cfg.Username = r.FormValue("username")
	cfg.From = r.FormValue("from_address")
	cfg.AllowRead = r.FormValue("allow_read") != ""
	cfg.AllowWrite = r.FormValue("allow_write") != ""
	cfg.Instructions = strings.TrimSpace(r.FormValue("instructions"))
	cfg.ProcessedLabel = strings.TrimSpace(r.FormValue("processed_label"))

	return cfg, orgID, memberID, r.FormValue("password")
}

func handleUISave(w http.ResponseWriter, r *http.Request) {
	if pluginsdk.MemberID(r) == "" {
		http.Error(w, "member context required", http.StatusForbidden)
		return
	}
	host := pluginsdk.HostClientFromContext(r.Context())

	cfg, orgID, memberID, password := formConfig(r)

	if err := host.SaveConfig(r.Context(), orgID, memberID, cfg.marshal()); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	// Champ vide = secret intact.
	if password != "" {
		if err := host.SetSecret(r.Context(), orgID, memberID, secretKeyPassword, password); err != nil {
			http.Error(w, "secret save failed", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?saved=1", http.StatusSeeOther)
}

func handleUITest(w http.ResponseWriter, r *http.Request) {
	if pluginsdk.MemberID(r) == "" {
		http.Error(w, "member context required", http.StatusForbidden)
		return
	}
	host := pluginsdk.HostClientFromContext(r.Context())

	cfg, orgID, memberID, password := formConfig(r)
	if password == "" {
		if stored, found, err := host.GetSecret(r.Context(), orgID, memberID, secretKeyPassword); err == nil && found {
			password = stored
		}
	}

	if !cfg.complete() || password == "" {
		redirectTested(w, r, false, "renseignez le serveur, l'identifiant et le mot de passe.", "")
		return
	}

	client, err := dialIMAP(cfg, password)
	if err != nil {
		certificate := ""
		if isCertificateError(err) {
			certificate = "imap"
		}
		redirectTested(w, r, false, err.Error(), certificate)
		return
	}
	_ = client.Close()

	// L'envoi est éprouvé aussi : les deux serveurs peuvent être deux
	// machines, avec deux certificats, et un envoi qui échoue ne se
	// découvrait qu'au moment de confirmer un courriel.
	if err := dialSMTP(cfg, password); err != nil {
		certificate := ""
		if isCertificateError(err) {
			certificate = "smtp"
		}
		redirectTested(w, r, false, err.Error(), certificate)
		return
	}

	redirectTested(w, r, true, "", "")
}

// redirectTested renvoie vers la page avec le résultat du test. La cause
// voyage dans l'URL : elle est destinée à être lue, pas devinée.
func redirectTested(w http.ResponseWriter, r *http.Request, ok bool, cause, certificateProtocol string) {
	params := url.Values{}
	if ok {
		params.Set("tested", "ok")
	} else {
		params.Set("tested", "ko")
		if cause != "" {
			params.Set("cause", cause)
		}
		if certificateProtocol != "" {
			params.Set("cert", certificateProtocol)
		}
	}

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?"+params.Encode(), http.StatusSeeOther)
}

// testFailureCause retourne la cause portée par la redirection, bornée
// pour ne pas déverser une page entière dans l'interface.
func testFailureCause(r *http.Request) string {
	cause := strings.TrimSpace(r.URL.Query().Get("cause"))
	if cause == "" {
		return "vérifiez le serveur, l'identifiant et le mot de passe."
	}
	if len(cause) > maxCauseChars {
		cause = cause[:maxCauseChars] + "…"
	}
	return cause
}

// maxCauseChars borne le message affiché. Une erreur de bibliothèque peut
// être très longue ; l'essentiel est en tête.
const maxCauseChars = 400

// handleUIAcceptCertificate enregistre — ou retire — une exception TLS du
// membre, pour l'un des deux serveurs. Une empreinte vide retire
// l'exception ; toute autre valeur doit être une empreinte SHA-256, sans
// quoi rien n'est écrit.
//
// L'empreinte vient du formulaire, donc du navigateur : elle est revalidée
// ici, et n'est de toute façon qu'une CIBLE d'épinglage — la poser sur une
// valeur fantaisiste ne fait rien accepter, elle fait échouer la connexion.
func handleUIAcceptCertificate(w http.ResponseWriter, r *http.Request) {
	if pluginsdk.MemberID(r) == "" {
		http.Error(w, "member context required", http.StatusForbidden)
		return
	}

	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	var cfg memberConfig
	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		cfg, _ = parseConfig(raw)
	}

	raw := strings.TrimSpace(r.FormValue("fingerprint"))
	fingerprint := pluginsdk.NormalizeTLSFingerprint(raw)
	if raw != "" && fingerprint == "" {
		redirectTested(w, r, false, "empreinte de certificat illisible.", "")
		return
	}

	protocol := r.FormValue("protocol")
	switch protocol {
	case "imap":
		cfg.IMAPTLSFingerprint = fingerprint
	case "smtp":
		cfg.SMTPTLSFingerprint = fingerprint
	default:
		http.Error(w, "unknown protocol", http.StatusBadRequest)
		return
	}

	if err := host.SaveConfig(r.Context(), orgID, memberID, cfg.marshal()); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	slog.WarnContext(r.Context(), "email: exception de certificat modifiée",
		"protocol", protocol,
		"accepted", fingerprint != "")

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?saved=1", http.StatusSeeOther)
}
