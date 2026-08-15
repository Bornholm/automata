package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/automata/internal/config"
)

// setBaseEnv positionne toutes les variables d'environnement référencées par
// baseYAML et testdata/valid/config.yaml.
func setBaseEnv(t *testing.T) {
	t.Helper()

	vars := map[string]string{
		"MAIN_MODEL":                "gpt-test",
		"MAIN_API_KEY":              "sk-main",
		"MAIN_BASE_URL":             "https://api.example.test",
		"TRANSCRIPTION_MODEL":       "whisper-test",
		"TRANSCRIPTION_API_KEY":     "sk-transcription",
		"GOOGLE_CALENDAR_MCP_URL":   "https://mcp.example.test/calendar",
		"GOOGLE_CALENDAR_MCP_TOKEN": "mcp-token",
		"ALICE_WHATSAPP_ID":         "alice-wa",
		"ORG_GROUP_CHANNEL_ID":      "group-chan",
		"ORG_GROUP_CALENDAR_ID":     "group-cal",
		"ALICE_PRIVATE_CHANNEL_ID":  "alice-chan",
		"ALICE_CALENDAR_ID":         "alice-cal",
	}

	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// baseYAML est une configuration minimale mais complète, valide, utilisée
// comme point de départ pour les tests de validation négatifs. Les prompts
// sont inline pour éviter d'avoir à créer des fichiers pour chaque scénario.
const baseYAML = `version: 1

organization:
  id: home
  display_name: Maison

storage:
  application:
    driver: sqlite
    path: data/app.sqlite
    pragmas:
      foreign_keys: true
      journal_mode: WAL
      busy_timeout: 5s

courier:
  providers:
    whatsapp:
      type: whatsapp
      session_path: data/courier/whatsapp

audio:
  enabled: true
  transcription_client: transcription
  max_size: 20MiB
  timeout: 2m
  persist_audio: false
  persist_transcription: false

llm_clients:
  main:
    provider: openai
    model: ${MAIN_MODEL}
    api_key: ${MAIN_API_KEY}
    base_url: ${MAIN_BASE_URL}

  transcription:
    provider: openai
    model: ${TRANSCRIPTION_MODEL}
    api_key: ${TRANSCRIPTION_API_KEY}

agents:
  main:
    type: orchestrator
    client: main
    system_prompt:
      inline: "Tu es l'agent principal."
    delegates:
      - agenda
    memory:
      search: true
      remember: true
      forget: true
    limits:
      max_sequential_tool_calls: 8
      max_actions_per_turn: 10
      tool_timeout: 30s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 64KiB

  agenda:
    type: specialist
    client: main
    system_prompt:
      inline: "Tu es l'agent agenda."
    mcp_servers:
      - google-calendar
    capabilities:
      - calendar.personal.read
      - calendar.personal.write
    limits:
      max_sequential_tool_calls: 4
      max_actions_per_turn: 5
      tool_timeout: 15s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 32KiB

mcp_servers:
  google-calendar:
    transport: http
    url: ${GOOGLE_CALENDAR_MCP_URL}
    headers:
      Authorization: Bearer ${GOOGLE_CALENDAR_MCP_TOKEN}

memory:
  store:
    driver: sqlite
    path: data/amoxtli.sqlite

  indexes:
    - id: lexical
      type: bleve
      path: data/memory.bleve
      weight: 1

  policies:
    private_can_write_org: false
    org_readable_by_children: true

identities:
  roles:
    adult:
      permissions:
        - memory.personal.read
        - memory.personal.write
        - memory.personal.delete
        - calendar.personal.read
        - calendar.personal.write

    scheduled-reader:
      permissions:
        - memory.org.read

  principals:
    - id: alice
      kind: human
      display_name: Alice
      roles: [adult]

    - id: scheduler-readonly
      kind: service
      display_name: Planificateur
      roles: [scheduled-reader]

origins:
  - provider: whatsapp
    external_user_id: ${ALICE_WHATSAPP_ID}
    principal_id: alice

channels:
  - provider: whatsapp
    channel_id: ${ORG_GROUP_CHANNEL_ID}
    display_name: Groupe principal
    kind: group
    org_id: home
    scope: group
    scope_id: main-group
    activation: mention
    members:
      - alice
    resources:
      calendar: ${ORG_GROUP_CALENDAR_ID}

  - provider: whatsapp
    channel_id: ${ALICE_PRIVATE_CHANNEL_ID}
    kind: private
    org_id: home
    scope: personal
    scope_id: alice
    principal_id: alice
    resources:
      calendar: ${ALICE_CALENDAR_ID}

schedules:
  - id: morning-summary
    enabled: true

    schedule:
      cron: "0 7 * * *"
      timezone: Europe/Paris

    execution:
      principal_id: scheduler-readonly
      org_id: home
      scope: org
      scope_id: home
      agent: main
      prompt: |
        Prépare un résumé des événements et tâches importants de la journée.
      actions:
        policy: read_only

    delivery:
      provider: whatsapp
      channel_id: ${ORG_GROUP_CHANNEL_ID}
      mode: on_content

    concurrency:
      policy: forbid
      timeout: 10m
`

// writeYAML écrit le contenu donné dans un fichier config.yaml sous un
// nouveau répertoire temporaire et retourne son chemin.
func writeYAML(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("écriture du fichier de configuration: %v", err)
	}

	return path
}

func TestLoad_ValidFromTestdata(t *testing.T) {
	setBaseEnv(t)

	cfg, err := config.Load("testdata/valid/config.yaml")
	if err != nil {
		t.Fatalf("Load: erreur inattendue: %v", err)
	}

	if cfg.Organization.ID != "home" {
		t.Errorf("Organization.ID = %q, attendu %q", cfg.Organization.ID, "home")
	}

	if len(cfg.Agents) != 2 {
		t.Errorf("len(Agents) = %d, attendu 2", len(cfg.Agents))
	}

	main, ok := cfg.Agents["main"]
	if !ok {
		t.Fatalf("agent 'main' manquant")
	}

	if !strings.Contains(main.SystemPrompt.Content, "Agent principal") {
		t.Errorf("SystemPrompt.Content = %q, attendu contenu du fichier prompts/main.md", main.SystemPrompt.Content)
	}

	if !filepath.IsAbs(main.SystemPrompt.File) {
		t.Errorf("SystemPrompt.File = %q, attendu un chemin absolu", main.SystemPrompt.File)
	}

	agenda := cfg.Agents["agenda"]
	if agenda.SystemPrompt.Content != "Tu es l'agent agenda." {
		t.Errorf("agenda.SystemPrompt.Content = %q", agenda.SystemPrompt.Content)
	}
}

func TestLoad_RelativePathResolvedAgainstConfigDir(t *testing.T) {
	setBaseEnv(t)

	// Se placer dans un répertoire différent du répertoire de configuration
	// pour vérifier que la résolution ne dépend pas du cwd du process.
	otherDir := t.TempDir()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("restauration du cwd: %v", err)
		}
	})

	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	configPath, err := filepath.Abs(filepath.Join(cwd, "testdata", "valid", "config.yaml"))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load: erreur inattendue: %v", err)
	}

	wantFile, err := filepath.Abs(filepath.Join(cwd, "testdata", "valid", "prompts", "main.md"))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	main := cfg.Agents["main"]
	if main.SystemPrompt.File != wantFile {
		t.Errorf("SystemPrompt.File = %q, attendu %q", main.SystemPrompt.File, wantFile)
	}

	if !strings.Contains(main.SystemPrompt.Content, "Agent principal") {
		t.Errorf("SystemPrompt.Content = %q, attendu contenu du fichier", main.SystemPrompt.Content)
	}
}

func TestLoad_InvalidYAMLSyntax(t *testing.T) {
	setBaseEnv(t)

	path := writeYAML(t, "version: 1\nagents: [this is not valid: yaml\n")

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un YAML invalide")
	}
}

func TestLoad_MissingEnvVar(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "model: ${MAIN_MODEL}", "model: ${DOES_NOT_EXIST}", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour une variable d'environnement absente")
	}

	if !strings.Contains(err.Error(), "DOES_NOT_EXIST") {
		t.Errorf("erreur = %v, attendu mention de DOES_NOT_EXIST", err)
	}
}

func TestLoad_DuplicateChannelID(t *testing.T) {
	setBaseEnv(t)

	extraChannel := `
  - provider: whatsapp
    channel_id: ${ORG_GROUP_CHANNEL_ID}
    kind: private
    org_id: home
    scope: personal
    scope_id: alice
    principal_id: alice
`
	content := strings.Replace(baseYAML, "\nschedules:", extraChannel+"\nschedules:", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un canal dupliqué")
	}

	if !strings.Contains(err.Error(), "doublon") {
		t.Errorf("erreur = %v, attendu mention de doublon", err)
	}
}

func TestLoad_UnknownAgentDelegate(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "    delegates:\n      - agenda", "    delegates:\n      - inexistant", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un agent délégué inconnu")
	}

	if !strings.Contains(err.Error(), "inexistant") {
		t.Errorf("erreur = %v, attendu mention de 'inexistant'", err)
	}
}

func TestLoad_UnknownScheduleAgent(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "      agent: main", "      agent: inexistant", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un agent de planification inconnu")
	}

	if !strings.Contains(err.Error(), "inexistant") {
		t.Errorf("erreur = %v, attendu mention de 'inexistant'", err)
	}
}

func TestLoad_UnknownMCPServer(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "    mcp_servers:\n      - google-calendar", "    mcp_servers:\n      - inexistant", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un serveur mcp inconnu")
	}

	if !strings.Contains(err.Error(), "inexistant") {
		t.Errorf("erreur = %v, attendu mention de 'inexistant'", err)
	}
}

func TestLoad_DelegationCycle(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, `  agenda:
    type: specialist
    client: main
    system_prompt:
      inline: "Tu es l'agent agenda."
    mcp_servers:
      - google-calendar
    capabilities:
      - calendar.personal.read
      - calendar.personal.write`, `  agenda:
    type: specialist
    client: main
    system_prompt:
      inline: "Tu es l'agent agenda."
    delegates:
      - main
    mcp_servers:
      - google-calendar
    capabilities:
      - calendar.personal.read
      - calendar.personal.write`, 1)

	// main délègue déjà à agenda dans baseYAML ; agenda délègue maintenant à
	// main, formant un cycle. main doit rester référencé comme specialist
	// cible valide pour isoler le test sur le cycle plutôt que sur le type.
	content = strings.Replace(content, "    type: orchestrator", "    type: specialist", 1)

	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un cycle de délégation")
	}

	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("erreur = %v, attendu mention de 'cycle'", err)
	}
}

func TestLoad_PromptFileMissing(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, `    system_prompt:
      inline: "Tu es l'agent principal."`, `    system_prompt:
      file: prompts/does-not-exist.md`, 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un prompt absent")
	}

	if !strings.Contains(err.Error(), "introuvable") {
		t.Errorf("erreur = %v, attendu mention de fichier introuvable", err)
	}
}

func TestLoad_SystemPromptFileAndInlineBoth(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, `    system_prompt:
      inline: "Tu es l'agent principal."`, `    system_prompt:
      inline: "Tu es l'agent principal."
      file: prompts/main.md`, 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue quand file et inline sont tous deux définis")
	}

	if !strings.Contains(err.Error(), "mutuellement exclusifs") {
		t.Errorf("erreur = %v, attendu mention de 'mutuellement exclusifs'", err)
	}
}

func TestLoad_SystemPromptNeitherFileNorInline(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, `    system_prompt:
      inline: "Tu es l'agent principal."`, `    system_prompt: {}`, 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue quand ni file ni inline ne sont définis")
	}

	if !strings.Contains(err.Error(), "requis") {
		t.Errorf("erreur = %v, attendu mention de 'requis'", err)
	}
}

func TestLoad_InvalidTimezone(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "timezone: Europe/Paris", "timezone: Not/AZone", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un fuseau horaire invalide")
	}

	if !strings.Contains(err.Error(), "fuseau") {
		t.Errorf("erreur = %v, attendu mention de fuseau", err)
	}
}

func TestLoad_InvalidCron(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, `cron: "0 7 * * *"`, `cron: "not a cron expression"`, 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour une expression cron invalide")
	}

	if !strings.Contains(err.Error(), "cron") {
		t.Errorf("erreur = %v, attendu mention de cron", err)
	}
}

func TestLoad_UnknownPermission(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "- memory.org.read", "- memory.org.teleport", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour une permission inconnue")
	}

	if !strings.Contains(err.Error(), "permission") {
		t.Errorf("erreur = %v, attendu mention de permission", err)
	}
}

func TestLoad_GroupChannelWithoutMembers(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "    members:\n      - alice\n    resources:\n      calendar: ${ORG_GROUP_CALENDAR_ID}", "    resources:\n      calendar: ${ORG_GROUP_CALENDAR_ID}", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un canal de groupe sans membres")
	}

	if !strings.Contains(err.Error(), "members") {
		t.Errorf("erreur = %v, attendu mention de members", err)
	}
}

func TestLoad_PrivateChannelWithoutPrincipal(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "    scope_id: alice\n    principal_id: alice\n    resources:\n      calendar: ${ALICE_CALENDAR_ID}", "    scope_id: alice\n    resources:\n      calendar: ${ALICE_CALENDAR_ID}", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour un canal privé sans principal")
	}

	if !strings.Contains(err.Error(), "principal_id") {
		t.Errorf("erreur = %v, attendu mention de principal_id", err)
	}
}

func TestLoad_AggregatesMultipleErrors(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "version: 1", "version: 2", 1)
	content = strings.Replace(content, "- memory.org.read", "- memory.org.teleport", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue")
	}

	var valErrs config.ValidationErrors
	if !errors.As(err, &valErrs) {
		t.Fatalf("erreur = %v (%T), attendu config.ValidationErrors", err, err)
	}

	if len(valErrs) < 2 {
		t.Errorf("len(ValidationErrors) = %d, attendu au moins 2", len(valErrs))
	}
}

func TestLoad_InvalidAgentLimitZero(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "max_actions_per_turn: 10", "max_actions_per_turn: 0", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour une limite max_actions_per_turn à zéro")
	}

	if !strings.Contains(err.Error(), "max_actions_per_turn") {
		t.Errorf("erreur = %v, attendu mention de max_actions_per_turn", err)
	}
}

func TestLoad_InvalidAgentLimitNegative(t *testing.T) {
	setBaseEnv(t)

	content := strings.Replace(baseYAML, "max_sequential_tool_calls: 8", "max_sequential_tool_calls: -1", 1)
	path := writeYAML(t, content)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load: erreur attendue pour une limite max_sequential_tool_calls négative")
	}

	if !strings.Contains(err.Error(), "max_sequential_tool_calls") {
		t.Errorf("erreur = %v, attendu mention de max_sequential_tool_calls", err)
	}
}
