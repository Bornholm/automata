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
		site, siteFound, err := s.pluginSites.FindBySlug(r.Context(), tx, slug)
		if err != nil {
			return err
		}
		if !siteFound {
			return nil
		}

		// Désactiver le plugin pour l'organisation coupe ses pages : la
		// désactivation est le coupe-circuit de l'opérateur. Le contenu
		// reste en base, la réactivation ressuscite les liens.
		enabled, err := s.pluginActivations.IsEnabled(r.Context(), tx, site.PluginName, site.OrgID)
		if err != nil {
			return err
		}
		if !enabled {
			return nil
		}

		object, found, err = s.pluginObjects.Get(r.Context(), tx, site.PluginName, site.OrgID, site.MemberID, site.Collection, filePath)
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
	header.Set("Cache-Control", "public, max-age=300")

	// ServeContent apporte HEAD, If-Modified-Since et les requêtes Range —
	// indispensables pour les vidéos — et respecte le Content-Type posé.
	http.ServeContent(w, r, "", object.UpdatedAt, bytes.NewReader(object.Data))
}
