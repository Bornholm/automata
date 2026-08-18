package registry

import (
	"strings"
	"testing"

	"github.com/bornholm/go-courier"

	"github.com/bornholm/automata/internal/config"
)

// Chaque type déclaré se construit sans toucher au réseau (convention
// go-courier : rien ne se connecte avant Listen/Send), et un type inconnu
// est refusé avec la liste des types valides.
func TestBuildCourierProviders(t *testing.T) {
	cases := map[string]config.CourierProvider{
		"whatsapp": {Type: "whatsapp", Extra: map[string]any{"session_path": t.TempDir() + "/wa.sqlite"}},
		"signal":   {Type: "signal", Extra: map[string]any{"account": "+33612345678", "address": "tcp://127.0.0.1:7583"}},
		"discord":  {Type: "discord", Extra: map[string]any{"token": "jeton"}},
		"rocket":   {Type: "rocket", Extra: map[string]any{"server_url": "https://chat.example.test", "username": "bot", "password": "p"}},
		"mail": {Type: "mail", Extra: map[string]any{
			"imap": map[string]any{"address": "imap.example.test:993", "username": "u", "password": "p", "check_interval": "2m", "folders": []any{"INBOX"}},
			"smtp": map[string]any{"address": "smtp.example.test:587", "issuer": "bot@example.test", "username": "u", "password": "p"},
		}},
	}

	for name, cp := range cases {
		t.Run(name, func(t *testing.T) {
			provider, err := buildCourierProvider(cp)
			if err != nil {
				t.Fatalf("buildCourierProvider: %v", err)
			}
			if provider == nil {
				t.Fatal("provider nil")
			}
			var _ courier.Provider = provider
		})
	}

	if _, err := buildCourierProvider(config.CourierProvider{Type: "pigeon"}); err == nil || !strings.Contains(err.Error(), "non supporté") {
		t.Errorf("type inconnu: err = %v", err)
	}
	if _, err := buildCourierProvider(config.CourierProvider{Type: "rocket", Extra: map[string]any{"server_url": "://invalide", "username": "u", "password": "p"}}); err == nil {
		t.Error("une server_url invalide doit être refusée à la construction")
	}
}
