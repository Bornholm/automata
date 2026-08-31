package registry

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/config"
)

// PlatformTypes énumère les types de comptes de messagerie gérables depuis
// l'administration, dans l'ordre d'affichage.
var PlatformTypes = []string{"whatsapp", "signal", "rocket", "discord", "mail"}

// platformTypeLabel nomme un type de compte pour l'affichage.
func platformTypeLabel(providerType string) string {
	switch providerType {
	case "whatsapp":
		return "WhatsApp"
	case "signal":
		return "Signal"
	case "rocket":
		return "Rocket.Chat"
	case "discord":
		return "Discord"
	case "mail":
		return "Courriel"
	default:
		return providerType
	}
}

// buildManagedProvider construit un fournisseur Courier depuis la
// configuration déchiffrée d'un compte enregistré. Il fait le pont entre
// la forme générique stockée en base (map JSON) et les structs typées de
// la configuration, en réutilisant leur décodage.
func buildManagedProvider(id, providerType string, raw map[string]any, qrHandler func(code string, linked bool)) (courier.Provider, error) {
	cp := config.CourierProvider{Type: providerType, Extra: raw}

	switch providerType {
	case "whatsapp":
		var c config.WhatsAppProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.SessionPath == "" {
			return nil, fmt.Errorf("champ session_path requis et non vide")
		}

		opts := []whatsapp.OptionFunc{
			whatsapp.WithDBPath(c.SessionPath),
			whatsapp.WithLogLevel(whatsmeowLogLevel()),
		}
		if qrHandler != nil {
			// Le code d'appairage remonte à l'administration au lieu d'être
			// imprimé sur la sortie standard : c'est ce qui permet de lier
			// un compte depuis le navigateur.
			opts = append(opts, whatsapp.WithQRHandler(func(_ context.Context, code string, linked bool) {
				qrHandler(code, linked)
			}))
		}

		return whatsapp.NewProvider(opts...), nil

	case "signal", "rocket", "discord", "mail", "rest":
		// Ces fournisseurs partagent le décodage de la configuration
		// statique : on réutilise le constructeur historique, seul le
		// chemin d'accès à la configuration change.
		return buildCourierProvider(cp)

	default:
		return nil, fmt.Errorf("type de compte %q non supporté", providerType)
	}
}

// whatsmeowLogLevel aligne la verbosité de whatsmeow sur celle du logger de
// l'instance.
//
// whatsmeow écrit sur la sortie standard avec son propre format, hors du
// journal structuré : sans cet alignement il déverse, en DEBUG, une paire
// de trames de maintien toutes les vingt-cinq secondes. Le 2026-08-31, ce
// bruit a enseveli les deux seules lignes qui expliquaient une panne de
// Rocket.Chat — il a fallu remonter vingt mille lignes pour les retrouver.
func whatsmeowLogLevel() string {
	logger := slog.Default()
	ctx := context.Background()

	switch {
	case logger.Enabled(ctx, slog.LevelDebug):
		return "DEBUG"
	case logger.Enabled(ctx, slog.LevelInfo):
		return "INFO"
	case logger.Enabled(ctx, slog.LevelWarn):
		return "WARN"
	default:
		return "ERROR"
	}
}
