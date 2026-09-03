package main

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// L'interface du profil : le catalogue proposé par l'exploitant, un
// interrupteur par entrée, et les identifiants que l'entrée réclame.
// Discipline des secrets : une valeur saisie n'est jamais relue — seul un
// booléen « défini » traverse, et un champ laissé vide conserve la valeur.

var uiTemplate = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;font-size:14px;color:#161c27;margin:0;padding:16px;background:#fff}
.agent{border:1px solid #e6e8ee;border-radius:12px;padding:14px 16px;margin-bottom:12px}
.agent h2{margin:0;font-size:15px}
.desc{color:#5a6478;font-size:13px;margin-top:4px;line-height:1.5}
label{display:block;margin-top:10px;font-weight:600;font-size:13px}
input[type=text],input[type=password]{width:100%;box-sizing:border-box;height:34px;padding:0 10px;margin-top:4px;border:1px solid #d8dce4;border-radius:8px;font-size:14px;background:#fff}
.hint{font-weight:400;color:#8a93a5;font-size:12px;margin-top:2px}
button{margin-top:14px;height:34px;padding:0 14px;border:0;border-radius:8px;background:#5b5bd6;color:#fff;font-weight:600;font-size:13px;cursor:pointer}
button.off{background:#fff;color:#161c27;border:1px solid #d8dce4}
.state{display:inline-block;margin-left:8px;font-size:12px;font-weight:600;padding:2px 8px;border-radius:99px}
.on{background:#e7f4ef;color:#17795e}
.flash{margin-bottom:12px;padding:8px 12px;border-radius:8px;background:#e7f4ef;color:#17795e;font-size:13px}
.error{background:#fce9e7;color:#b42318}
.notice{margin-top:10px;padding:8px 12px;border-radius:8px;background:#f5f5fe;color:#3f3fa8;font-size:12px;line-height:1.5}
</style></head><body>
{{if not .MemberScoped}}
<p class="hint">Ces sous-agents s'activent depuis le profil de chaque personne : ce sont ses outils, ses identifiants.</p>
{{else}}
{{if .Flash}}<div class="flash {{if .FlashError}}error{{end}}">{{.Flash}}</div>{{end}}
{{if not .Agents}}
<p class="hint">Aucun sous-agent n'est proposé par cette instance. Le catalogue est déclaré par l'exploitant.</p>
{{end}}
{{range .Agents}}
<div class="agent">
	<h2>{{.Name}}{{if .Enabled}}<span class="state on">activé</span>{{end}}</h2>
	<div class="desc">{{.Description}}</div>
	{{range .Servers}}<div class="hint">{{.Label}}</div>{{end}}
	<form method="post" action="{{$.Base}}save">
		<input type="hidden" name="agent" value="{{.Name}}">
		{{range .Credentials}}
		<label>{{.Label}}<input type="password" name="cred_{{.Key}}" placeholder="{{if .Defined}}••••••••{{else}}{{if .Required}}requis{{else}}facultatif{{end}}{{end}}">
		<div class="hint">{{if .Help}}{{.Help}} {{end}}{{if .Defined}}Défini. Laissez vide pour le conserver.{{end}}</div></label>
		{{end}}
		{{if .Missing}}
		<div class="notice">Renseignez {{.MissingLabel}} pour activer ce sous-agent.</div>
		{{end}}
		{{if .Enabled}}
		<button type="submit" name="action" value="disable" class="off">Désactiver</button>
		{{if .Credentials}}<button type="submit" name="action" value="enable">Enregistrer</button>{{end}}
		{{else}}
		<button type="submit" name="action" value="enable">Activer</button>
		{{end}}
	</form>
</div>
{{end}}
{{end}}
</body></html>`))

type uiCredential struct {
	Key      string
	Label    string
	Help     string
	Required bool
	// Defined : la valeur existe côté hôte. Elle n'est jamais relue.
	Defined bool
}

// uiServer est l'état d'installation d'un serveur, dit en une ligne.
type uiServer struct {
	Label string
}

type uiAgent struct {
	Name        string
	Description string
	Enabled     bool
	Servers     []uiServer
	Credentials []uiCredential
	// Missing : au moins un identifiant requis manque.
	Missing      bool
	MissingLabel string
}

type uiData struct {
	Base         string
	MemberScoped bool
	Agents       []uiAgent
	Flash        string
	FlashError   bool
}

func (p *Plugin) newUIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", p.handleUIRoot)
	mux.HandleFunc("POST /save", p.handleUISave)
	return mux
}

func (p *Plugin) handleUIRoot(w http.ResponseWriter, r *http.Request) {
	data := uiData{
		Base:         pluginsdk.BasePath(r),
		MemberScoped: pluginsdk.MemberID(r) != "",
	}
	if !data.MemberScoped {
		_ = uiTemplate.Execute(w, data)
		return
	}

	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	var cfg memberConfig
	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		cfg = parseConfig(raw)
	}

	for _, agent := range p.catalog.Agents {
		row := uiAgent{
			Name:        agent.Name,
			Description: agent.Description,
			Enabled:     cfg.enabled(agent.Name),
		}

		for _, server := range agent.Servers {
			if label := p.installLabel(agent.Name, server); label != "" {
				row.Servers = append(row.Servers, uiServer{Label: label})
			}
		}

		var missing []string
		for _, cred := range agent.Credentials {
			_, defined, err := host.GetSecret(r.Context(), orgID, memberID, secretKey(agent.Name, cred.Key))
			defined = defined && err == nil
			row.Credentials = append(row.Credentials, uiCredential{
				Key: cred.Key, Label: credentialLabel(cred), Help: cred.Help,
				Required: cred.Required, Defined: defined,
			})
			if cred.Required && !defined {
				missing = append(missing, credentialLabel(cred))
			}
		}
		row.Missing = len(missing) > 0
		row.MissingLabel = strings.Join(missing, ", ")

		data.Agents = append(data.Agents, row)
	}

	switch r.URL.Query().Get("done") {
	case "enabled":
		data.Flash = "Sous-agent activé. Il est disponible dès votre prochain message."
	case "disabled":
		data.Flash = "Sous-agent désactivé."
	case "missing":
		data.Flash = "Il manque un identifiant : le sous-agent reste inactif."
		data.FlashError = true
	}

	_ = uiTemplate.Execute(w, data)
}

func (p *Plugin) handleUISave(w http.ResponseWriter, r *http.Request) {
	if pluginsdk.MemberID(r) == "" {
		http.Error(w, "member context required", http.StatusForbidden)
		return
	}

	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)

	// Le nom vient d'un formulaire : il ne vaut que s'il désigne une
	// entrée du catalogue de l'exploitant.
	agent, ok := p.catalog.find(r.FormValue("agent"))
	if !ok {
		http.Error(w, "unknown sub-agent", http.StatusBadRequest)
		return
	}

	var cfg memberConfig
	if raw, found, err := host.GetConfig(r.Context(), orgID, memberID); err == nil && found {
		cfg = parseConfig(raw)
	}

	// Les identifiants saisis sont enregistrés dans tous les cas : une
	// désactivation ne doit pas faire perdre ce qui vient d'être tapé.
	present := map[string]bool{}
	for _, cred := range agent.Credentials {
		key := secretKey(agent.Name, cred.Key)
		if value := r.FormValue("cred_" + cred.Key); value != "" {
			if err := host.SetSecret(r.Context(), orgID, memberID, key, value); err != nil {
				http.Error(w, "secret save failed", http.StatusInternalServerError)
				return
			}
			present[cred.Key] = true
			continue
		}
		if _, found, err := host.GetSecret(r.Context(), orgID, memberID, key); err == nil && found {
			present[cred.Key] = true
		}
	}

	enable := r.FormValue("action") == "enable"
	if enable && len(agent.missingCredentials(present)) > 0 {
		http.Redirect(w, r, pluginsdk.BasePath(r)+"?done=missing", http.StatusSeeOther)
		return
	}

	if err := host.SaveConfig(r.Context(), orgID, memberID, cfg.withEntry(agent.Name, enable).marshal()); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	// Les identifiants ont pu changer : la connexion en cours parlerait
	// encore avec les anciens.
	p.pool.forget(agent.Name, orgID, memberID)

	done := "disabled"
	if enable {
		done = "enabled"
	}
	http.Redirect(w, r, pluginsdk.BasePath(r)+"?done="+done, http.StatusSeeOther)
}

// installLabel dit en une ligne où en est le serveur : rien à installer,
// version en service, mise à jour en attente, ou installation impossible.
// Vide quand il n'y a rien à en dire.
func (p *Plugin) installLabel(agentName string, server serverSpec) string {
	if server.Install == nil {
		return ""
	}
	if !p.installer.available() {
		return "Serveur « " + server.Name + " » : installation impossible, l'instance n'a pas de répertoire de données (" + envDataDir + ")."
	}

	state, pending := p.installer.pending(agentName, server)
	switch {
	case pending:
		return "Serveur « " + server.Name + " » : version " + state.Version +
			" en service, mise à jour vers " + server.Version + " à la prochaine utilisation."
	case state.Version != "":
		return "Serveur « " + server.Name + " » : version " + state.Version + " installée."
	default:
		return "Serveur « " + server.Name + " » : version " + server.Version + ", installée à la première activation."
	}
}

// credentialLabel retombe sur la clé quand l'exploitant n'a pas écrit de
// libellé : mieux vaut un nom technique qu'un champ anonyme.
func credentialLabel(cred credentialField) string {
	if strings.TrimSpace(cred.Label) != "" {
		return cred.Label
	}
	return cred.Key
}
