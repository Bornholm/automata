package config

import (
	"strings"
	"testing"
)

func courierConfig(providerType string, extra map[string]any) *Config {
	return &Config{
		Courier: Courier{
			Providers: map[string]CourierProvider{
				"chat": {Type: providerType, Extra: extra},
			},
		},
	}
}

// Un identifiant manquant refuse le chargement, il n'échoue pas au premier
// message : même principe que les llm_clients.
func TestValidateCourierProviders(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *Config
		fragment string // vide = configuration valide
	}{
		{"whatsapp valide", courierConfig("whatsapp", map[string]any{"session_path": "./wa.sqlite"}), ""},
		{"whatsapp sans session", courierConfig("whatsapp", nil), "session_path"},
		{"signal valide", courierConfig("signal", map[string]any{"account": "+33612345678"}), ""},
		{"signal sans compte", courierConfig("signal", map[string]any{"address": "tcp://127.0.0.1:7583"}), "account"},
		{"discord valide", courierConfig("discord", map[string]any{"token": "x"}), ""},
		{"discord sans jeton", courierConfig("discord", nil), "token"},
		{"rocket valide", courierConfig("rocket", map[string]any{"server_url": "https://chat.example.test", "username": "bot", "password": "p"}), ""},
		{"rocket sans identifiants", courierConfig("rocket", map[string]any{"server_url": "https://chat.example.test"}), "username/password"},
		{
			"mail valide",
			courierConfig("mail", map[string]any{
				"imap": map[string]any{"address": "imap.example.test:993", "username": "u", "password": "p"},
				"smtp": map[string]any{"address": "smtp.example.test:587", "issuer": "bot@example.test"},
			}),
			"",
		},
		{
			"mail sans smtp",
			courierConfig("mail", map[string]any{
				"imap": map[string]any{"address": "imap.example.test:993", "username": "u", "password": "p"},
			}),
			"smtp",
		},
		{"type inconnu", courierConfig("pigeon", nil), "non supporté"},
		{"type absent", courierConfig("", nil), "type: requis"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateCourierProviders(tc.cfg)

			if tc.fragment == "" {
				if len(errs) != 0 {
					t.Fatalf("configuration valide refusée: %v", errs)
				}
				return
			}

			for _, err := range errs {
				if strings.Contains(err.Error(), tc.fragment) {
					return
				}
			}
			t.Errorf("erreurs = %v, attendu une erreur contenant %q", errs, tc.fragment)
		})
	}
}

// Les champs imbriqués (mail.imap.*) traversent le décodage YAML générique
// jusqu'aux structs typées, durée comprise.
func TestDecodeExtra_NestedFields(t *testing.T) {
	cp := CourierProvider{
		Type: "mail",
		Extra: map[string]any{
			"imap": map[string]any{
				"address":        "imap.example.test:993",
				"check_interval": "2m",
				"folders":        []any{"INBOX", "Assistant"},
			},
			"smtp": map[string]any{"address": "smtp.example.test:587", "issuer": "bot@example.test"},
		},
	}

	var c MailProviderConfig
	if err := cp.DecodeExtra(&c); err != nil {
		t.Fatalf("DecodeExtra: %v", err)
	}

	if c.IMAP.Address != "imap.example.test:993" || c.SMTP.Issuer != "bot@example.test" {
		t.Errorf("décodage = %+v", c)
	}
	if c.IMAP.CheckInterval.Duration().Minutes() != 2 {
		t.Errorf("check_interval = %s, attendu 2m", c.IMAP.CheckInterval.Duration())
	}
	if len(c.IMAP.Folders) != 2 || c.IMAP.Folders[1] != "Assistant" {
		t.Errorf("folders = %v", c.IMAP.Folders)
	}
}
