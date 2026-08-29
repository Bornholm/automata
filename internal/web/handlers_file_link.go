package web

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bornholm/automata/internal/plugin"
)

// Lien temporaire de téléchargement d'un fichier de plugin (route /f/).
//
// Il existe parce que la messagerie plafonne ses pièces jointes bien en
// deçà de ce qu'un espace de travail produit : une vidéo de 140 Mo ne
// rentre pas dans WhatsApp. Le lien est la seconde voie de sortie.
//
// Les octets ne sont NI copiés NI stockés : ils traversent depuis le
// plugin, tranche par tranche (voir plugin.Manager.OpenFile). Un fichier
// de plusieurs centaines de mégaoctets ne coûte donc jamais sa taille en
// mémoire, quel que soit le nombre de téléchargements simultanés.

// fileLinkDownloadTimeout borne la durée d'un téléchargement. Généreux :
// une vidéo de 200 Mo sur une liaison mobile lente prend du temps, et
// couper un transfert presque abouti serait pire que de le laisser finir.
const fileLinkDownloadTimeout = 30 * time.Minute

// handleFileLink sert le fichier désigné par un jeton signé.
//
// Un jeton invalide, expiré ou visant un plugin désactivé rend 404, sans
// distinction : rien n'indique à qui l'essaie s'il a manqué de peu.
func (s *Server) handleFileLink(w http.ResponseWriter, r *http.Request) {
	pluginName, orgID, memberID, filePath, ok := s.ParseFileLinkToken(r.PathValue("token"), s.Now())
	if !ok {
		http.NotFound(w, r)
		return
	}

	if s.PluginMgr == nil {
		http.NotFound(w, r)
		return
	}

	// Désactiver le plugin pour l'organisation coupe ses liens : c'est le
	// coupe-circuit de l'opérateur, relu à chaque accès.
	var enabled bool
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		enabled, err = s.PluginActivations.IsEnabled(r.Context(), tx, pluginName, orgID)
		return err
	}) {
		return
	}
	if !enabled {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), fileLinkDownloadTimeout)
	defer cancel()

	meta, body, err := s.PluginMgr.OpenFile(ctx, pluginName, plugin.CallContext{
		OrgID:    orgID,
		MemberID: memberID,
	}, filePath)
	if err != nil {
		// Le fichier a pu disparaître avant le lien : un espace de travail
		// inactif est effacé au bout de 24 h. 404 est alors la réponse
		// juste, et l'échec reste visible côté exploitation.
		s.Logger.WarnContext(r.Context(), "web: fichier de lien introuvable",
			"plugin", pluginName, "org", orgID, "error", err)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = body.Close() }()

	filename := meta.Filename
	if filename == "" {
		filename = path.Base(filePath)
	}

	header := w.Header()
	header.Set("Content-Type", fileLinkContentType(meta.MimeType, filename))
	// Toujours en pièce jointe : ce lien sert à RÉCUPÉRER un fichier, pas à
	// l'afficher. Rien n'est donc jamais interprété par le navigateur, quel
	// que soit le type — un html produit dans l'atelier ne s'exécute pas.
	header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(filename)))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Robots-Tag", "noindex")
	header.Set("Cache-Control", "no-store")
	// Content-Length quand la source l'annonce : le navigateur affiche une
	// progression au lieu d'un compteur qui monte sans fin.
	if meta.Size > 0 {
		header.Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}

	// Pas de http.ServeContent : il réclame un io.ReadSeeker, que le flux
	// n'offre pas. Conséquence assumée — pas de requête Range, donc pas de
	// reprise ni de lecture partielle (voir docs/plugins-workspace.md).
	copied, err := io.Copy(w, body)
	if err != nil {
		// Les en-têtes sont déjà partis : impossible de répondre une
		// erreur. On journalise, le client verra un transfert tronqué.
		s.Logger.WarnContext(r.Context(), "web: téléchargement interrompu",
			"plugin", pluginName, "org", orgID, "bytes", copied, "error", err)
		return
	}

	s.Logger.InfoContext(r.Context(), "web: fichier téléchargé par lien",
		"plugin", pluginName, "org", orgID, "bytes", copied)
}

// fileLinkContentType retient le type annoncé par le plugin, ou le devine
// de l'extension. Il ne sert qu'à nommer le contenu : la réponse est de
// toute façon marquée en pièce jointe et non reniflable.
func fileLinkContentType(mimeType, filename string) string {
	if parsed, _, err := mime.ParseMediaType(mimeType); err == nil && parsed != "" {
		return parsed
	}
	if guessed := mime.TypeByExtension(path.Ext(filename)); guessed != "" {
		if parsed, _, err := mime.ParseMediaType(guessed); err == nil {
			return parsed
		}
	}

	return "application/octet-stream"
}

// sanitizeFilename retire d'un nom de fichier ce qui casserait l'en-tête
// Content-Disposition — guillemets, sauts de ligne et chemins.
func sanitizeFilename(name string) string {
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)

	if name == "" || name == "." || name == ".." {
		return "file"
	}

	return name
}
