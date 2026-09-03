package main

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Le catalogue appartient à L'EXPLOITANT. C'est un fichier lu au
// démarrage, jamais écrit par un membre ni par le modèle : y déposer une
// entrée engage le même niveau de confiance que déposer un binaire dans le
// répertoire des plugins, puisqu'une entrée peut lancer un processus.
//
// Le membre ne fait que deux choses : choisir parmi ce que l'exploitant a
// proposé, et renseigner ses propres identifiants.

// defaultCatalog est le catalogue embarqué, servi quand l'exploitant n'en
// fournit pas. Il ne contient que des entrées vérifiées avec le dépôt.
//
//go:embed catalog/default.yaml
var defaultCatalog []byte

// envCatalogFile désigne le catalogue de l'exploitant. Absent : le
// catalogue embarqué.
const envCatalogFile = "SUBAGENTS_CATALOG"

// Transports supportés, ceux du gestionnaire MCP de l'hôte. "http" parle
// HTTP+SSE (révision 2024-11-05 du protocole), "streamable-http" la
// révision 2025-03-26 et suivantes : un serveur ne parle en général que
// l'un des deux.
const (
	transportStdio      = "stdio"
	transportHTTP       = "http"
	transportStreamable = "streamable-http"
)

var supportedTransports = []string{transportStdio, transportHTTP, transportStreamable}

// namePattern borne les noms d'entrées : ils deviennent des outils
// delegate_to_<nom> côté hôte.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// catalog est le catalogue complet.
type catalog struct {
	Agents []catalogAgent `yaml:"agents"`
}

// catalogAgent est une entrée : un sous-agent activable, et les serveurs
// MCP dont il tire ses outils.
type catalogAgent struct {
	// Name devient delegate_to_<nom> une fois l'entrée activée.
	Name string `yaml:"name"`
	// Description et SystemPrompt partent au modèle : anglais.
	Description  string `yaml:"description"`
	SystemPrompt string `yaml:"system_prompt"`
	// MaxToolCalls borne la boucle d'outils du sous-agent. 0 : défaut de
	// l'hôte.
	MaxToolCalls int `yaml:"max_tool_calls"`
	// Credentials décrit ce que le membre doit saisir. Leurs valeurs
	// remplacent les patrons {{clé}} de la configuration des serveurs :
	// c'est ce qui maintient le cloisonnement, chaque membre se connectant
	// avec les siens.
	Credentials []credentialField `yaml:"credentials"`
	Servers     []serverSpec      `yaml:"servers"`
}

// credentialField est un identifiant demandé au membre. Sa valeur est
// scellée par l'hôte et n'est jamais relue vers l'interface.
type credentialField struct {
	Key   string `yaml:"key"`
	Label string `yaml:"label"`
	Help  string `yaml:"help"`
	// Required : l'entrée n'est pas montée tant qu'il manque.
	Required bool `yaml:"required"`
}

// serverSpec décrit un serveur MCP.
type serverSpec struct {
	Name      string `yaml:"name"`
	Transport string `yaml:"transport"`
	// Command et Env valent pour stdio, URL et Headers pour les deux
	// transports http. Tous portent des patrons {{nom}}.
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	// Version est l'identité de l'installation, pas une décoration :
	// changer ce numéro dans le catalogue est ce qui déclenche une mise à
	// jour. Obligatoire dès qu'Install est déclaré.
	Version string `yaml:"version"`
	// Install dit comment obtenir le binaire d'un serveur stdio à la
	// première activation. Absent : le binaire est censé être déjà là.
	Install *installSpec `yaml:"install"`
	// Files sont des fichiers posés dans {{files}} avant le démarrage —
	// une politique d'autorisation, une configuration. Ils sont réécrits à
	// chaque vérification : le catalogue peut en changer le contenu sans
	// changer de version.
	Files map[string]string `yaml:"files"`
	// ReadOnly épingle les outils à considérer comme des lectures quand le
	// serveur ne les annote pas lui-même. Tout le reste est une écriture,
	// donc soumis à la confirmation de l'hôte.
	ReadOnly []string `yaml:"read_only"`
}

// Noms de patrons fournis par le plugin, interdits comme clé
// d'identifiant : une clé homonyme masquerait la valeur de l'hôte sans
// prévenir.
var reservedTemplateNames = []string{"arch", "bin", "files", "os", "version"}

// loadCatalog lit le catalogue de l'exploitant, ou le catalogue embarqué à
// défaut. Un fichier illisible ou invalide est une ERREUR DE DÉMARRAGE :
// un catalogue à moitié chargé donnerait un membre qui active un agent
// qui ne se montera jamais, sans que rien ne le dise.
func loadCatalog() (catalog, error) {
	raw := defaultCatalog
	source := "catalogue embarqué"

	if path := strings.TrimSpace(os.Getenv(envCatalogFile)); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return catalog{}, fmt.Errorf("lecture de %s: %w", path, err)
		}
		raw, source = data, path
	}

	var cat catalog
	if err := yaml.Unmarshal(raw, &cat); err != nil {
		return catalog{}, fmt.Errorf("%s: %w", source, err)
	}
	if err := cat.validate(); err != nil {
		return catalog{}, fmt.Errorf("%s: %w", source, err)
	}

	return cat, nil
}

// validate refuse un catalogue dont une entrée ne pourrait pas fonctionner,
// en la nommant.
func (c catalog) validate() error {
	seen := map[string]struct{}{}

	for _, agent := range c.Agents {
		if !namePattern.MatchString(agent.Name) {
			return fmt.Errorf("nom d'agent %q invalide (minuscules, chiffres et « _ », commençant par une lettre)", agent.Name)
		}
		if _, dup := seen[agent.Name]; dup {
			return fmt.Errorf("agent %q déclaré deux fois", agent.Name)
		}
		seen[agent.Name] = struct{}{}

		if err := agent.validate(); err != nil {
			return fmt.Errorf("agent %q: %w", agent.Name, err)
		}
	}

	return nil
}

func (a catalogAgent) validate() error {
	if strings.TrimSpace(a.Description) == "" {
		return fmt.Errorf("description requise (elle sert au modèle à décider de déléguer)")
	}
	if strings.TrimSpace(a.SystemPrompt) == "" {
		return fmt.Errorf("system_prompt requis")
	}
	if len(a.Servers) == 0 {
		return fmt.Errorf("aucun serveur MCP déclaré : le sous-agent n'aurait aucun outil")
	}

	declared := map[string]struct{}{}
	for _, cred := range a.Credentials {
		if !namePattern.MatchString(cred.Key) {
			return fmt.Errorf("clé d'identifiant %q invalide", cred.Key)
		}
		if slices.Contains(reservedTemplateNames, cred.Key) {
			return fmt.Errorf("clé d'identifiant %q réservée par le plugin (%s)", cred.Key, strings.Join(reservedTemplateNames, ", "))
		}
		declared[cred.Key] = struct{}{}
	}

	servers := map[string]struct{}{}
	for _, server := range a.Servers {
		if !namePattern.MatchString(server.Name) {
			return fmt.Errorf("nom de serveur %q invalide", server.Name)
		}
		if _, dup := servers[server.Name]; dup {
			return fmt.Errorf("serveur %q déclaré deux fois", server.Name)
		}
		servers[server.Name] = struct{}{}

		if err := server.validate(declared); err != nil {
			return fmt.Errorf("serveur %q: %w", server.Name, err)
		}
	}

	return nil
}

func (s serverSpec) validate(declaredCredentials map[string]struct{}) error {
	if !slices.Contains(supportedTransports, s.Transport) {
		return fmt.Errorf("transport %q non supporté (%s)", s.Transport, strings.Join(supportedTransports, ", "))
	}

	switch s.Transport {
	case transportStdio:
		if len(s.Command) == 0 {
			return fmt.Errorf("command requise en stdio")
		}
	default:
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("url requise en %s", s.Transport)
		}
		if s.Install != nil {
			return fmt.Errorf("install n'a de sens qu'en stdio")
		}
	}

	if s.Install != nil {
		// Sans version, « installer si le binaire est absent » fige le
		// serveur au premier jour : une correction publiée en amont ne
		// serait jamais reprise.
		if strings.TrimSpace(s.Version) == "" {
			return fmt.Errorf("version requise dès qu'une installation est déclarée")
		}
		if strings.ContainsAny(s.Version, `/\`) {
			return fmt.Errorf("version %q invalide (elle nomme un répertoire)", s.Version)
		}
		if s.Install.Fetch == nil && len(s.Install.Command) == 0 {
			return fmt.Errorf("install sans fetch ni command")
		}
		if s.Install.Fetch != nil && s.Install.Fetch.Sha256 == "" && s.Install.Fetch.Checksums == "" {
			return fmt.Errorf("install.fetch sans somme de contrôle (sha256 ou checksums)")
		}
	}

	for name := range s.Files {
		if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
			return fmt.Errorf("nom de fichier %q invalide (pas de chemin)", name)
		}
	}

	// L'installation et les fichiers sont posés UNE FOIS par serveur, pas
	// par membre : un identifiant n'y a pas de valeur, et l'y placer
	// mettrait ceux d'une personne dans un fichier partagé.
	for _, name := range s.installPlaceholders() {
		if !slices.Contains(reservedTemplateNames, name) {
			return fmt.Errorf("patron {{%s}} dans une installation ou un fichier : seuls %s y sont disponibles",
				name, strings.Join(reservedTemplateNames, ", "))
		}
	}

	// Un patron sans source est une erreur de catalogue, pas une surprise
	// à l'exécution : le membre verrait un agent qui refuse de démarrer
	// sans avoir rien à corriger de son côté.
	for _, name := range s.placeholders() {
		if slices.Contains(reservedTemplateNames, name) {
			continue
		}
		if _, ok := declaredCredentials[name]; !ok {
			return fmt.Errorf("patron {{%s}} sans identifiant déclaré (credentials)", name)
		}
	}

	return nil
}

// placeholders retourne les noms de patrons utilisés par le serveur,
// dédupliqués et triés.
func (s serverSpec) placeholders() []string {
	texts := slices.Clone(s.Command)
	texts = append(texts, s.URL)
	for _, key := range slices.Sorted(mapKeys(s.Env)) {
		texts = append(texts, s.Env[key])
	}
	for _, key := range slices.Sorted(mapKeys(s.Headers)) {
		texts = append(texts, s.Headers[key])
	}

	return templateNamesIn(texts...)
}

// installPlaceholders retourne les patrons de l'installation et des
// fichiers déclarés.
func (s serverSpec) installPlaceholders() []string {
	var texts []string
	if s.Install != nil {
		texts = append(texts, s.Install.Command...)
		if s.Install.Fetch != nil {
			texts = append(texts, s.Install.Fetch.URL, s.Install.Fetch.Checksums, s.Install.Fetch.Extract, s.Install.Fetch.Binary)
		}
	}
	for _, key := range slices.Sorted(mapKeys(s.Files)) {
		texts = append(texts, s.Files[key])
	}

	return templateNamesIn(texts...)
}

// needsCredentials dit si le serveur porte au moins un patron
// d'identifiant. C'est ce qui décide de la granularité du cloisonnement :
// une connexion par membre, ou une connexion partagée par organisation.
func (s serverSpec) needsCredentials() bool {
	for _, name := range s.placeholders() {
		if !slices.Contains(reservedTemplateNames, name) {
			return true
		}
	}
	return false
}

// find retrouve une entrée par son nom.
func (c catalog) find(name string) (catalogAgent, bool) {
	for _, agent := range c.Agents {
		if agent.Name == name {
			return agent, true
		}
	}
	return catalogAgent{}, false
}

// missingCredentials liste les identifiants requis que le membre n'a pas
// encore renseignés, dans l'ordre du catalogue.
func (a catalogAgent) missingCredentials(present map[string]bool) []string {
	var missing []string
	for _, cred := range a.Credentials {
		if cred.Required && !present[cred.Key] {
			missing = append(missing, cred.Key)
		}
	}
	return missing
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}
