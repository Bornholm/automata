package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/config"
)

// scriptedAnswers assemble les réponses du wizard. Une chaîne vide accepte la
// valeur par défaut proposée.
func scriptedAnswers(answers ...string) string {
	return strings.Join(answers, "\n") + "\n"
}

// setEnvFromExample déclare toutes les variables listées par le fichier
// d'environnement généré, avec une valeur factice : c'est exactement ce que
// l'opérateur fait avant de démarrer.
//
// Chaque variable reçoit une valeur DISTINCTE, dérivée de son nom. Une valeur
// commune ferait apparaître deux canaux ou deux origines identiques, que la
// validation rejette à raison.
func setEnvFromExample(t *testing.T, envExample string) {
	t.Helper()

	scanner := bufio.NewScanner(strings.NewReader(envExample))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, _, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("ligne d'environnement invalide: %q", line)
		}

		name = strings.TrimSpace(name)
		// Certaines valeurs sont soumises à une longueur minimale (secret
		// de session, clé de chiffrement) : le remplissage évite d'avoir
		// à connaître ici lesquelles.
		value := strings.ToLower(name) + "-de-test"
		for len(value) < 40 {
			value += "x"
		}
		t.Setenv(name, value)
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("lecture du fichier d'environnement: %v", err)
	}
}

// TestConfigInit_ProducesLoadableConfig est le test qui compte : la
// configuration produite par le wizard doit passer le chargement ET la
// validation complets, sans retouche. C'est la seule preuve qu'elle est
// utilisable de bout en bout.
func TestConfigInit_ProducesLoadableConfig(t *testing.T) {
	dir := t.TempDir()

	input := scriptedAnswers(
		// Organisation et emplacements.
		"", "", "/data", "prompts",
		// Interface web, puis plugins : activés, valeurs par défaut.
		"o", "", "o", "",
		// Modèle.
		"openai",
		// Fonctionnalités : audio, pièces jointes, observabilité.
		"o", "o", "o", "",
		// Alice, puis Léo.
		"alice", "", "adult", "o",
		"leo", "", "child", "n",
		// Canal privé pour chacun, puis un groupe.
		"o", "o", "o", "main-group",
		// Agenda avec un jeton par personne.
		"o", "o", "o",
		// Pas de recherche, pas de todo.
		"n", "n",
		// Une tâche planifiée, tout par défaut.
		"o", "", "", "", "",
	)

	configYAML, envExample, err := runConfigInit(strings.NewReader(input), &strings.Builder{})
	if err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	// Les prompts référencés doivent exister : le chargement les lit.
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o750); err != nil {
		t.Fatalf("création du répertoire de prompts: %v", err)
	}
	for _, name := range []string{"main.md", "agenda.md"} {
		if err := os.WriteFile(filepath.Join(promptsDir, name), []byte("Tu es un agent de test."), 0o600); err != nil {
			t.Fatalf("écriture du prompt %s: %v", name, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("écriture de la configuration: %v", err)
	}

	setEnvFromExample(t, envExample)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("la configuration générée devrait être valide, obtenu:\n%v\n\n--- configuration ---\n%s", err, configYAML)
	}

	// Quelques vérifications de fond sur ce qui a été demandé.
	if cfg.Organization.ID != "home" {
		t.Errorf("organization.id = %q", cfg.Organization.ID)
	}
	if len(cfg.Identities.Principals) != 3 {
		t.Fatalf("principaux: got %d, expected 3 (alice, leo, scheduler-readonly)", len(cfg.Identities.Principals))
	}
	if len(cfg.Channels) != 3 {
		t.Fatalf("canaux: got %d, expected 3 (deux privés, un groupe)", len(cfg.Channels))
	}
	if len(cfg.Schedules) != 1 {
		t.Fatalf("tâches planifiées: got %d, expected 1", len(cfg.Schedules))
	}
	if !cfg.Attachments.Enabled || !cfg.Audio.Enabled || !cfg.Observability.Enabled {
		t.Error("audio, pièces jointes et observabilité devaient être activés")
	}
}

// TestConfigInit_PerPrincipalMCPTokens vérifie que le choix « chaque personne
// a son propre jeton » produit bien une surcharge par principal, ce qui leur
// donne des connexions MCP isolées.
func TestConfigInit_PerPrincipalMCPTokens(t *testing.T) {
	dir := t.TempDir()

	input := scriptedAnswers(
		"", "", "/data", "prompts",
		"o", "", "o", "",
		"openai",
		"n", "n", "n",
		"alice", "", "adult", "o",
		"leo", "", "child", "n",
		"o", "o", "n",
		// Agenda, avec jeton, propre à chaque personne.
		"o", "o", "o",
		"n", "n",
		"n",
	)

	configYAML, envExample, err := runConfigInit(strings.NewReader(input), &strings.Builder{})
	if err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o750); err != nil {
		t.Fatalf("création du répertoire de prompts: %v", err)
	}
	for _, name := range []string{"main.md", "agenda.md"} {
		if err := os.WriteFile(filepath.Join(promptsDir, name), []byte("Tu es un agent de test."), 0o600); err != nil {
			t.Fatalf("écriture du prompt: %v", err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("écriture de la configuration: %v", err)
	}

	setEnvFromExample(t, envExample)

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("configuration invalide:\n%v\n\n%s", err, configYAML)
	}

	for _, principal := range cfg.Identities.Principals {
		override, ok := principal.MCP["google-calendar"]
		if !ok {
			t.Fatalf("le principal %q devrait déclarer sa propre connexion google-calendar", principal.ID)
		}
		if override.Headers["Authorization"] == "" {
			t.Errorf("le principal %q n'a pas d'en-tête d'autorisation", principal.ID)
		}
	}

	// Les jetons doivent être distincts, sinon l'isolation ne sert à rien.
	first := cfg.Identities.Principals[0].MCP["google-calendar"].Headers["Authorization"]
	second := cfg.Identities.Principals[1].MCP["google-calendar"].Headers["Authorization"]

	if first == second {
		t.Error("les deux principaux référencent le même jeton : leurs connexions ne seraient pas réellement séparées")
	}
}

// TestConfigInit_SharedMCPTokenStaysOnServer couvre l'autre branche : un
// serveur sans authentification par utilisateur garde son jeton commun.
func TestConfigInit_SharedMCPTokenStaysOnServer(t *testing.T) {
	input := scriptedAnswers(
		"", "", "/data", "prompts",
		"o", "", "o", "",
		"openai",
		"n", "n", "n",
		"alice", "", "adult", "n",
		"o", "n",
		// Agenda, avec jeton, mais partagé.
		"o", "o", "n",
		"n", "n",
		"n",
	)

	configYAML, _, err := runConfigInit(strings.NewReader(input), &strings.Builder{})
	if err != nil {
		t.Fatalf("runConfigInit: %v", err)
	}

	if !strings.Contains(configYAML, "GOOGLE_CALENDAR_MCP_TOKEN") {
		t.Error("le jeton commun devrait être déclaré au niveau du serveur")
	}
	if strings.Contains(configYAML, "ALICE_GOOGLE_CALENDAR_TOKEN") {
		t.Error("aucune surcharge par principal ne devait être générée")
	}
}

// TestConfigInit_NoPrincipalIsRefused vérifie qu'une configuration sans
// personne déclarée n'est pas écrite : elle ne répondrait à personne.
func TestConfigInit_NoPrincipalIsRefused(t *testing.T) {
	// L'entrée s'épuise avant la première question sur les personnes.
	input := scriptedAnswers("", "", "/data", "prompts", "o", "", "o", "", "openai", "n", "n", "n")

	if _, _, err := runConfigInit(strings.NewReader(input), &strings.Builder{}); err == nil {
		t.Fatal("une configuration sans principal devrait être refusée")
	}
}

// TestWriteIfAbsent_RefusesToOverwrite protège une configuration en place
// contre une génération lancée par mégarde.
func TestWriteIfAbsent_RefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := writeIfAbsent(path, "version: 1\n"); err != nil {
		t.Fatalf("première écriture: %v", err)
	}

	if err := writeIfAbsent(path, "version: 2\n"); err == nil {
		t.Fatal("la seconde écriture aurait dû être refusée")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lecture: %v", err)
	}
	if string(content) != "version: 1\n" {
		t.Errorf("le fichier existant a été modifié: %q", content)
	}
}
