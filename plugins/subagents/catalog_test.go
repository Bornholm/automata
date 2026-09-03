package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCatalog écrit un catalogue et pointe l'environnement dessus.
func writeCatalog(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("écriture du catalogue: %v", err)
	}
	t.Setenv(envCatalogFile, path)
}

const validCatalog = `
agents:
  - name: netprobe
    description: probes network endpoints
    system_prompt: You are a network probing specialist.
    max_tool_calls: 8
    servers:
      - name: netprobe
        transport: stdio
        command: ["netprobe-mcp"]
        read_only: [tcp_probe]
  - name: tickets
    description: reads the ticket tracker
    system_prompt: You are a ticket assistant.
    credentials:
      - key: api_token
        label: Jeton d'API
        required: true
    servers:
      - name: tracker
        transport: streamable-http
        url: https://tracker.test/mcp
        headers:
          Authorization: "Bearer {{api_token}}"
`

func TestLoadCatalog_ReadsTheOperatorFile(t *testing.T) {
	writeCatalog(t, validCatalog)

	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if len(cat.Agents) != 2 {
		t.Fatalf("%d entrée(s), attendu 2", len(cat.Agents))
	}

	netprobe, ok := cat.find("netprobe")
	if !ok {
		t.Fatal("entrée netprobe absente")
	}
	if netprobe.MaxToolCalls != 8 {
		t.Errorf("max_tool_calls=%d, attendu 8", netprobe.MaxToolCalls)
	}
	// Sans patron d'identifiant, la connexion peut être partagée par
	// l'organisation.
	if netprobe.Servers[0].needsCredentials() {
		t.Error("un serveur sans patron ne devrait pas exiger d'identifiant")
	}

	tickets, _ := cat.find("tickets")
	if !tickets.Servers[0].needsCredentials() {
		t.Error("un serveur à patron d'identifiant doit être cloisonné par membre")
	}
}

// Le catalogue embarqué doit rester valide : il est chargé au démarrage de
// chaque instance qui n'en fournit pas d'autre.
func TestLoadCatalog_EmbeddedIsValid(t *testing.T) {
	if _, err := loadCatalog(); err != nil {
		t.Fatalf("catalogue embarqué invalide: %v", err)
	}
}

// Un catalogue invalide est refusé en nommant l'entrée fautive : un
// catalogue à moitié chargé donnerait un membre qui active un agent qui ne
// se montera jamais.
func TestLoadCatalog_RefusesInvalidEntries(t *testing.T) {
	cases := map[string]struct{ body, needle string }{
		"patron sans identifiant déclaré": {
			body: `
agents:
  - name: tickets
    description: reads the tracker
    system_prompt: You are an assistant.
    servers:
      - name: tracker
        transport: http
        url: https://tracker.test/mcp
        headers:
          Authorization: "Bearer {{api_token}}"
`,
			needle: "{{api_token}}",
		},
		"clé d'identifiant réservée": {
			body: `
agents:
  - name: tickets
    description: reads the tracker
    system_prompt: You are an assistant.
    credentials:
      - key: bin
        required: true
    servers:
      - name: tracker
        transport: http
        url: https://tracker.test/mcp?k={{bin}}
`,
			needle: "réservée",
		},
		"transport inconnu": {
			body: `
agents:
  - name: tickets
    description: reads the tracker
    system_prompt: You are an assistant.
    servers:
      - name: tracker
        transport: websocket
        url: wss://tracker.test/mcp
`,
			needle: "non supporté",
		},
		"stdio sans commande": {
			body: `
agents:
  - name: tickets
    description: reads the tracker
    system_prompt: You are an assistant.
    servers:
      - name: tracker
        transport: stdio
`,
			needle: "command requise",
		},
		"aucun serveur": {
			body: `
agents:
  - name: tickets
    description: reads the tracker
    system_prompt: You are an assistant.
`,
			needle: "aucun serveur",
		},
		"nom d'entrée invalide": {
			body: `
agents:
  - name: Mon Agent
    description: reads the tracker
    system_prompt: You are an assistant.
    servers:
      - name: tracker
        transport: stdio
        command: ["x"]
`,
			needle: "invalide",
		},
		"description absente": {
			body: `
agents:
  - name: tickets
    system_prompt: You are an assistant.
    servers:
      - name: tracker
        transport: stdio
        command: ["x"]
`,
			needle: "description requise",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			writeCatalog(t, tc.body)

			_, err := loadCatalog()
			if err == nil {
				t.Fatal("catalogue invalide accepté")
			}
			if !strings.Contains(err.Error(), tc.needle) {
				t.Errorf("message sans %q: %v", tc.needle, err)
			}
			// L'entrée fautive doit être nommée : sans cela, l'exploitant
			// cherche dans tout le fichier.
			if !strings.Contains(err.Error(), "tickets") && !strings.Contains(err.Error(), "Mon Agent") {
				t.Errorf("message sans le nom de l'entrée: %v", err)
			}
		})
	}
}

func TestCatalog_MissingCredentialsNamesWhatIsAbsent(t *testing.T) {
	writeCatalog(t, validCatalog)
	cat, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}

	tickets, _ := cat.find("tickets")

	missing := tickets.missingCredentials(map[string]bool{})
	if len(missing) != 1 || missing[0] != "api_token" {
		t.Fatalf("identifiants manquants inattendus: %v", missing)
	}
	if len(tickets.missingCredentials(map[string]bool{"api_token": true})) != 0 {
		t.Error("un identifiant renseigné est encore signalé manquant")
	}
}
