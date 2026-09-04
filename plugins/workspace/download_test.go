package main

import (
	"os"
	"path/filepath"
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

// Le tableau des capacités est la seule déclaration : outil, script,
// description et phrase de succès doivent y être complets, sinon l'outil
// existe pour le modèle sans rien lui dire d'utile.
func TestFetchCapabilities_AreComplete(t *testing.T) {
	if len(fetchCapabilities) == 0 {
		t.Fatal("aucune capacité de téléchargement déclarée")
	}

	seen := map[string]bool{}
	for _, capability := range fetchCapabilities {
		if seen[capability.Tool] {
			t.Errorf("l'outil %q est déclaré deux fois", capability.Tool)
		}
		seen[capability.Tool] = true

		if capability.Script == "" || capability.Purpose == "" || capability.Success == "" {
			t.Errorf("capacité %q incomplète: %+v", capability.Tool, capability)
		}
		for _, param := range capability.Params {
			if param.Name == "" || param.Description == "" {
				t.Errorf("paramètre incomplet dans %q: %+v", capability.Tool, param)
			}
		}

		found, ok := lookupFetchCapability(capability.Tool)
		if !ok || found.Script != capability.Script {
			t.Errorf("%q est introuvable par son nom d'outil", capability.Tool)
		}
	}

	if _, ok := lookupFetchCapability("run_command"); ok {
		t.Error("run_command est pris pour une capacité de téléchargement")
	}
}

// Une capacité dont le script n'est pas dans allowed_binaries échoue chez
// l'utilisateur, jamais ici : LeaSH refuse le binaire, et le message parle
// de policy. Ce test est le point de contrôle qui manquerait sinon — et il
// vaut aussi dans l'autre sens, la policy étant l'inventaire de ce que
// l'agent peut atteindre sur le réseau.
func TestFetchCapabilities_MatchTheSandboxPolicy(t *testing.T) {
	policy, err := os.ReadFile(filepath.Join("..", "..", "misc", "toolbox", "policies", "fetch.yaml"))
	if err != nil {
		t.Fatalf("lecture de la policy: %v", err)
	}
	allowed := allowedBinaries(string(policy))

	for _, capability := range fetchCapabilities {
		if !allowed[capability.Script] {
			t.Errorf("%s appelle %q, absent de allowed_binaries de fetch.yaml", capability.Tool, capability.Script)
		}
		script := filepath.Join("..", "..", "misc", "toolbox", capability.Script)
		if _, err := os.Stat(script); err != nil {
			t.Errorf("%s appelle %q, dont le script est absent de misc/toolbox: %v", capability.Tool, capability.Script, err)
		}
	}

	for binary := range allowed {
		var declared bool
		for _, capability := range fetchCapabilities {
			if capability.Script == binary {
				declared = true
			}
		}
		if !declared {
			t.Errorf("%q est autorisé par la policy réseau sans qu'aucune capacité ne l'appelle", binary)
		}
	}
}

// allowedBinaries lit la liste `allowed_binaries` d'une policy LeaSH sans
// analyseur YAML : le bloc est une suite de « - nom », et n'entraîner tout
// le paquet pour ça serait payer cher une lecture de six lignes.
func allowedBinaries(policy string) map[string]bool {
	allowed := map[string]bool{}
	var inside bool
	for _, line := range strings.Split(policy, "\n") {
		switch {
		case strings.HasPrefix(line, "allowed_binaries:"):
			inside = true
		case !inside:
		case strings.HasPrefix(strings.TrimSpace(line), "#"), strings.TrimSpace(line) == "":
		case strings.HasPrefix(strings.TrimSpace(line), "- "):
			allowed[strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))] = true
		default:
			// Une clé de premier niveau : le bloc est fini.
			inside = false
		}
	}
	return allowed
}

// Un paramètre absent prend son défaut ; un paramètre malformé est refusé
// avec un texte qui dit au modèle comment s'en sortir, plutôt que de
// laisser une valeur inattendue atteindre la ligne de commande du script.
func TestFetchParam_Validate(t *testing.T) {
	param := fetchParam{Name: "lang", Default: "fr,en", Pattern: langPattern}

	for _, accepted := range []string{"", "  ", "fr", "fr,en", "pt-BR", "fr-FR,en-US"} {
		got, err := param.validate(accepted)
		if err != nil {
			t.Errorf("validate(%q): %v", accepted, err)
		}
		if accepted == "" || strings.TrimSpace(accepted) == "" {
			if got != "fr,en" {
				t.Errorf("validate(%q) = %q, défaut attendu", accepted, got)
			}
		}
	}

	for _, refused := range []string{"fr en", "fr;en", "$(id)", "../etc", "fr,"} {
		if got, err := param.validate(refused); err == nil {
			t.Errorf("validate(%q) = %q, refus attendu", refused, got)
		}
	}

	// Sans motif, la valeur passe telle quelle : toutes les capacités
	// n'ont pas besoin d'une grammaire.
	free := fetchParam{Name: "libre", Default: "x"}
	if got, err := free.validate("n'importe quoi"); err != nil || got != "n'importe quoi" {
		t.Errorf("validate sans motif = %q, %v", got, err)
	}
}
