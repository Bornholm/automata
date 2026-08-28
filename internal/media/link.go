package media

import (
	"fmt"
	"strings"

	"github.com/bornholm/go-courier"
)

// LinkSharesNotice décrit les liens partagés avec carte de prévisualisation,
// pour que l'agent ne prenne pas un partage de lien pour un envoi de
// fichier : sans cette note, « voici la vidéo » suivi d'une carte Facebook
// lance un traitement de fichier qui ne trouvera jamais rien.
//
// URL et titre seulement — la vignette n'est pas transmise, elle ferait
// justement croire à une pièce jointe.
//
// En anglais : ce texte part vers le modèle (AGENTS.md).
func LinkSharesNotice(previews []courier.LinkPreview, hasFiles bool) string {
	if len(previews) == 0 {
		return ""
	}

	lines := make([]string, 0, len(previews))
	for _, p := range previews {
		title := p.Title
		if title == "" {
			title = p.Description
		}

		if title == "" {
			lines = append(lines, "- "+p.URL)
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (%q)", p.URL, title))
	}

	if hasFiles {
		return "\n\n[The message also shares a link with a preview card — the link is not an uploaded file:\n" +
			strings.Join(lines, "\n") + "]"
	}

	return "\n\n[The message shares a link with a preview card. This is NOT an uploaded file: no file is attached to this message.\n" +
		strings.Join(lines, "\n") + "\n" +
		"To work on the media behind the link, ask the user to send the file itself as an attachment.]"
}
