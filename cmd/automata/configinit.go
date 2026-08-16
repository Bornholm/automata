package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// wizard pose des questions sur in et écrit ses invites sur out.
//
// Il lit ligne à ligne plutôt que d'exiger un terminal : un opérateur peut
// donc rejouer une génération en redirigeant un fichier de réponses, et les
// tests scriptent la même chose sans TTY.
type wizard struct {
	in   *bufio.Scanner
	out  io.Writer
	fail error
}

func newWizard(in io.Reader, out io.Writer) *wizard {
	scanner := bufio.NewScanner(in)
	// Une réponse peut être une URL longue ; la taille par défaut de
	// bufio.Scanner (64 Kio) suffit largement, mais on la fixe explicitement
	// pour ne pas dépendre d'un défaut.
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)

	return &wizard{in: scanner, out: out}
}

// ask pose une question et retourne la réponse, ou def si l'opérateur valide
// sans rien saisir.
//
// Une fois l'entrée épuisée, toutes les questions suivantes retournent leur
// valeur par défaut et l'erreur est mémorisée : le wizard ne boucle pas
// indéfiniment sur un flux fermé.
func (w *wizard) ask(question, def string) string {
	if w.fail != nil {
		return def
	}

	if def != "" {
		fmt.Fprintf(w.out, "%s [%s] : ", question, def)
	} else {
		fmt.Fprintf(w.out, "%s : ", question)
	}

	if !w.in.Scan() {
		if err := w.in.Err(); err != nil {
			w.fail = err
		} else {
			w.fail = io.EOF
		}

		fmt.Fprintln(w.out)

		return def
	}

	answer := strings.TrimSpace(w.in.Text())
	if answer == "" {
		return def
	}

	return answer
}

// askRequired repose la question tant que la réponse est vide.
func (w *wizard) askRequired(question string) string {
	for {
		answer := w.ask(question, "")
		if answer != "" {
			return answer
		}

		if w.fail != nil {
			return ""
		}

		fmt.Fprintln(w.out, "  une valeur est requise.")
	}
}

// askYesNo pose une question fermée.
func (w *wizard) askYesNo(question string, def bool) bool {
	defLabel := "o/N"
	if def {
		defLabel = "O/n"
	}

	for {
		answer := strings.ToLower(w.ask(question+" (o/n)", defLabel))

		switch answer {
		case "o", "oui", "y", "yes":
			return true
		case "n", "non", "no":
			return false
		case strings.ToLower(defLabel):
			return def
		}

		if w.fail != nil {
			return def
		}

		fmt.Fprintln(w.out, "  répondre par o ou n.")
	}
}

// askChoice impose une réponse parmi choices.
func (w *wizard) askChoice(question string, choices []string, def string) string {
	for {
		answer := w.ask(fmt.Sprintf("%s (%s)", question, strings.Join(choices, "|")), def)

		for _, choice := range choices {
			if strings.EqualFold(answer, choice) {
				return choice
			}
		}

		if w.fail != nil {
			return def
		}

		fmt.Fprintf(w.out, "  valeurs acceptées : %s.\n", strings.Join(choices, ", "))
	}
}

func (w *wizard) section(title string) {
	fmt.Fprintf(w.out, "\n── %s\n\n", title)
}

// envVarName transforme un identifiant libre en nom de variable
// d'environnement : "google calendar" devient "GOOGLE_CALENDAR".
func envVarName(parts ...string) string {
	var b strings.Builder

	for i, part := range parts {
		if i > 0 {
			b.WriteByte('_')
		}

		for _, r := range strings.ToUpper(part) {
			switch {
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
	}

	return b.String()
}

// initAnswers rassemble les réponses du wizard, telles que le gabarit les
// consomme.
type initAnswers struct {
	OrgID          string
	OrgDisplayName string
	DataDir        string
	PromptsDir     string

	LLMProvider string
	LLMModelVar string
	LLMKeyVar   string
	LLMBaseVar  string

	Audio           bool
	AudioModelVar   string
	AudioKeyVar     string
	Attachments     bool
	Observability   bool
	ObservabilityAt string

	Principals []initPrincipal
	Channels   []initChannel
	Servers    []initServer
	Agents     []initAgent
	Schedule   *initSchedule

	// EnvVars liste toutes les variables référencées, pour le fichier
	// d'exemple d'environnement.
	EnvVars []string
}

type initPrincipal struct {
	ID          string
	DisplayName string
	Role        string
	ExternalVar string
	MCP         []initPrincipalMCP
}

type initPrincipalMCP struct {
	Server   string
	TokenVar string
}

type initChannel struct {
	Name        string
	IDVar       string
	Kind        string
	Scope       string
	ScopeID     string
	PrincipalID string
	Members     []string
	// Resources associe une clé de ressource (déclarée par un serveur MCP) au
	// nom de la variable d'environnement portant sa valeur.
	Resources map[string]string
}

type initServer struct {
	Name     string
	URLVar   string
	TokenVar string

	// Politique applicative du serveur. Vide pour un service en lecture
	// seule, dont tous les outils s'exécutent directement.
	ResourceKey      string
	ResourceParam    string
	PermissionDomain string
	RequireRFC3339   bool
	DedupeWrites     bool
}

type initAgent struct {
	Name       string
	PromptFile string
	Server     string
}

type initSchedule struct {
	ID        string
	Cron      string
	Timezone  string
	Prompt    string
	ChannelID string
}

// runConfigInit conduit l'entretien et retourne le contenu du fichier de
// configuration ainsi que celui du fichier d'environnement d'exemple.
func runConfigInit(in io.Reader, out io.Writer) (configYAML string, envExample string, err error) {
	w := newWizard(in, out)

	fmt.Fprintln(out, "Génération d'une configuration Automata.")
	fmt.Fprintln(out, "Validez par Entrée pour accepter la valeur entre crochets.")
	fmt.Fprintln(out, "Aucun secret n'est écrit dans le fichier : seules des références")
	fmt.Fprintln(out, "d'environnement le sont, listées à la fin.")

	answers := &initAnswers{}

	askOrganization(w, answers)
	askLLM(w, answers)
	askOptions(w, answers)
	askPrincipals(w, answers)
	askChannels(w, answers)
	askSpecialists(w, answers)
	askSchedule(w, answers)

	if w.fail != nil && w.fail != io.EOF {
		return "", "", fmt.Errorf("lecture des réponses: %w", w.fail)
	}

	if len(answers.Principals) == 0 {
		return "", "", fmt.Errorf("au moins un principal est requis pour produire une configuration exploitable")
	}

	answers.collectEnvVars()

	configYAML, err = renderTemplate(configTemplate, answers)
	if err != nil {
		return "", "", err
	}

	envExample, err = renderTemplate(envTemplate, answers)
	if err != nil {
		return "", "", err
	}

	return configYAML, envExample, nil
}

func askOrganization(w *wizard, a *initAnswers) {
	w.section("Organisation")

	a.OrgID = w.ask("Identifiant de l'organisation", "home")
	a.OrgDisplayName = w.ask("Nom affiché", "Maison")

	w.section("Emplacements")

	a.DataDir = w.ask("Répertoire des données (bases, index, session WhatsApp)", "/data")
	a.PromptsDir = w.ask("Répertoire des prompts, relatif au fichier de configuration", "../prompts")
}

func askLLM(w *wizard, a *initAnswers) {
	w.section("Modèle de langage")

	a.LLMProvider = w.askChoice("Fournisseur", []string{"openai", "mistral", "openrouter"}, "openai")
	a.LLMModelVar = envVarName("main", "model")
	a.LLMKeyVar = envVarName("main", "api", "key")
	a.LLMBaseVar = envVarName("main", "base", "url")

	fmt.Fprintf(w.out, "  le modèle et la clé seront lus dans ${%s} et ${%s}.\n", a.LLMModelVar, a.LLMKeyVar)
}

func askOptions(w *wizard, a *initAnswers) {
	w.section("Fonctionnalités")

	a.Audio = w.askYesNo("Transcrire les messages vocaux", true)
	if a.Audio {
		a.AudioModelVar = envVarName("transcription", "model")
		a.AudioKeyVar = envVarName("transcription", "api", "key")
	}

	a.Attachments = w.askYesNo("Accepter les images et documents joints", true)

	a.Observability = w.askYesNo("Exposer santé et métriques en local", true)
	if a.Observability {
		a.ObservabilityAt = w.ask("Adresse d'écoute", "127.0.0.1:9090")
	}
}

func askPrincipals(w *wizard, a *initAnswers) {
	w.section("Personnes")

	fmt.Fprintln(w.out, "Déclarez chaque personne autorisée à parler à l'assistant.")
	fmt.Fprintln(w.out, "Un message venant d'ailleurs est ignoré sans appel au modèle.")
	fmt.Fprintln(w.out)

	for {
		id := w.askRequired("Identifiant court (ex: alice)")
		if id == "" {
			return
		}

		principal := initPrincipal{
			ID:          id,
			DisplayName: w.ask("Nom affiché", strings.ToUpper(id[:1])+id[1:]),
			Role:        w.askChoice("Rôle", []string{"adult", "child"}, "adult"),
			ExternalVar: envVarName(id, "whatsapp", "id"),
		}

		fmt.Fprintf(w.out, "  son identifiant WhatsApp sera lu dans ${%s}.\n", principal.ExternalVar)

		a.Principals = append(a.Principals, principal)

		if !w.askYesNo("Ajouter une autre personne", false) {
			return
		}
	}
}

func askChannels(w *wizard, a *initAnswers) {
	w.section("Canaux")

	for i, principal := range a.Principals {
		if !w.askYesNo(fmt.Sprintf("Créer un canal privé pour %s", principal.DisplayName), true) {
			continue
		}

		a.Channels = append(a.Channels, initChannel{
			Name:        principal.ID + "-priv",
			IDVar:       envVarName(principal.ID, "private", "channel", "id"),
			Kind:        "private",
			Scope:       "personal",
			ScopeID:     principal.ID,
			PrincipalID: principal.ID,
		})

		_ = i
	}

	if !w.askYesNo("Déclarer un canal de groupe", len(a.Principals) > 1) {
		return
	}

	scopeID := w.ask("Identifiant de portée du groupe", "main-group")

	members := make([]string, 0, len(a.Principals))
	for _, principal := range a.Principals {
		members = append(members, principal.ID)
	}

	a.Channels = append(a.Channels, initChannel{
		Name:    "group",
		IDVar:   envVarName("org", "group", "channel", "id"),
		Kind:    "group",
		Scope:   "group",
		ScopeID: scopeID,
		Members: members,
	})
}

func askSpecialists(w *wizard, a *initAnswers) {
	w.section("Spécialistes")

	fmt.Fprintln(w.out, "Chaque spécialiste est branché sur un serveur MCP en HTTP.")
	fmt.Fprintln(w.out, "Vous pourrez en ajouter d'autres plus tard : voir docs/agents.md.")
	fmt.Fprintln(w.out)

	// Ce catalogue est une commodité d'amorçage, pas une liste de domaines
	// connus de l'application : chaque entrée ne fait que pré-remplir une
	// politique que l'opérateur aurait pu écrire à la main. N'importe quel
	// autre service se déclare de la même façon, sans toucher au code.
	catalogue := []struct {
		server           string
		agent            string
		label            string
		asks             bool
		resourceKey      string
		resourceParam    string
		permissionDomain string
		requireRFC3339   bool
		dedupeWrites     bool
	}{
		{"google-calendar", "agenda", "Agenda (lecture et création d'événements)", true, "calendar", "calendar_id", "calendar", true, false},
		{"internet-search", "research", "Recherche Internet", false, "", "", "", false, false},
		{"todo", "todo", "Listes de tâches", true, "todo", "list_id", "todo", false, true},
	}

	for _, entry := range catalogue {
		if !w.askYesNo("Activer le spécialiste "+entry.label, false) {
			continue
		}

		server := initServer{
			Name:             entry.server,
			URLVar:           envVarName(entry.server, "mcp", "url"),
			ResourceKey:      entry.resourceKey,
			ResourceParam:    entry.resourceParam,
			PermissionDomain: entry.permissionDomain,
			RequireRFC3339:   entry.requireRFC3339,
			DedupeWrites:     entry.dedupeWrites,
		}

		if w.askYesNo("  ce serveur demande-t-il un jeton d'authentification", entry.asks) {
			perPrincipal := w.askYesNo("  chaque personne a-t-elle son propre jeton", entry.asks)

			if perPrincipal {
				for i := range a.Principals {
					a.Principals[i].MCP = append(a.Principals[i].MCP, initPrincipalMCP{
						Server:   entry.server,
						TokenVar: envVarName(a.Principals[i].ID, entry.server, "token"),
					})
				}

				fmt.Fprintln(w.out, "  chaque personne obtiendra sa propre connexion, isolée des autres.")
			} else {
				server.TokenVar = envVarName(entry.server, "mcp", "token")
			}
		}

		a.Servers = append(a.Servers, server)
		a.Agents = append(a.Agents, initAgent{
			Name:       entry.agent,
			PromptFile: a.PromptsDir + "/" + entry.agent + ".md",
			Server:     entry.server,
		})
	}

	// Chaque serveur déclarant une ressource impose aux canaux de la fournir
	// pour leur portée. La clé n'est pas connue d'avance : elle vient de la
	// politique du serveur.
	for _, server := range a.Servers {
		if server.ResourceKey == "" {
			continue
		}

		for i := range a.Channels {
			if a.Channels[i].Resources == nil {
				a.Channels[i].Resources = map[string]string{}
			}

			a.Channels[i].Resources[server.ResourceKey] = envVarName(a.Channels[i].ScopeID, server.ResourceKey, "id")
		}
	}
}

func askSchedule(w *wizard, a *initAnswers) {
	w.section("Tâche planifiée")

	if !w.askYesNo("Ajouter un résumé quotidien", false) {
		return
	}

	channelID := ""
	for _, channel := range a.Channels {
		channelID = channel.IDVar
		if channel.Kind == "group" {
			break
		}
	}

	if channelID == "" {
		fmt.Fprintln(w.out, "  aucun canal déclaré, tâche ignorée.")
		return
	}

	a.Schedule = &initSchedule{
		ID:        w.ask("Identifiant de la tâche", "morning-summary"),
		Cron:      w.ask("Expression cron", "0 7 * * *"),
		Timezone:  w.ask("Fuseau horaire", "Europe/Paris"),
		Prompt:    w.ask("Consigne", "Prépare un résumé des événements et tâches importants de la journée."),
		ChannelID: channelID,
	}
}

// collectEnvVars rassemble, sans doublon, toutes les variables référencées.
func (a *initAnswers) collectEnvVars() {
	seen := map[string]bool{}

	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			a.EnvVars = append(a.EnvVars, name)
		}
	}

	add(a.LLMModelVar)
	add(a.LLMKeyVar)
	add(a.LLMBaseVar)
	add(a.AudioModelVar)
	add(a.AudioKeyVar)

	for _, principal := range a.Principals {
		add(principal.ExternalVar)
		for _, override := range principal.MCP {
			add(override.TokenVar)
		}
	}

	for _, channel := range a.Channels {
		add(channel.IDVar)
		for _, key := range sortedResourceKeys(channel.Resources) {
			add(channel.Resources[key])
		}
	}

	for _, server := range a.Servers {
		add(server.URLVar)
		add(server.TokenVar)
	}

	sort.Strings(a.EnvVars)
}

func renderTemplate(text string, a *initAnswers) (string, error) {
	tmpl, err := template.New("config").Parse(text)
	if err != nil {
		return "", fmt.Errorf("gabarit invalide: %w", err)
	}

	var b strings.Builder
	if err := tmpl.Execute(&b, a); err != nil {
		return "", fmt.Errorf("génération: %w", err)
	}

	return b.String(), nil
}

// writeIfAbsent écrit content dans path, en refusant d'écraser un fichier
// existant : une configuration en production ne doit jamais disparaître
// derrière une génération lancée par mégarde.
func writeIfAbsent(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s existe déjà, aucun fichier n'a été écrit", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("vérification de %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("création du répertoire %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("écriture de %s: %w", path, err)
	}

	return nil
}

// sortedResourceKeys retourne les clés de resources triées, pour que la
// génération soit reproductible d'une exécution à l'autre.
func sortedResourceKeys(resources map[string]string) []string {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
