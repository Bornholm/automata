package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"

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
</style></head><body>
{{if .Saved}}<div class="flash">Configuration enregistrée.</div>{{end}}
{{if .Tested}}<div class="flash {{if not .TestOK}}error{{end}}">{{.TestMessage}}</div>{{end}}
{{if not .MemberScoped}}
<p class="hint">La boîte mail se configure depuis la page de profil de chaque membre : ce panneau d'administration n'affiche aucune donnée personnelle.</p>
{{else}}
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
}

func newUIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleUIHome)
	mux.HandleFunc("POST /save", handleUISave)
	mux.HandleFunc("POST /test", handleUITest)
	return mux
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

	return data
}

func handleUIHome(w http.ResponseWriter, r *http.Request) {
	data := loadUIData(r)
	data.Saved = r.URL.Query().Get("saved") == "1"
	if msg := r.URL.Query().Get("tested"); msg != "" {
		data.Tested = true
		data.TestOK = msg == "ok"
		if data.TestOK {
			data.TestMessage = "Connexion IMAP réussie."
		} else {
			data.TestMessage = "Connexion impossible : vérifiez le serveur, l'identifiant et le mot de passe."
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

	result := "ko"
	if cfg.complete() && password != "" {
		if client, err := dialIMAP(cfg, password); err == nil {
			_ = client.Close()
			result = "ok"
		}
	}

	http.Redirect(w, r, fmt.Sprintf("%s?tested=%s", pluginsdk.BasePath(r), result), http.StatusSeeOther)
}
