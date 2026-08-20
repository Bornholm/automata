package config

import (
	"strings"
	"testing"
)

// validWebConfig retourne une section web complète et valide, à dégrader
// dans chaque cas de test.
func validWebConfig() *Config {
	return &Config{
		Courier: Courier{Providers: map[string]CourierProvider{
			"boite": {Type: "mail"},
		}},
		Web: Web{
			Enabled:       true,
			Addr:          "127.0.0.1:8081",
			BaseURL:       "https://automata.exemple.fr",
			SessionSecret: strings.Repeat("s", 32),
			Admin:         WebAdmin{Email: "op@exemple.fr", PasswordHash: "$2a$10$abcdefghijklmnopqrstuv"},
			MailProvider:  "boite",
			Credits: WebCredits{
				Packs: []WebCreditPack{{Credits: 1000, PriceEUR: 9}},
			},
		},
	}
}

func TestValidateWeb_DisabledSectionIsAlwaysValid(t *testing.T) {
	if errs := validateWeb(&Config{}); len(errs) != 0 {
		t.Fatalf("section web absente refusée: %v", errs)
	}
}

func TestValidateWeb_CompleteSectionIsAccepted(t *testing.T) {
	if errs := validateWeb(validWebConfig()); len(errs) != 0 {
		t.Fatalf("section web complète refusée: %v", errs)
	}
}

// Un secret de session trop court rend les cookies signés forgeables : la
// panne serait silencieuse et grave, elle doit être refusée au chargement.
func TestValidateWeb_RejectsShortSessionSecret(t *testing.T) {
	cfg := validWebConfig()
	cfg.Web.SessionSecret = "court"
	assertHasError(t, validateWeb(cfg), "web.session_secret: au moins 32 octets")
}

func TestValidateWeb_RequiresAdminCredentials(t *testing.T) {
	cfg := validWebConfig()
	cfg.Web.Admin = WebAdmin{}
	assertHasError(t, validateWeb(cfg), "web.admin.email: requis")
	assertHasError(t, validateWeb(cfg), "web.admin.password_hash: requis")
}

func TestValidateWeb_RejectsUnknownMailProvider(t *testing.T) {
	cfg := validWebConfig()
	cfg.Web.MailProvider = "fantome"
	assertHasError(t, validateWeb(cfg), `web.mail_provider: provider courier "fantome" introuvable`)
}

func TestValidateWeb_RejectsInvalidPack(t *testing.T) {
	cfg := validWebConfig()
	cfg.Web.Credits.Packs = []WebCreditPack{{Credits: 0, PriceEUR: -1}}
	assertHasError(t, validateWeb(cfg), "web.credits.packs[0].credits")
	assertHasError(t, validateWeb(cfg), "web.credits.packs[0].price_eur")
}

// La section plugins est refusée incomplète dès l'activation : un client
// LLM manquant ne doit pas se découvrir au premier tour d'un sous-agent.
func TestValidatePlugins(t *testing.T) {
	base := func() *Config {
		return &Config{
			Plugins:    Plugins{Enabled: true, Dir: "./plugins", Client: "main"},
			LLMClients: map[string]LLMClient{"main": {Provider: "openai", Model: "m", APIKey: "k", BaseURL: "u"}},
		}
	}

	if errs := validatePlugins(base()); len(errs) != 0 {
		t.Fatalf("configuration valide refusée: %v", errs)
	}

	cfg := base()
	cfg.Plugins.Client = "inconnu"
	if errs := validatePlugins(cfg); len(errs) == 0 {
		t.Error("client llm inconnu accepté")
	}

	cfg = base()
	cfg.Plugins.Dir = ""
	if errs := validatePlugins(cfg); len(errs) == 0 {
		t.Error("répertoire manquant accepté")
	}

	cfg = base()
	cfg.Plugins.Enabled = false
	cfg.Plugins.Dir = ""
	cfg.Plugins.Client = ""
	if errs := validatePlugins(cfg); len(errs) != 0 {
		t.Errorf("section désactivée refusée: %v", errs)
	}
}
