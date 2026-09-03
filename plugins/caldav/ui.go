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

// L'interface de configuration, rendue dans l'iframe du profil. Discipline
// des secrets : le mot de passe n'est JAMAIS relu — seul un booléen
// « défini » traverse, et le champ laissé vide conserve le secret.

var uiTemplate = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;font-size:14px;color:#161c27;margin:0;padding:16px;background:#fff}
label{display:block;margin-top:10px;font-weight:600;font-size:13px}
input[type=text],input[type=password],input[type=number],select{width:100%;box-sizing:border-box;height:34px;padding:0 10px;margin-top:4px;border:1px solid #d8dce4;border-radius:8px;font-size:14px;background:#fff}
.check{display:flex;align-items:center;gap:8px;margin-top:12px;font-weight:600;font-size:13px}
.hint{font-weight:400;color:#8a93a5;font-size:12px;margin-top:2px}
button{margin-top:16px;height:36px;padding:0 16px;border:0;border-radius:8px;background:#5b5bd6;color:#fff;font-weight:600;font-size:14px;cursor:pointer}
.flash{margin-top:12px;padding:8px 12px;border-radius:8px;background:#e7f4ef;color:#17795e;font-size:13px}
.error{background:#fce9e7;color:#b42318}
.notice{margin-top:14px;padding:8px 12px;border-radius:8px;background:#f5f5fe;color:#3f3fa8;font-size:12px;line-height:1.5}
.warn{background:#fff6e6;color:#8a5a00}
</style></head><body>
{{if not .MemberScoped}}
<p class="hint">Ce plugin se configure depuis le profil de chaque personne : c'est son agenda, ses identifiants.</p>
{{else}}
{{if .Saved}}<div class="flash">Configuration enregistrée.</div>{{end}}
{{if .Tested}}<div class="flash {{if not .TestOK}}error{{end}}">{{.TestMessage}}</div>{{end}}
{{if .Cert}}
<div class="notice warn">
	<strong>Le serveur présente un certificat que la vérification refuse.</strong>
	<div class="hint" style="margin-top:6px">{{.Cert.VerifyError}}</div>
	<dl style="margin:8px 0 0">
		<div><span class="hint">Sujet</span> {{.Cert.Subject}}</div>
		<div><span class="hint">Émetteur</span> {{.Cert.Issuer}}{{if .Cert.SelfSigned}} (auto-signé){{end}}</div>
		<div><span class="hint">Valide jusqu'au</span> {{.CertExpiry}}</div>
		<div style="margin-top:6px"><span class="hint">Empreinte SHA-256</span><br><code style="font-size:11px;word-break:break-all">{{.CertFingerprint}}</code></div>
	</dl>
	<div style="margin-top:8px">Comparez cette empreinte à celle de votre serveur avant d'accepter. L'exception ne vaudra que pour <em>ce</em> certificat : un autre restera refusé.</div>
	<form method="post" action="{{.Base}}accept-certificate" style="margin:0">
		<input type="hidden" name="fingerprint" value="{{.Cert.Fingerprint}}">
		<button type="submit">Accepter ce certificat</button>
	</form>
</div>
{{end}}
{{if .HasException}}
<div class="notice">
	Une exception de certificat est enregistrée pour ce serveur.
	<form method="post" action="{{.Base}}accept-certificate" style="margin:0">
		<input type="hidden" name="fingerprint" value="">
		<button type="submit" style="background:#fff;color:#161c27;border:1px solid #d8dce4">Retirer l'exception</button>
	</form>
</div>
{{end}}
<form method="post" action="{{.Base}}save">
	<label>Adresse du serveur CalDAV<input type="text" name="server_url" value="{{.Cfg.ServerURL}}" placeholder="https://exemple.fr/remote.php/dav">
	<div class="hint">Celle de votre fournisseur d'agenda : Nextcloud, Fastmail, iCloud, Radicale…</div></label>
	<label>Identifiant<input type="text" name="username" value="{{.Cfg.Username}}" placeholder="vous@exemple.fr"></label>
	<label>Mot de passe<input type="password" name="password" placeholder="{{if .HasPassword}}••••••••{{else}}mot de passe du compte{{end}}">
	<div class="hint">{{if .HasPassword}}Défini. Laissez vide pour le conserver.{{else}}Un mot de passe d'application vaut mieux que celui du compte.{{end}}</div></label>
	{{if .Calendars}}
	<label>Agenda
		<select name="calendar_path">
			{{range .Calendars}}<option value="{{.Path}}" {{if eq .Path $.Cfg.CalendarPath}}selected{{end}}>{{.Name}}</option>{{end}}
		</select>
	</label>
	{{else if .Cfg.CalendarName}}
	<label>Agenda<input type="text" value="{{.Cfg.CalendarName}}" disabled>
	<div class="hint">Testez la connexion pour choisir un autre agenda.</div></label>
	{{end}}
	<div class="check"><input type="checkbox" id="ar" name="allow_read" {{if .Cfg.AllowRead}}checked{{end}}><label for="ar" style="margin:0">L'assistant peut consulter mon agenda</label></div>
	<div class="check"><input type="checkbox" id="aw" name="allow_write" {{if .Cfg.AllowWrite}}checked{{end}}><label for="aw" style="margin:0">L'assistant peut préparer des événements</label></div>
	<div class="notice">Rien n'est écrit dans votre agenda sans votre accord : chaque création ou annulation préparée par l'assistant attend votre « confirmer » dans la conversation. Ce réglage décide seulement de ce que l'assistant voit.</div>
	<div class="check"><input type="checkbox" id="es" name="event_store" {{if .Cfg.EventStore}}checked{{end}}><label for="es" style="margin:0">Ranger mes rappels dans cet agenda</label></div>
	<div class="notice warn">Vos rappels apparaîtront alors sur tous vos appareils, et vous pourrez les déplacer depuis n'importe quel client. En contrepartie leur texte est conservé <strong>chez votre fournisseur d'agenda</strong>, en clair — alors qu'Automata le garde chiffré dans sa propre base. Décochez pour revenir au stockage interne ; les rappels déjà posés dans l'agenda y restent.</div>
	<button type="submit">Enregistrer</button>
	<button type="submit" formaction="{{.Base}}test" style="background:#fff;color:#161c27;border:1px solid #d8dce4;margin-left:8px">Tester la connexion</button>
</form>
{{end}}
</body></html>`))

// calendarChoice est un agenda proposé au choix.
type calendarChoice struct {
	Path string
	Name string
}

type uiData struct {
	Base         string
	MemberScoped bool
	Cfg          memberConfig
	HasPassword  bool
	Calendars    []calendarChoice
	Saved        bool
	Tested       bool
	TestOK       bool
	TestMessage  string
	// Cert est le certificat que le serveur présente, montré après un
	// échec de vérification pour que la personne décide en connaissance
	// de cause. Nil le reste du temps.
	Cert            *pluginsdk.CertificateInfo
	CertFingerprint string
	CertExpiry      string
	HasException    bool
}

func newUIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleUIRoot)
	mux.HandleFunc("POST /save", handleUISave)
	mux.HandleFunc("POST /test", handleUITest)
	mux.HandleFunc("POST /accept-certificate", handleUIAcceptCertificate)
	return mux
}

func handleUIRoot(w http.ResponseWriter, r *http.Request) {
	data := loadUIData(r)
	data.Saved = r.URL.Query().Get("saved") == "1"

	if msg := r.URL.Query().Get("tested"); msg != "" {
		data.Tested = true
		data.TestOK = msg == "ok"
		if data.TestOK {
			data.TestMessage = "Connexion réussie."
			data.Calendars = discoverChoices(r)
		} else {
			// La cause telle qu'elle remonte du serveur, et pas une phrase
			// passe-partout : « vérifiez l'adresse et le mot de passe » ne
			// dit rien quand le vrai motif est un certificat expiré.
			data.TestMessage = "Connexion impossible : " + testFailureCause(r)
		}
	}

	// Après un échec de certificat, on montre ce que le serveur présente.
	// L'inspection ne fait rien confiance : elle regarde.
	if data.Tested && !data.TestOK && r.URL.Query().Get("cert") == "1" {
		if info, ok := inspectServerCertificate(r.Context(), data.Cfg.ServerURL); ok {
			data.Cert = &info
			data.CertFingerprint = info.FormattedFingerprint()
			data.CertExpiry = info.NotAfter.Local().Format("02/01/2006 15:04")
		}
	}

	_ = uiTemplate.Execute(w, data)
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
	data.HasException = data.Cfg.TLSFingerprint != ""

	return data
}

// testFailureCause retourne la cause portée par la redirection, bornée
// pour ne pas déverser une page entière dans l'interface.
func testFailureCause(r *http.Request) string {
	cause := strings.TrimSpace(r.URL.Query().Get("cause"))
	if cause == "" {
		return "vérifiez l'adresse, l'identifiant et le mot de passe."
	}
	if len(cause) > maxCauseChars {
		cause = cause[:maxCauseChars] + "…"
	}
	return cause
}

// maxCauseChars borne le message affiché. Une erreur de bibliothèque peut
// être très longue ; l'essentiel est en tête.
const maxCauseChars = 400

// discoverChoices interroge le serveur pour proposer les agendas du
// compte. N'est appelé qu'après un test réussi : inutile de faire attendre
// l'affichage sur un serveur distant à chaque ouverture de la page.
func discoverChoices(r *http.Request) []calendarChoice {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	raw, found, err := host.GetConfig(r.Context(), orgID, memberID)
	if err != nil || !found {
		return nil
	}
	cfg, err := parseConfig(raw)
	if err != nil || !cfg.complete() {
		return nil
	}
	password, found, err := host.GetSecret(r.Context(), orgID, memberID, secretKeyPassword)
	if err != nil || !found {
		return nil
	}

	sess, err := dial(r.Context(), cfg, password)
	if err != nil {
		return nil
	}

	calendars, err := discoverCalendars(r.Context(), sess.client)
	if err != nil {
		return nil
	}

	choices := make([]calendarChoice, 0, len(calendars))
	for _, cal := range calendars {
		name := cal.Name
		if name == "" {
			name = cal.Path
		}
		choices = append(choices, calendarChoice{Path: cal.Path, Name: name})
	}
	return choices
}

// formConfig relit la configuration existante et applique le formulaire.
func formConfig(r *http.Request) (memberConfig, string, string, string) {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	var cfg memberConfig
	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		cfg, _ = parseConfig(raw)
	}

	cfg.ServerURL = r.FormValue("server_url")
	cfg.Username = r.FormValue("username")
	cfg.AllowRead = r.FormValue("allow_read") != ""
	cfg.AllowWrite = r.FormValue("allow_write") != ""
	if path := r.FormValue("calendar_path"); path != "" {
		cfg.CalendarPath = path
	}
	if seconds, err := strconv.Atoi(r.FormValue("poll_seconds")); err == nil && seconds > 0 {
		cfg.PollSeconds = seconds
	}

	// La reprise du magasin ne s'active QUE sur un compte joignable :
	// cocher la case sur une configuration vide couperait les rappels de
	// la personne sans rien pour les accueillir.
	cfg.EventStore = r.FormValue("event_store") != "" && cfg.complete()

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

	// La configuration saisie est enregistrée avant le test : sans cela,
	// l'agenda proposé au choix serait celui de l'ancienne adresse.
	if cfg.complete() {
		_ = host.SaveConfig(r.Context(), orgID, memberID, cfg.marshal())
		if password != "" {
			_ = host.SetSecret(r.Context(), orgID, memberID, secretKeyPassword, password)
		}
	}

	if !cfg.complete() || password == "" {
		redirectTested(w, r, false, "renseignez l'adresse, l'identifiant et le mot de passe.", false)
		return
	}

	_, err := dial(r.Context(), cfg, password)
	if err == nil {
		redirectTested(w, r, true, "", false)
		return
	}

	// La cause va au journal ET à l'écran : l'exploitant la lit dans le
	// flux d'Automata, la personne la lit sur sa page.
	logDialFailure(r.Context(), cfg, err)
	redirectTested(w, r, false, err.Error(), isCertificateError(err))
}

// redirectTested renvoie vers la page avec le résultat du test. La cause
// voyage dans l'URL : elle est destinée à être lue, pas devinée.
func redirectTested(w http.ResponseWriter, r *http.Request, ok bool, cause string, certificate bool) {
	params := url.Values{}
	if ok {
		params.Set("tested", "ok")
	} else {
		params.Set("tested", "ko")
		if cause != "" {
			params.Set("cause", cause)
		}
		if certificate {
			params.Set("cert", "1")
		}
	}

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?"+params.Encode(), http.StatusSeeOther)
}

// handleUIAcceptCertificate enregistre — ou retire — l'exception TLS du
// membre. Une empreinte vide retire l'exception ; toute autre valeur doit
// être une empreinte SHA-256, sans quoi rien n'est écrit.
//
// L'empreinte vient du formulaire, donc du navigateur : elle est
// revalidée ici, et n'est de toute façon qu'une CIBLE d'épinglage — la
// poser sur une valeur fantaisiste ne fait rien accepter, elle fait
// échouer la connexion.
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
		redirectTested(w, r, false, "empreinte de certificat illisible.", false)
		return
	}
	cfg.TLSFingerprint = fingerprint

	if err := host.SaveConfig(r.Context(), orgID, memberID, cfg.marshal()); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	slog.WarnContext(r.Context(), "caldav: exception de certificat modifiée",
		"server", tlsServerName(cfg.ServerURL),
		"accepted", fingerprint != "")

	http.Redirect(w, r, pluginsdk.BasePath(r)+"?saved=1", http.StatusSeeOther)
}
