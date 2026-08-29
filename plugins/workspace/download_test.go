package main

import (
	"strings"
	"testing"
)

// La liste blanche est la politique d'exploitation : elle accepte les
// sous-domaines d'un domaine listé, et rien d'autre.
func TestValidateDownloadURL(t *testing.T) {
	domains := []string{"youtube.com", "youtu.be", "vimeo.com"}

	accepted := []string{
		"https://www.youtube.com/watch?v=abc",
		"https://m.youtube.com/watch?v=abc",
		"https://youtu.be/abc",
		"http://vimeo.com/123456",
		// Sans schéma : un modèle recopie le lien tel qu'il s'affiche.
		// Vu en production, c'est ce qui a fait échouer le premier essai.
		"www.youtube.com/watch?v=abc",
		"vimeo.com/123456",
	}
	for _, raw := range accepted {
		if _, err := validateDownloadURL(raw, domains); err != nil {
			t.Errorf("%q devrait être accepté: %v", raw, err)
		}
	}

	refused := map[string]string{
		// Domaine hors liste : le cas courant.
		"https://exemple.fr/video.mp4": "limited to these sites",
		// Suffixe trompeur : « notyoutube.com » ne doit pas passer pour un
		// sous-domaine de youtube.com.
		"https://notyoutube.com/watch": "limited to these sites",
		"https://youtube.com.evil.tld": "limited to these sites",
		// Schémas hors http(s) : file:// lirait le disque du sandbox.
		// La complétion en https ne s'applique qu'à l'absence de schéma,
		// jamais à un schéma explicite et refusé.
		"file:///etc/passwd": "http and https",
		"ftp://exemple.fr/v": "http and https",
		// Adresses littérales : la forme d'une tentative vers un service
		// interne (SSRF), refusée avant même la liste blanche.
		"http://127.0.0.1:8080/admin":   "numeric host",
		"http://10.0.0.5/":              "numeric host",
		"http://169.254.169.254/latest": "numeric host",
		// Hôte absent.
		"https://": "no host",
	}
	for raw, fragment := range refused {
		_, err := validateDownloadURL(raw, domains)
		if err == nil {
			t.Errorf("%q devrait être refusé", raw)
			continue
		}
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("%q: erreur %q, attendu contenant %q", raw, err, fragment)
		}
	}
}

// Le nom de sortie est borné : ce qui atterrit dans le workspace est
// ensuite manipulé par des scripts.
func TestValidateOutputName(t *testing.T) {
	if name, err := validateOutputName(""); err != nil || name != "video" {
		t.Errorf("nom vide = (%q, %v), attendu le défaut", name, err)
	}
	if _, err := validateOutputName("ma-video_2"); err != nil {
		t.Errorf("nom simple refusé: %v", err)
	}
	for _, bad := range []string{"../evasion", "nom avec espace", "nom;rm -rf", "trop" + strings.Repeat("long", 20)} {
		if _, err := validateOutputName(bad); err == nil {
			t.Errorf("%q devrait être refusé", bad)
		}
	}
}

// La liste par défaut sert quand l'exploitant n'a rien configuré ; une
// liste explicite la remplace entièrement.
func TestDownloadDomains(t *testing.T) {
	t.Setenv(envDownloadDomains, "")
	if got := downloadDomains(); len(got) != len(defaultDownloadDomains) {
		t.Errorf("sans configuration = %v, attendu le défaut", got)
	}

	t.Setenv(envDownloadDomains, " Peertube.example , , tube.local ")
	got := downloadDomains()
	if len(got) != 2 || got[0] != "peertube.example" || got[1] != "tube.local" {
		t.Errorf("liste configurée = %v", got)
	}
}

// L'échappement protège l'apostrophe simple, seul caractère capable de
// clore la citation dans le script transmis à LeaSH.
func TestShellSingleQuoted(t *testing.T) {
	if got := shellSingleQuoted("https://a/b"); got != "https://a/b" {
		t.Errorf("valeur simple altérée: %q", got)
	}
	if got := shellSingleQuoted("a'b"); got != `a'\''b` {
		t.Errorf("apostrophe mal échappée: %q", got)
	}
}
