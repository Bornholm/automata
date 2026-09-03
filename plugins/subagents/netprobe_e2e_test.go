package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// envE2E déclenche la recette de bout en bout du catalogue embarqué. Elle
// télécharge une release depuis GitHub et sonde Internet : elle n'a rien à
// faire dans une intégration continue, mais tout à faire sous la main de
// qui touche au catalogue.
//
//	SUBAGENTS_E2E=1 go test -run Netprobe -v ./...
const envE2E = "SUBAGENTS_E2E"

// La recette complète de l'entrée netprobe : installation vérifiée depuis
// la release, connexion, découverte des outils, une sonde publique qui
// répond, et une cible du réseau interne que la politique refuse.
func TestNetprobe_EndToEnd(t *testing.T) {
	if os.Getenv(envE2E) != "1" {
		t.Skipf("recette réseau désactivée (%s=1 pour la lancer)", envE2E)
	}

	t.Setenv(envDataDir, t.TempDir())

	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	agent, ok := cat.find("netprobe")
	if !ok {
		t.Fatal("le catalogue embarqué ne propose pas netprobe")
	}

	p := newPlugin(cat)
	t.Cleanup(p.pool.close)

	conn, err := p.connectionFor(context.Background(), agent, agent.Servers[0], "atelier", "cam", hostValues())
	if err != nil {
		t.Fatalf("installation ou connexion: %v", err)
	}

	tools, err := p.pool.tools(context.Background(), conn)
	if err != nil {
		t.Fatalf("découverte des outils: %v", err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
		if !tool.ReadOnly {
			t.Errorf("l'outil %q n'est pas épinglé en lecture : chaque sonde demanderait une confirmation", tool.Name)
		}
	}
	for _, expected := range []string{"tcp_probe", "http_probe", "dns_probe", "tls_diagnose"} {
		if !slices.Contains(names, expected) {
			t.Errorf("outil %q absent : %v", expected, names)
		}
	}

	// Une cible publique QUELCONQUE, délibérément absente de la liste
	// d'autorisation interne de netprobe : celle-ci ne connaît qu'une
	// poignée d'hôtes de diagnostic, et un catalogue qui l'appliquerait par
	// mégarde refuserait presque toutes les questions posées. Un
	// example.com ne verrait pas la différence.
	public, err := p.pool.call(context.Background(), conn, "http_probe", map[string]any{"url": "https://www.wikipedia.org"})
	if err != nil {
		t.Fatalf("sonde publique: %v", err)
	}
	// Ce qui compte est que la cible ait été JOIGNABLE, pas le code rendu :
	// un site peut répondre 403 à une sonde sans que la politique y soit
	// pour quoi que ce soit.
	if !strings.Contains(public, "http_probe OK") {
		t.Errorf("cible publique non atteinte: %s", public)
	}

	// Le refus des adresses privées est ce qui sépare un outil de
	// diagnostic d'un SSRF offert sur le réseau interne.
	private, err := p.pool.call(context.Background(), conn, "tcp_probe", map[string]any{"host": "192.168.1.1", "port": 80})
	if err != nil {
		t.Fatalf("sonde privée: %v", err)
	}
	if !strings.Contains(strings.ToLower(private), "private address not permitted") {
		t.Errorf("une cible du réseau interne n'a pas été refusée: %s", private)
	}
}
