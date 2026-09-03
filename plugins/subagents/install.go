package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Installation des serveurs MCP déclarés par le catalogue.
//
// C'est le point le plus sensible du plugin : installer, c'est exécuter du
// code sur le serveur avec les droits d'Automata. Trois règles le bornent.
//
//  1. La source est le CATALOGUE, jamais un membre ni le modèle. Aucune
//     URL, aucune commande ne vient d'une conversation.
//  2. Le chemin par défaut est un téléchargement VÉRIFIÉ : le plugin va
//     chercher l'archive lui-même et refuse tout ce dont le sha256 ne
//     correspond pas. Il fonctionne dans l'image distroless, qui n'a ni
//     shell ni compilateur.
//  3. Une commande libre reste possible pour les cas particuliers, à
//     condition que l'image embarque l'outillage nécessaire.

// envDataDir désigne le répertoire d'installation. Absent, les entrées à
// installer sont désactivées — les serveurs http, eux, continuent de
// fonctionner.
const envDataDir = "SUBAGENTS_DATA_DIR"

const (
	// installTimeout borne une installation entière (téléchargement,
	// vérification, extraction, ou commande).
	installTimeout = 5 * time.Minute
	// maxArchiveBytes borne ce qu'on accepte de télécharger : une archive
	// démesurée remplirait le volume de données.
	maxArchiveBytes = 256 << 20
	// maxChecksumsBytes borne le fichier de sommes.
	maxChecksumsBytes = 1 << 20
)

// installSpec décrit comment obtenir le binaire d'un serveur stdio.
type installSpec struct {
	Fetch *fetchSpec `yaml:"fetch"`
	// Command est exécutée telle quelle. Elle exige une image qui embarque
	// l'outillage : l'image distroless publiée n'a ni shell ni toolchain,
	// et l'installation y échouera franchement plutôt qu'à moitié.
	Command []string `yaml:"command"`
}

// fetchSpec est le téléchargement vérifié.
type fetchSpec struct {
	URL string `yaml:"url"`
	// Checksums est l'URL d'un fichier « <somme>  <nom> » (format
	// sha256sum, celui que publie goreleaser). Alternative : Sha256, la
	// somme écrite en dur.
	Checksums string `yaml:"checksums"`
	Sha256    string `yaml:"sha256"`
	// Extract est le chemin, dans l'archive tar.gz, du fichier à installer.
	// Vide : l'URL désigne le binaire lui-même.
	Extract string `yaml:"extract"`
	// Binary est le nom du fichier installé. Vide : le nom de base
	// d'Extract, ou celui de l'URL.
	Binary string `yaml:"binary"`
}

// installedState est ce que le plugin sait de l'installation présente.
// C'est lui qui permet de dire « installé 0.4.0, le catalogue demande
// 0.5.0 » plutôt que de comparer des dates de fichiers.
type installedState struct {
	Version     string `json:"version"`
	Sha256      string `json:"sha256,omitempty"`
	InstalledAt string `json:"installed_at"`
}

// installer pose les binaires et les fichiers des serveurs, et suit leur
// version.
type installer struct {
	dataDir string
	client  *http.Client

	// Le plugin est un processus unique : deux membres qui activent la
	// même entrée en même temps sont deux goroutines, pas deux processus.
	// Un verrou en mémoire suffit donc à garantir une installation unique
	// — un verrou de fichier ne protégerait de rien de plus.
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newInstaller() *installer {
	return &installer{
		dataDir: strings.TrimSpace(os.Getenv(envDataDir)),
		client:  &http.Client{Timeout: installTimeout},
		locks:   map[string]*sync.Mutex{},
	}
}

// available dit si des installations sont possibles.
func (i *installer) available() bool { return i.dataDir != "" }

// serverDir est la racine d'un serveur, versions confondues.
func (i *installer) serverDir(agentName, serverName string) string {
	return filepath.Join(i.dataDir, "servers", agentName, serverName)
}

// versionDir isole une version : deux peuvent coexister, ce qui rend le
// retour arrière aussi simple que réécrire l'ancien numéro dans le
// catalogue.
func (i *installer) versionDir(agentName, serverName, version string) string {
	return filepath.Join(i.serverDir(agentName, serverName), version)
}

// ensure garantit que le serveur est installé à la version du catalogue,
// et retourne les patrons {{bin}}, {{files}} et {{version}}.
//
// Un serveur sans installation (http, ou binaire déjà présent sur la
// machine) retourne les patrons sans rien poser sur le disque.
func (i *installer) ensure(ctx context.Context, agent catalogAgent, server serverSpec) (map[string]string, error) {
	if server.Install == nil {
		values := hostValues()
		values["version"] = server.Version
		return values, nil
	}
	if !i.available() {
		return nil, fmt.Errorf("aucun répertoire de données (%s) : ce sous-agent ne peut pas être installé", envDataDir)
	}

	lock := i.lockFor(agent.Name, server.Name)
	lock.Lock()
	defer lock.Unlock()

	dir := i.versionDir(agent.Name, server.Name, server.Version)
	// {{os}} et {{arch}} servent à composer l'URL d'une release publiée par
	// plateforme : ils doivent être là avant le téléchargement, pas
	// seulement au démarrage du serveur.
	values := hostValues()
	values["bin"] = filepath.Join(dir, "bin")
	values["files"] = filepath.Join(dir, "files")
	values["version"] = server.Version

	if state, ok := i.readState(agent.Name, server.Name); ok && state.Version == server.Version {
		// Déjà à jour. Les fichiers déclarés sont réécrits : le catalogue
		// peut en avoir changé le contenu sans changer de version, et ce
		// sont eux qui portent les politiques.
		return values, i.writeFiles(server, values)
	}

	// Ménage AVANT d'installer : les versions retirées le sont depuis au
	// moins un cycle de la faucheuse, aucun client ne les tient plus.
	i.removeOtherVersions(agent.Name, server.Name, server.Version)

	if err := os.MkdirAll(values["bin"], 0o755); err != nil {
		return nil, fmt.Errorf("création du répertoire d'installation: %w", err)
	}

	sum, err := i.install(ctx, server, values)
	if err != nil {
		// Rien n'est écrit dans l'état : l'ancienne version, si elle
		// existe encore, reste en service. Dégrader vaut mieux
		// qu'interrompre.
		return nil, err
	}

	if err := i.writeFiles(server, values); err != nil {
		return nil, err
	}
	if err := i.writeState(agent.Name, server.Name, installedState{
		Version:     server.Version,
		Sha256:      sum,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return nil, err
	}

	slog.Info("subagents: serveur MCP installé",
		"agent", agent.Name, "server", server.Name, "version", server.Version)

	return values, nil
}

// pending dit si le catalogue demande une version différente de celle
// installée. Sert à l'interface : « mise à jour en attente ».
func (i *installer) pending(agentName string, server serverSpec) (installedState, bool) {
	if server.Install == nil || !i.available() {
		return installedState{}, false
	}
	state, ok := i.readState(agentName, server.Name)
	if !ok {
		return installedState{}, false
	}
	return state, state.Version != server.Version
}

func (i *installer) lockFor(agentName, serverName string) *sync.Mutex {
	i.mu.Lock()
	defer i.mu.Unlock()

	key := agentName + "|" + serverName
	if lock, ok := i.locks[key]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	i.locks[key] = lock
	return lock
}

func (i *installer) statePath(agentName, serverName string) string {
	return filepath.Join(i.serverDir(agentName, serverName), "installed.json")
}

func (i *installer) readState(agentName, serverName string) (installedState, bool) {
	raw, err := os.ReadFile(i.statePath(agentName, serverName))
	if err != nil {
		return installedState{}, false
	}
	var state installedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return installedState{}, false
	}
	return state, state.Version != ""
}

func (i *installer) writeState(agentName, serverName string, state installedState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(i.statePath(agentName, serverName), raw, 0o644)
}

// removeOtherVersions supprime les répertoires de version qui ne sont plus
// celui du catalogue.
func (i *installer) removeOtherVersions(agentName, serverName, keep string) {
	entries, err := os.ReadDir(i.serverDir(agentName, serverName))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == keep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(i.serverDir(agentName, serverName), entry.Name()))
	}
}

// writeFiles pose les fichiers déclarés par le serveur (une politique
// d'autorisation, une configuration) dans {{files}}.
func (i *installer) writeFiles(server serverSpec, values map[string]string) error {
	if len(server.Files) == 0 {
		return nil
	}
	dir := values["files"]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("création du répertoire de fichiers: %w", err)
	}

	for name, content := range server.Files {
		rendered, missing := renderTemplate(content, values)
		if err := errMissingPlaceholders(missing); err != nil {
			return fmt.Errorf("fichier %q: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("écriture de %q: %w", name, err)
		}
	}

	return nil
}

// install exécute l'installation déclarée et retourne la somme retenue.
func (i *installer) install(ctx context.Context, server serverSpec, values map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	if server.Install.Fetch != nil {
		return i.fetch(ctx, *server.Install.Fetch, values)
	}
	if len(server.Install.Command) > 0 {
		return "", i.runCommand(ctx, server.Install.Command, values)
	}

	return "", fmt.Errorf("installation déclarée sans fetch ni command")
}

// fetch télécharge, vérifie et installe le binaire. La vérification n'est
// pas négociable : sans elle, l'installation ferait confiance à ce que
// répond le réseau.
func (i *installer) fetch(ctx context.Context, spec fetchSpec, values map[string]string) (string, error) {
	url, missing := renderTemplate(spec.URL, values)
	if err := errMissingPlaceholders(missing); err != nil {
		return "", err
	}

	payload, err := i.download(ctx, url, maxArchiveBytes)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(payload)
	got := hex.EncodeToString(sum[:])

	want, err := i.expectedSum(ctx, spec, url, values)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(got, want) {
		// Rien n'est écrit : ni binaire, ni état. La version en service
		// reste celle d'avant.
		return "", fmt.Errorf("somme de contrôle inattendue pour %s", filepath.Base(url))
	}

	binary := spec.Binary
	if binary == "" {
		binary = filepath.Base(spec.Extract)
	}
	if binary == "" || binary == "." {
		binary = filepath.Base(url)
	}
	target := filepath.Join(values["bin"], binary)

	if spec.Extract == "" {
		if err := os.WriteFile(target, payload, 0o755); err != nil {
			return "", fmt.Errorf("écriture du binaire: %w", err)
		}
		return got, nil
	}

	if err := extractFromTarGz(payload, spec.Extract, target); err != nil {
		return "", err
	}

	return got, nil
}

// expectedSum retourne la somme attendue : celle écrite dans le catalogue,
// ou celle du fichier de sommes de la release.
func (i *installer) expectedSum(ctx context.Context, spec fetchSpec, url string, values map[string]string) (string, error) {
	if spec.Sha256 != "" {
		return spec.Sha256, nil
	}
	if spec.Checksums == "" {
		return "", fmt.Errorf("aucune somme de contrôle déclarée (sha256 ou checksums) : téléchargement refusé")
	}

	checksumsURL, missing := renderTemplate(spec.Checksums, values)
	if err := errMissingPlaceholders(missing); err != nil {
		return "", err
	}

	raw, err := i.download(ctx, checksumsURL, maxChecksumsBytes)
	if err != nil {
		return "", err
	}

	name := filepath.Base(url)
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// Le format sha256sum préfixe le nom d'un « * » en mode binaire.
		if strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}

	return "", fmt.Errorf("aucune somme pour %s dans le fichier de sommes", name)
}

func (i *installer) download(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("requête invalide: %w", err)
	}

	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("téléchargement impossible: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("téléchargement refusé (code %d)", resp.StatusCode)
	}

	// max+1 : lire un octet de trop est ce qui permet de DÉTECTER le
	// dépassement au lieu d'installer un fichier tronqué.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("lecture interrompue: %w", err)
	}
	if int64(len(payload)) > max {
		return nil, fmt.Errorf("fichier trop volumineux (plus de %d octets)", max)
	}

	return payload, nil
}

// extractFromTarGz extrait UNE entrée nommée de l'archive vers target.
// Seul le nom déclaré par le catalogue est extrait : une archive ne peut
// pas déposer ce qu'elle veut où elle veut.
func extractFromTarGz(payload []byte, name, target string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("archive illisible: %w", err)
	}
	defer func() { _ = gz.Close() }()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return fmt.Errorf("l'archive ne contient pas %q", name)
		}
		if err != nil {
			return fmt.Errorf("archive illisible: %w", err)
		}
		if header.Name != name || header.Typeflag != tar.TypeReg {
			continue
		}

		file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("écriture du binaire: %w", err)
		}
		_, err = io.Copy(file, io.LimitReader(reader, maxArchiveBytes))
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("écriture du binaire: %w", err)
		}
		return closeErr
	}
}

// runCommand exécute la commande d'installation du catalogue. Sa sortie
// part au journal de l'exploitant, jamais au modèle : elle peut contenir
// n'importe quoi.
func (i *installer) runCommand(ctx context.Context, argv []string, values map[string]string) error {
	rendered := make([]string, len(argv))
	var missing []string
	for n, arg := range argv {
		value, miss := renderTemplate(arg, values)
		rendered[n] = value
		missing = append(missing, miss...)
	}
	if err := errMissingPlaceholders(missing); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, rendered[0], rendered[1:]...)
	cmd.Dir = values["bin"]
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("subagents: commande d'installation en échec",
			"command", rendered[0], "output", truncate(string(out), 2000), "error", err)
		return fmt.Errorf("la commande d'installation a échoué (voir les journaux du serveur)")
	}

	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
