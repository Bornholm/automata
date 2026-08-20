package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Reverse proxy des interfaces embarquées des plugins. Patron repris du
// système de plugins de Xolo, avec trois corrections de sécurité :
// l'iframe est rendue avec sandbox sans allow-same-origin (origine opaque,
// pas d'accès aux cookies de l'application), les Set-Cookie du plugin sont
// supprimés, et chaque requête proxifiée porte le jeton d'UI que seul le
// proxy connaît — le port loopback du plugin est fermé aux autres
// processus locaux.
//
// Les POST proxifiés sont exemptés du contrôle CSRF de l'application : le
// formulaire vit dans le document du plugin, qui ne peut pas porter notre
// jeton. La protection vient de la session (admin ou lien de profil), du
// jeton d'UI et de la sandbox.

// isPluginUIPath reconnaît les chemins servis par le reverse proxy des
// interfaces de plugins, seuls exemptés du contrôle CSRF de
// l'application (voir le commentaire de tête).
func isPluginUIPath(path string) bool {
	if !strings.HasPrefix(path, "/admin/orgs/") {
		return false
	}
	_, rest, found := strings.Cut(strings.TrimPrefix(path, "/admin/orgs/"), "/plugins/")
	if !found {
		return false
	}
	_, uiPath, found := strings.Cut(rest, "/ui/")
	return found && !strings.Contains(uiPath, "..")
}

// PluginUIEndpoint est la part du gestionnaire de plugins dont le proxy a
// besoin. Le port et le jeton sont relus à CHAQUE requête : ils changent à
// chaque redémarrage du plugin.
type PluginUIEndpoint interface {
	UIEndpoint(name string) (port uint32, token string, ok bool)
}

// handleAdminPluginUI proxifie l'interface d'un plugin pour l'opérateur.
// L'organisation vit dans le CHEMIN, pas en paramètre de requête : une
// navigation relative du document du plugin (formulaire, lien) reste sous
// le préfixe et conserve donc son contexte — un ?org= se perdrait au
// premier POST.
func (s *Server) handleAdminPluginUI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	orgID := r.PathValue("id")

	exists := false
	if ok := s.withTx(w, r, func(tx *sql.Tx) error {
		_, found, err := s.orgs.FindByID(r.Context(), tx, orgID)
		exists = found
		return err
	}); !ok {
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}

	s.proxyPluginUI(w, r, name, orgID, "", "admin", "/admin/orgs/"+orgID+"/plugins/"+name+"/ui")
}

// handleProfilePluginUI proxifie l'interface d'un plugin pour un membre,
// derrière la session de profil. Le plugin doit être ACTIF pour
// l'organisation du membre : sinon la page n'existe pas.
func (s *Server) handleProfilePluginUI(w http.ResponseWriter, r *http.Request) {
	member, _, ok := s.resolveProfile(w, r)
	if !ok {
		return
	}

	name := r.PathValue("name")

	enabled := false
	if ok := s.withTx(w, r, func(tx *sql.Tx) error {
		var err error
		enabled, err = s.pluginActivations.IsEnabled(r.Context(), tx, name, member.OrgID)
		return err
	}); !ok {
		return
	}
	if !enabled {
		http.NotFound(w, r)
		return
	}

	base := "/p/" + r.PathValue("link") + "/plugins/" + name + "/ui"
	s.proxyPluginUI(w, r, name, member.OrgID, member.ID, "member", base)
}

// handlePluginOAuthCallback relaie le retour d'un fournisseur OAuth vers
// le plugin. La route est PUBLIQUE et stable — un fournisseur comme Google
// exige une URL de redirection fixe, incompatible avec les liens de profil
// temporaires — mais elle est étroite : seul le chemin oauth/callback du
// plugin est atteignable, sans aucun en-tête d'identité. La sécurité du
// flux repose sur le paramètre state, généré et vérifié par le plugin.
func (s *Server) handlePluginOAuthCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.proxyPluginUI(w, r, name, "", "", "public", "/plugins/"+name+"/oauth")
}

// proxyPluginUI relaie la requête vers le serveur HTTP embarqué du plugin.
func (s *Server) proxyPluginUI(w http.ResponseWriter, r *http.Request, name, orgID, memberID, view, basePath string) {
	if s.pluginManager == nil {
		http.NotFound(w, r)
		return
	}

	endpoint, ok := s.pluginManager.(PluginUIEndpoint)
	if !ok {
		http.NotFound(w, r)
		return
	}
	port, token, ok := endpoint.UIEndpoint(name)
	if !ok {
		http.NotFound(w, r)
		return
	}

	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(target)

	uiPath := "/" + r.PathValue("path")
	if view == "public" {
		// La route publique ne dessert que le retour OAuth, rien d'autre.
		uiPath = "/oauth/callback"
	}

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = uiPath
		req.URL.RawPath = ""
		// Contrat d'identité : dérivé de la session et de l'URL côté
		// hôte, jamais du navigateur.
		req.Header.Set("X-Automata-Org-Id", orgID)
		req.Header.Set("X-Automata-Member-Id", memberID)
		req.Header.Set("X-Automata-Plugin-Base-Path", basePath+"/")
		req.Header.Set("X-Automata-View", view)
		req.Header.Set("X-Automata-UI-Token", token)
		// L'URL publique de l'instance : le plugin en a besoin pour
		// composer une URL de redirection OAuth enregistrable.
		req.Header.Set("X-Automata-Base-Url", strings.TrimSuffix(s.cfg.Web.BaseURL, "/"))
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		// Le document du plugin ne pose jamais de cookie sur notre
		// origine, et il ne s'affiche que dans notre iframe.
		resp.Header.Del("Set-Cookie")
		resp.Header.Set("Content-Security-Policy", "frame-ancestors 'self'")

		// Les redirections relatives du plugin restent sous son préfixe.
		if loc := resp.Header.Get("Location"); loc != "" && strings.HasPrefix(loc, "/") {
			resp.Header.Set("Location", basePath+loc)
		}
		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.logger.WarnContext(r.Context(), "web: interface de plugin injoignable", "plugin", name, "error", err)
		http.Error(w, "interface du plugin indisponible", http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}
