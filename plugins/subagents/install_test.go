package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseServer sert une archive tar.gz et son checksums.txt, comme une
// release goreleaser. corrupt fausse la somme publiée : c'est le cas qui
// doit être refusé.
func releaseServer(t *testing.T, content map[string][]byte, corrupt bool) *httptest.Server {
	t.Helper()

	archives := map[string][]byte{}
	var checksums strings.Builder

	for name, body := range content {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "serveur", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("archive: %v", err)
		}
		_ = tw.Close()
		_ = gz.Close()

		archives[name] = buf.Bytes()

		sum := sha256.Sum256(buf.Bytes())
		published := hex.EncodeToString(sum[:])
		if corrupt {
			published = strings.Repeat("0", len(published))
		}
		fmt.Fprintf(&checksums, "%s  %s\n", published, name)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums.String()))
	})
	for name, body := range archives {
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
		})
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

// installableAgent déclare une entrée dont le serveur s'installe depuis
// l'URL donnée.
func installableAgent(base, version string) catalogAgent {
	return catalogAgent{
		Name:         "probe",
		Description:  "probes things",
		SystemPrompt: "You are a prober.",
		Servers: []serverSpec{{
			Name:      "fake",
			Transport: transportStdio,
			Version:   version,
			Command:   []string{"{{bin}}/serveur"},
			Install: &installSpec{Fetch: &fetchSpec{
				URL:       base + "/serveur_{{version}}_linux.tar.gz",
				Checksums: base + "/checksums.txt",
				Extract:   "serveur",
			}},
			Files: map[string]string{"policy.yaml": "version: {{version}}\n"},
		}},
	}
}

func newTestInstaller(t *testing.T) *installer {
	t.Helper()
	t.Setenv(envDataDir, t.TempDir())
	return newInstaller()
}

// L'installation télécharge, vérifie la somme, extrait le binaire déclaré
// et pose les fichiers du catalogue.
func TestInstaller_FetchesAndVerifies(t *testing.T) {
	server := releaseServer(t, map[string][]byte{
		"serveur_1.0.0_linux.tar.gz": []byte("#!/bin/sh\nexit 0\n"),
	}, false)

	inst := newTestInstaller(t)
	agent := installableAgent(server.URL, "1.0.0")

	values, err := inst.ensure(context.Background(), agent, agent.Servers[0])
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	binary := filepath.Join(values["bin"], "serveur")
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("binaire absent: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("binaire non exécutable: %v", info.Mode())
	}

	// Les fichiers déclarés sont posés, patrons résolus.
	policy, err := os.ReadFile(filepath.Join(values["files"], "policy.yaml"))
	if err != nil {
		t.Fatalf("politique absente: %v", err)
	}
	if !strings.Contains(string(policy), "version: 1.0.0") {
		t.Errorf("politique non rendue: %s", policy)
	}

	// L'état retient la version : c'est lui qui permettra de dire qu'une
	// mise à jour est en attente.
	state, ok := inst.readState(agent.Name, "fake")
	if !ok || state.Version != "1.0.0" || state.Sha256 == "" {
		t.Errorf("état d'installation inattendu: %+v", state)
	}
}

// Une somme qui ne correspond pas annule TOUT : rien n'est installé, rien
// n'est retenu, et l'ancienne version — s'il y en a une — reste en
// service.
func TestInstaller_RefusesATamperedArchive(t *testing.T) {
	server := releaseServer(t, map[string][]byte{
		"serveur_1.0.0_linux.tar.gz": []byte("charge utile"),
	}, true)

	inst := newTestInstaller(t)
	agent := installableAgent(server.URL, "1.0.0")

	_, err := inst.ensure(context.Background(), agent, agent.Servers[0])
	if err == nil {
		t.Fatal("une archive à somme fausse a été installée")
	}
	if !strings.Contains(err.Error(), "somme de contrôle") {
		t.Errorf("message inattendu: %v", err)
	}

	if _, ok := inst.readState(agent.Name, "fake"); ok {
		t.Error("un état d'installation a été écrit malgré l'échec")
	}
	if _, err := os.Stat(filepath.Join(inst.versionDir(agent.Name, "fake", "1.0.0"), "bin", "serveur")); err == nil {
		t.Error("un binaire a été écrit malgré l'échec")
	}
}

// Un téléchargement sans somme déclarée est refusé au chargement du
// catalogue : installer ce que répond le réseau, sans rien vérifier, n'est
// pas une option.
func TestCatalog_RefusesAFetchWithoutChecksum(t *testing.T) {
	writeCatalog(t, `
agents:
  - name: probe
    description: probes things
    system_prompt: You are a prober.
    servers:
      - name: fake
        transport: stdio
        version: "1.0.0"
        command: ["{{bin}}/serveur"]
        install:
          fetch:
            url: https://exemple.test/serveur.tar.gz
`)

	_, err := loadCatalog()
	if err == nil {
		t.Fatal("un téléchargement sans somme a été accepté")
	}
	if !strings.Contains(err.Error(), "somme de contrôle") {
		t.Errorf("message inattendu: %v", err)
	}
}

// Une installation déclarée sans version est refusée : « installer si le
// binaire est absent » fige le serveur au premier jour.
func TestCatalog_RefusesAnInstallWithoutVersion(t *testing.T) {
	writeCatalog(t, `
agents:
  - name: probe
    description: probes things
    system_prompt: You are a prober.
    servers:
      - name: fake
        transport: stdio
        command: ["{{bin}}/serveur"]
        install:
          fetch:
            url: https://exemple.test/serveur.tar.gz
            sha256: abc
`)

	_, err := loadCatalog()
	if err == nil {
		t.Fatal("une installation sans version a été acceptée")
	}
	if !strings.Contains(err.Error(), "version requise") {
		t.Errorf("message inattendu: %v", err)
	}
}

// Un identifiant de membre n'a rien à faire dans une installation, qui est
// posée une fois pour tous.
func TestCatalog_RefusesACredentialInAnInstall(t *testing.T) {
	writeCatalog(t, `
agents:
  - name: probe
    description: probes things
    system_prompt: You are a prober.
    credentials:
      - key: token
    servers:
      - name: fake
        transport: stdio
        version: "1.0.0"
        command: ["{{bin}}/serveur"]
        install:
          fetch:
            url: https://exemple.test/serveur.tar.gz?t={{token}}
            sha256: abc
`)

	_, err := loadCatalog()
	if err == nil {
		t.Fatal("un identifiant dans une installation a été accepté")
	}
	if !strings.Contains(err.Error(), "{{token}}") {
		t.Errorf("message inattendu: %v", err)
	}
}

// Le second appel ne réinstalle pas : c'est l'état, pas la présence du
// fichier, qui fait foi.
func TestInstaller_DoesNotReinstallTheSameVersion(t *testing.T) {
	var hits int
	server := releaseServer(t, map[string][]byte{
		"serveur_1.0.0_linux.tar.gz": []byte("charge utile"),
	}, false)

	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, server.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(counting.Close)

	inst := newTestInstaller(t)
	agent := installableAgent(counting.URL, "1.0.0")

	if _, err := inst.ensure(context.Background(), agent, agent.Servers[0]); err != nil {
		t.Fatalf("première installation: %v", err)
	}
	first := hits

	if _, err := inst.ensure(context.Background(), agent, agent.Servers[0]); err != nil {
		t.Fatalf("seconde vérification: %v", err)
	}
	if hits != first {
		t.Errorf("%d requête(s) supplémentaires : la version installée a été retéléchargée", hits-first)
	}
}

// Monter le numéro de version dans le catalogue installe la nouvelle, et
// l'ancien répertoire disparaît une fois la nouvelle en place.
func TestInstaller_UpgradesWhenTheCatalogVersionChanges(t *testing.T) {
	server := releaseServer(t, map[string][]byte{
		"serveur_1.0.0_linux.tar.gz": []byte("ancienne"),
		"serveur_2.0.0_linux.tar.gz": []byte("nouvelle"),
	}, false)

	inst := newTestInstaller(t)

	old := installableAgent(server.URL, "1.0.0")
	if _, err := inst.ensure(context.Background(), old, old.Servers[0]); err != nil {
		t.Fatalf("installation initiale: %v", err)
	}

	next := installableAgent(server.URL, "2.0.0")
	if state, pending := inst.pending(next.Name, next.Servers[0]); !pending || state.Version != "1.0.0" {
		t.Errorf("mise à jour non signalée: state=%+v pending=%v", state, pending)
	}

	values, err := inst.ensure(context.Background(), next, next.Servers[0])
	if err != nil {
		t.Fatalf("mise à jour: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(values["bin"], "serveur"))
	if err != nil {
		t.Fatalf("binaire absent après mise à jour: %v", err)
	}
	if string(body) != "nouvelle" {
		t.Errorf("binaire non remplacé: %q", body)
	}

	if _, err := os.Stat(inst.versionDir(next.Name, "fake", "1.0.0")); err == nil {
		t.Error("l'ancienne version n'a pas été nettoyée")
	}
	if _, pending := inst.pending(next.Name, next.Servers[0]); pending {
		t.Error("une mise à jour est encore annoncée après l'avoir faite")
	}
}

// Une mise à jour qui échoue ne touche à rien : l'ancienne version reste
// en service. Dégrader vaut mieux qu'interrompre.
func TestInstaller_FailedUpgradeKeepsTheWorkingVersion(t *testing.T) {
	server := releaseServer(t, map[string][]byte{
		"serveur_1.0.0_linux.tar.gz": []byte("ancienne"),
	}, false)

	inst := newTestInstaller(t)
	old := installableAgent(server.URL, "1.0.0")
	if _, err := inst.ensure(context.Background(), old, old.Servers[0]); err != nil {
		t.Fatalf("installation initiale: %v", err)
	}

	// La 2.0.0 n'existe pas sur la release : 404.
	next := installableAgent(server.URL, "2.0.0")
	if _, err := inst.ensure(context.Background(), next, next.Servers[0]); err == nil {
		t.Fatal("une mise à jour impossible a été déclarée réussie")
	}

	state, _ := inst.readState(old.Name, "fake")
	if state.Version != "1.0.0" {
		t.Errorf("l'état a été abîmé par l'échec: %+v", state)
	}
}

// Sans répertoire de données, une entrée à installer est refusée
// franchement : elle ne se montera pas, et l'interface le dit.
func TestInstaller_RefusesWithoutADataDirectory(t *testing.T) {
	t.Setenv(envDataDir, "")
	inst := newInstaller()

	agent := installableAgent("https://exemple.test", "1.0.0")
	if _, err := inst.ensure(context.Background(), agent, agent.Servers[0]); err == nil {
		t.Fatal("une installation a été tentée sans répertoire de données")
	} else if !strings.Contains(err.Error(), envDataDir) {
		t.Errorf("message sans la variable à renseigner: %v", err)
	}

	// Un serveur sans installation n'est pas concerné : les entrées http
	// continuent de fonctionner.
	http := serverSpec{Name: "api", Transport: transportStreamable, URL: "https://exemple.test/mcp"}
	if _, err := inst.ensure(context.Background(), agent, http); err != nil {
		t.Errorf("un serveur sans installation a été refusé: %v", err)
	}
}
