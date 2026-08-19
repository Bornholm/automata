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
