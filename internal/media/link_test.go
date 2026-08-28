package media

import (
	"strings"
	"testing"

	"github.com/bornholm/go-courier"
)

// Le cas du bug d'origine : un reel Facebook partagé en carte de
// prévisualisation, sans aucun fichier joint. L'annotation doit dire
// explicitement qu'aucun fichier n'accompagne le message.
func TestLinkSharesNotice(t *testing.T) {
	previews := []courier.LinkPreview{{
		URL:   "https://facebook.com/reel/123",
		Title: "438 vues | Reel by Kevin",
	}}

	notice := LinkSharesNotice(previews, false)
	if notice == "" {
		t.Fatal("une annotation est attendue")
	}
	for _, fragment := range []string{
		"https://facebook.com/reel/123",
		"438 vues | Reel by Kevin",
		"no file is attached",
	} {
		if !strings.Contains(notice, fragment) {
			t.Errorf("l'annotation ne contient pas %q :\n%s", fragment, notice)
		}
	}
}

// Quand le message porte AUSSI de vrais fichiers, l'annotation signale le
// lien sans prétendre que le message est vide.
func TestLinkSharesNoticeWithFiles(t *testing.T) {
	previews := []courier.LinkPreview{{URL: "https://example.com"}}

	notice := LinkSharesNotice(previews, true)
	if notice == "" {
		t.Fatal("une annotation est attendue")
	}
	if strings.Contains(notice, "no file is attached") {
		t.Errorf("l'annotation ne doit pas nier les fichiers présents :\n%s", notice)
	}
}

// Le titre absent est remplacé par la description ; sans l'un ni l'autre,
// l'URL suffit.
func TestLinkSharesNoticeFallbacks(t *testing.T) {
	notice := LinkSharesNotice([]courier.LinkPreview{{URL: "https://a", Description: "desc only"}}, false)
	if !strings.Contains(notice, `"desc only"`) {
		t.Errorf("la description doit remplacer le titre absent :\n%s", notice)
	}

	notice = LinkSharesNotice([]courier.LinkPreview{{URL: "https://b"}}, false)
	if !strings.Contains(notice, "- https://b") {
		t.Errorf("l'URL seule doit suffire :\n%s", notice)
	}

	if got := LinkSharesNotice(nil, false); got != "" {
		t.Errorf("aucune annotation attendue sans preview, obtenu %q", got)
	}
}
