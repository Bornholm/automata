package web

import (
	"bytes"
	"database/sql"
	"net/http"
	"path"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
)

// Pages publiques des plugins : le contenu d'une collection publiée
// (plugin_public_sites → plugin_objects) est servi tel quel sous
// /s/<slug>/. Le HTML vient d'un modèle piloté par un membre — il est
// traité en contenu hostile : CSP sandbox (origine opaque, donc aucune
// requête créditée vers /admin ou /p/ depuis un script de la page), aucun
// cookie posé, types MIME dérivés de l'extension et jamais du client.

// publicSiteContentTypes est l'allowlist des types servis inline. Toute
// extension hors liste part en octet-stream à télécharger : on ne laisse
// pas le navigateur deviner.
var publicSiteContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/plain; charset=utf-8",
	".xml":   "text/xml; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".avif":  "image/avif",
	".ico":   "image/x-icon",
	".mp4":   "video/mp4",
	".webm":  "video/webm",
	".mp3":   "audio/mpeg",
	".ogg":   "audio/ogg",
	".wav":   "audio/wav",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".wasm":  "application/wasm",
	".vtt":   "text/vtt; charset=utf-8",
	".pdf":   "application/pdf",
}

// handlePublicSiteRoot redirige /s/<slug> vers /s/<slug>/ : sans le slash
// final, les liens relatifs de l'index (href="style.css") se résoudraient
// hors du préfixe de la page.
func (s *Server) handlePublicSiteRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/s/"+r.PathValue("slug")+"/", http.StatusMovedPermanently)
}

// handlePublicSite sert un fichier d'une collection publiée.
func (s *Server) handlePublicSite(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	var (
		site      persistence.PluginPublicSite
		siteFound bool
	)
	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		var err error
		site, siteFound, err = s.pluginSites.FindBySlug(r.Context(), tx, slug)
		return err
	})
	if !ok {
		return
	}
	if !siteFound {
		http.NotFound(w, r)
		return
	}

	s.servePluginObject(w, r, site.PluginName, site.OrgID, site.MemberID, site.Collection,
		"public, max-age=300")
}

// handleDraftPreviewRoot redirige /d/<token> vers /d/<token>/, pour les
// mêmes liens relatifs que la route publique.
func (s *Server) handleDraftPreviewRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/d/"+r.PathValue("token")+"/", http.StatusMovedPermanently)
}

// handleDraftPreview sert un brouillon derrière un jeton signé éphémère :
// la capacité du membre à regarder sa propre page avant de la publier —
// remise dans son canal privé, jamais listée. Un jeton expiré ou forgé
// donne un 404 indistinct.
func (s *Server) handleDraftPreview(w http.ResponseWriter, r *http.Request) {
	pluginName, orgID, memberID, collection, ok := s.parseDraftPreviewToken(r.PathValue("token"), s.now())
	if !ok {
		http.NotFound(w, r)
		return
	}

	// no-store : un brouillon change à chaque itération avec l'agent, et
	// rien d'un contenu non publié ne doit rester dans un cache partagé.
	s.servePluginObject(w, r, pluginName, orgID, memberID, collection, "no-store")
}

// servePluginObject sert un fichier du magasin d'objets avec les en-têtes
// d'isolement communs aux pages publiées et aux prévisualisations. Le
// contenu vient d'un modèle piloté par un membre : il est traité en
// contenu hostile quelle que soit la route.
func (s *Server) servePluginObject(w http.ResponseWriter, r *http.Request, pluginName, orgID, memberID, collection, cacheControl string) {
	filePath := r.PathValue("path")
	if filePath == "" {
		filePath = "index.html"
	}

	// Les clés du magasin sont des valeurs en base, pas des chemins de
	// fichiers : aucun traversal possible. On refuse quand même toute
	// forme suspecte, par hygiène et pour des 404 nets.
	if strings.Contains(filePath, "..") || strings.HasPrefix(filePath, "/") {
		http.NotFound(w, r)
		return
	}

	var (
		object persistence.PluginObject
		found  bool
	)
	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		// Désactiver le plugin pour l'organisation coupe ses pages : la
		// désactivation est le coupe-circuit de l'opérateur. Le contenu
		// reste en base, la réactivation ressuscite les liens.
		enabled, err := s.pluginActivations.IsEnabled(r.Context(), tx, pluginName, orgID)
		if err != nil {
			return err
		}
		if !enabled {
			return nil
		}

		object, found, err = s.pluginObjects.Get(r.Context(), tx, pluginName, orgID, memberID, collection, filePath)
		return err
	})
	if !ok {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	contentType, inline := publicSiteContentTypes[strings.ToLower(path.Ext(filePath))]
	if !inline {
		contentType = "application/octet-stream"
		w.Header().Set("Content-Disposition", "attachment")
	}

	header := w.Header()
	header.Set("Content-Type", contentType)
	// L'origine opaque du sandbox est la frontière : un script de la page
	// ne peut ni lire les cookies (il n'y en a pas ici) ni émettre de
	// requête créditée vers le reste du serveur.
	header.Set("Content-Security-Policy", "sandbox allow-scripts")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	// Pages non listées, non devinables : elles n'ont rien à faire dans
	// un index de recherche.
	header.Set("X-Robots-Tag", "noindex")
	header.Set("Cache-Control", cacheControl)

	// ServeContent apporte HEAD, If-Modified-Since et les requêtes Range —
	// indispensables pour les vidéos — et respecte le Content-Type posé.
	http.ServeContent(w, r, "", object.UpdatedAt, bytes.NewReader(object.Data))
}
