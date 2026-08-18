package config

import (
	"slices"
	"strings"
	"testing"
)

func TestRenderMCPTemplate(t *testing.T) {
	rendered, missing := RenderMCPTemplate("--host {{host}} --user {{ user }}", map[string]string{
		"host": "imap.example.com",
		"user": "alice",
	})
	if rendered != "--host imap.example.com --user alice" {
		t.Errorf("rendered = %q", rendered)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, attendu aucun", missing)
	}

	rendered, missing = RenderMCPTemplate("{{host}}:{{port}}", map[string]string{"host": "h"})
	if rendered != "h:{{port}}" {
		t.Errorf("rendered = %q, le patron sans valeur doit rester littéral", rendered)
	}
	if !slices.Equal(missing, []string{"port"}) {
		t.Errorf("missing = %v, attendu [port]", missing)
	}
}

func TestMCPServerTemplatePlaceholders(t *testing.T) {
	server := MCPServer{
		Command: []string{"imap-mcp", "--host", "{{host}}", "--host2", "{{host}}"},
		Env:     map[string]string{"PASSWORD": "{{password}}", "STATIC": "rien"},
	}

	got := server.TemplatePlaceholders()
	if !slices.Equal(got, []string{"host", "password"}) {
		t.Errorf("placeholders = %v, attendu [host password] (dédupliqués, triés)", got)
	}
}

func stdioTestConfig() *Config {
	return &Config{
		MCPServers: map[string]MCPServer{
			"imap": {
				Transport: "stdio",
				Command:   []string{"imap-mcp", "--host", "{{host}}"},
				Env:       map[string]string{"IMAP_PASSWORD": "{{password}}"},
			},
		},
		Identities: Identities{
			Principals: []Principal{
				{
					ID:   "alice",
					Kind: PrincipalKindHuman,
					MCP: map[string]MCPOverride{
						"imap": {Values: map[string]string{"host": "imap.example.com", "password": "x"}},
					},
				},
			},
		},
	}
}

func assertHasError(t *testing.T, errs []error, fragment string) {
	t.Helper()
	for _, err := range errs {
		if strings.Contains(err.Error(), fragment) {
			return
		}
	}
	t.Errorf("erreurs = %v, attendu une erreur contenant %q", errs, fragment)
}

func TestValidateMCPServers_Stdio(t *testing.T) {
	cfg := stdioTestConfig()
	if errs := validateMCPServers(cfg); len(errs) != 0 {
		t.Fatalf("configuration stdio valide refusée: %v", errs)
	}

	// Commande absente.
	server := cfg.MCPServers["imap"]
	server.Command = nil
	cfg.MCPServers["imap"] = server
	assertHasError(t, validateMCPServers(cfg), "command: requis")

	// URL et headers sans effet en stdio.
	cfg = stdioTestConfig()
	server = cfg.MCPServers["imap"]
	server.URL = "http://example.com"
	server.Headers = map[string]string{"Authorization": "x"}
	cfg.MCPServers["imap"] = server
	errs := validateMCPServers(cfg)
	assertHasError(t, errs, "url: sans effet")
	assertHasError(t, errs, "headers: sans effet")

	// Command et env sans effet en http.
	cfg = stdioTestConfig()
	cfg.MCPServers["imap"] = MCPServer{
		Transport: "http",
		URL:       "http://example.com",
		Command:   []string{"x"},
		Env:       map[string]string{"A": "b"},
	}
	errs = validateMCPServers(cfg)
	assertHasError(t, errs, "command: sans effet")
	assertHasError(t, errs, "env: sans effet")

	// Transport inconnu.
	cfg = stdioTestConfig()
	cfg.MCPServers["imap"] = MCPServer{Transport: "websocket", URL: "x"}
	assertHasError(t, validateMCPServers(cfg), "non supporté")
}

func TestValidateIdentities_StdioOverrides(t *testing.T) {
	cfg := stdioTestConfig()
	if errs := validateIdentities(cfg); len(errs) != 0 {
		t.Fatalf("surcharge stdio valide refusée: %v", errs)
	}

	// Patron sans valeur : signalé par nom au chargement.
	cfg.Identities.Principals[0].MCP["imap"] = MCPOverride{Values: map[string]string{"host": "h"}}
	assertHasError(t, validateIdentities(cfg), "{{password}}")

	// URL/headers sans effet sur un serveur stdio.
	cfg = stdioTestConfig()
	cfg.Identities.Principals[0].MCP["imap"] = MCPOverride{
		URL:     "http://example.com",
		Headers: map[string]string{"Authorization": "x"},
		Values:  map[string]string{"host": "h", "password": "p"},
	}
	errs := validateIdentities(cfg)
	assertHasError(t, errs, "url: sans effet")
	assertHasError(t, errs, "headers: sans effet")

	// Une valeur sans patron correspondant est une surcharge inopérante.
	cfg = stdioTestConfig()
	cfg.MCPServers["imap"] = MCPServer{Transport: "http", URL: "http://example.com"}
	cfg.Identities.Principals[0].MCP["imap"] = MCPOverride{Values: map[string]string{"a": "b"}}
	assertHasError(t, validateIdentities(cfg), "aucun patron {{a}}")
}

func TestValidateIdentities_HTTPTemplates(t *testing.T) {
	httpConfig := func() *Config {
		return &Config{
			MCPServers: map[string]MCPServer{
				"meteo": {
					Transport: "http",
					URL:       "https://mcp.example.com/tenants/{{tenant}}/mcp",
					Headers:   map[string]string{"X-Api-Key": "{{api_key}}"},
				},
			},
			Identities: Identities{
				Principals: []Principal{
					{
						ID:   "alice",
						Kind: PrincipalKindHuman,
						MCP: map[string]MCPOverride{
							"meteo": {Values: map[string]string{"tenant": "alice", "api_key": "k"}},
						},
					},
				},
			},
		}
	}

	if errs := validateIdentities(httpConfig()); len(errs) != 0 {
		t.Fatalf("surcharge http templetée valide refusée: %v", errs)
	}

	// Patron sans valeur : signalé par nom.
	cfg := httpConfig()
	cfg.Identities.Principals[0].MCP["meteo"] = MCPOverride{Values: map[string]string{"tenant": "alice"}}
	assertHasError(t, validateIdentities(cfg), "{{api_key}}")

	// L'URL du principal remplace celle du serveur : ses patrons à elle font
	// foi, ceux de l'URL du serveur ne comptent plus.
	cfg = httpConfig()
	cfg.Identities.Principals[0].MCP["meteo"] = MCPOverride{
		URL:    "https://autre.example.com/mcp?key={{cle}}",
		Values: map[string]string{"cle": "x", "api_key": "k"},
	}
	if errs := validateIdentities(cfg); len(errs) != 0 {
		t.Fatalf("URL de principal templetée refusée: %v", errs)
	}
}
