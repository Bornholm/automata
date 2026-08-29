package main

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// L'interface de profil, rendue dans l'iframe sandboxée : la liste des
// espaces du membre, le lien public à copier, le téléchargement d'une
// archive et la suppression. Pas de confirm() JavaScript — l'iframe n'a
// pas allow-modals — la suppression exige de retaper le nom de l'espace.

var uiTemplate = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;font-size:14px;color:#161c27;margin:0;padding:16px;background:#fff}
h2{font-size:15px;margin:0 0 4px}
.hint{font-weight:400;color:#8a93a5;font-size:12px}
.space{border:1px solid #e4e7ee;border-radius:10px;padding:12px 14px;margin-top:12px}
.head{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.name{font-weight:600;font-size:14px}
.badge{font-size:11px;font-weight:600;border-radius:999px;padding:2px 8px;background:#eef0f5;color:#5a6372}
.badge.live{background:#e7f4ef;color:#17795e}
.link{display:flex;align-items:center;gap:6px;margin-top:8px}
.link input{flex:1;min-width:0;height:30px;padding:0 8px;border:1px solid #d8dce4;border-radius:8px;font-size:12px;color:#5a6372;background:#fafbfc}
.actions{display:flex;align-items:center;gap:8px;margin-top:10px;flex-wrap:wrap}
button,a.btn{height:30px;padding:0 12px;border:1px solid #d8dce4;border-radius:8px;background:#fff;color:#161c27;font-weight:600;font-size:12px;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center}
button.danger{color:#b42318;border-color:#f2c6c2}
.confirm input{height:30px;padding:0 8px;border:1px solid #d8dce4;border-radius:8px;font-size:12px;width:160px}
.flash{margin-top:12px;padding:8px 12px;border-radius:8px;background:#e7f4ef;color:#17795e;font-size:13px}
.error{background:#fce9e7;color:#b42318}
.files{margin:8px 0 0;padding:0;list-style:none;font-size:12px;color:#5a6372}
</style></head><body>
{{if not .MemberScoped}}
<p class="hint">Les pages appartiennent à chaque personne : elles se gèrent depuis son profil.</p>
{{else}}
<h2>Mes pages</h2>
<p class="hint">Les pages sont créées et modifiées dans la conversation avec l'assistant. Un lien publié reste en ligne jusqu'à sa dépublication ou sa suppression.</p>
{{if .Flash}}<div class="flash {{if .FlashError}}error{{end}}">{{.Flash}}</div>{{end}}
{{if not .Spaces}}<p class="hint" style="margin-top:14px">Aucune page pour l'instant. Demandez à l'assistant d'en créer une !</p>{{end}}
{{range .Spaces}}
<div class="space">
	<div class="head">
		<span class="name">{{.Name}}</span>
		{{if .URL}}<span class="badge live">publiée</span>{{else}}<span class="badge">brouillon</span>{{end}}
		<span class="hint">{{.FileCount}} fichier(s), {{.SizeLabel}}</span>
	</div>
	{{if .URL}}
	<div class="link">
		<input type="text" value="{{.URL}}" readonly id="url-{{.Name}}">
		<button type="button" onclick="copyLink('url-{{.Name}}', this)">Copier</button>
	</div>
	{{end}}
	{{if .PreviewURL}}
	<div class="link">
		<input type="text" value="{{.PreviewURL}}" readonly id="preview-{{.Name}}" title="Lien de prévisualisation du brouillon">
		<button type="button" onclick="copyLink('preview-{{.Name}}', this)">Copier</button>
	</div>
	<div class="hint">Aperçu du brouillon — lien personnel, valable environ une heure.</div>
	{{end}}
	<div class="actions">
		<a class="btn" href="{{$.Base}}spaces/{{.Name}}/archive.zip">Télécharger l'archive</a>
		<form method="post" action="{{$.Base}}spaces/{{.Name}}/delete" class="confirm" style="display:flex;gap:8px;align-items:center;margin:0">
			<input type="text" name="confirm_name" placeholder="retapez « {{.Name}} »" autocomplete="off">
			<button type="submit" class="danger">Supprimer</button>
		</form>
	</div>
</div>
{{end}}
<script>
function copyLink(id, btn){
	var input=document.getElementById(id);
	input.select();
	var done=function(){btn.textContent="Copié !";setTimeout(function(){btn.textContent="Copier";},1500);};
	if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(input.value).then(done,function(){document.execCommand("copy");done();});}
	else{document.execCommand("copy");done();}
}
</script>
{{end}}
</body></html>`))

type uiSpace struct {
	Name       string
	URL        string
	PreviewURL string
	FileCount  int
	SizeLabel  string
}

type uiData struct {
	Base         string
	MemberScoped bool
	Spaces       []uiSpace
	Flash        string
	FlashError   bool
}

func newUIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleUIRoot)
	mux.HandleFunc("POST /spaces/{name}/delete", handleUIDelete)
	mux.HandleFunc("GET /spaces/{name}/archive.zip", handleUIArchive)
	return mux
}

func handleUIRoot(w http.ResponseWriter, r *http.Request) {
	data := uiData{
		Base:         pluginsdk.BasePath(r),
		MemberScoped: pluginsdk.MemberID(r) != "",
	}

	switch r.URL.Query().Get("flash") {
	case "deleted":
		data.Flash = "Page supprimée. Le lien public est mort."
	case "badname":
		data.Flash = "Suppression annulée : le nom saisi ne correspond pas."
		data.FlashError = true
	case "error":
		data.Flash = "La suppression a échoué. Réessayez."
		data.FlashError = true
	}

	if data.MemberScoped {
		data.Spaces = loadSpaces(r)
	}

	_ = uiTemplate.Execute(w, data)
}

// loadSpaces assemble la vue : espaces du magasin + publications.
func loadSpaces(r *http.Request) []uiSpace {
	host := pluginsdk.HostClientFromContext(r.Context())
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)
	ctx := r.Context()

	collections, err := host.ListCollections(ctx, orgID, memberID, "spaces/")
	if err != nil {
		return nil
	}

	names := map[string]struct{}{}
	for _, collection := range collections {
		rest, ok := strings.CutPrefix(collection, "spaces/")
		if !ok {
			continue
		}
		if name, _, ok := strings.Cut(rest, "/"); ok {
			names[name] = struct{}{}
		}
	}

	published := map[string]string{}
	if publications, err := host.ListPublications(ctx, orgID, memberID); err == nil {
		for _, publication := range publications {
			if space, ok := spaceOfLive(publication.Collection); ok {
				published[space] = publication.URL
			}
		}
	}

	spaces := make([]uiSpace, 0, len(names))
	for name := range names {
		space := uiSpace{Name: name, URL: published[name]}
		// Lien de prévisualisation du brouillon : signé et éphémère, il
		// est refabriqué à chaque affichage. Un échec (brouillon vide)
		// laisse simplement le champ absent.
		if url, _, err := host.PreviewCollection(ctx, orgID, memberID, draftCollection(name)); err == nil {
			space.PreviewURL = url
		}
		if entries, err := host.ListObjects(ctx, orgID, memberID, draftCollection(name)); err == nil {
			var total int64
			for _, entry := range entries {
				total += entry.Size
			}
			space.FileCount = len(entries)
			space.SizeLabel = sizeLabel(total)
		}
		spaces = append(spaces, space)
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].Name < spaces[j].Name })

	return spaces
}

func handleUIDelete(w http.ResponseWriter, r *http.Request) {
	base := pluginsdk.BasePath(r)
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)
	name := r.PathValue("name")

	if memberID == "" || !spaceNamePattern.MatchString(name) {
		http.Redirect(w, r, base+"?flash=error", http.StatusSeeOther)
		return
	}
	// Le garde-fou de l'humain : retaper le nom remplace le confirm()
	// indisponible dans l'iframe sandboxée.
	if strings.TrimSpace(r.FormValue("confirm_name")) != name {
		http.Redirect(w, r, base+"?flash=badname", http.StatusSeeOther)
		return
	}

	host := pluginsdk.HostClientFromContext(r.Context())
	if _, err := host.DeleteCollection(r.Context(), orgID, memberID, liveCollection(name)); err != nil {
		http.Redirect(w, r, base+"?flash=error", http.StatusSeeOther)
		return
	}
	if _, err := host.DeleteCollection(r.Context(), orgID, memberID, draftCollection(name)); err != nil {
		http.Redirect(w, r, base+"?flash=error", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, base+"?flash=deleted", http.StatusSeeOther)
}

func handleUIArchive(w http.ResponseWriter, r *http.Request) {
	orgID, memberID := pluginsdk.OrgID(r), pluginsdk.MemberID(r)
	name := r.PathValue("name")

	if memberID == "" || !spaceNamePattern.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	host := pluginsdk.HostClientFromContext(r.Context())
	plugin := &Plugin{}
	plugin.SetHostClient(host)
	data, err := plugin.zipSpace(r.Context(), callScope{host: host, orgID: orgID, memberID: memberID}, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".zip"))
	_, _ = w.Write(data)
}

// sizeLabel rend une taille lisible.
func sizeLabel(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f Mio", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0f Kio", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d octets", bytes)
	}
}
