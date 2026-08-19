package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
)

// PlatformTypes énumère les types de comptes de messagerie gérables depuis
// l'administration, dans l'ordre d'affichage.
var PlatformTypes = []string{"whatsapp", "signal", "rocket", "discord", "mail"}

// migratePlatforms importe dans la table platforms les comptes déclarés
// dans courier.providers, une seule fois : les lignes existantes ne sont
// jamais écrasées.
//
// La configuration est reprise à l'IDENTIQUE — en particulier le chemin de
// session WhatsApp : la migration ne doit jamais coûter un ré-appairage à
// une instance en production.
func migratePlatforms(ctx context.Context, db *persistence.DB, cfg *config.Config, secrets *secretbox.Box, logger *slog.Logger) error {
	if len(cfg.Courier.Providers) == 0 {
		return nil
	}

	repo := persistence.NewPlatformRepository()
	now := time.Now()

	var migrated int

	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		for name, provider := range cfg.Courier.Providers {
			if _, exists, err := repo.FindByID(ctx, tx, name); err != nil {
				return err
			} else if exists {
				continue
			}

			raw, err := json.Marshal(provider.Extra)
			if err != nil {
				return fmt.Errorf("sérialisation de la configuration du compte %q: %w", name, err)
			}

			sealed, err := secrets.Seal(string(raw))
			if err != nil {
				return err
			}

			if err := repo.Insert(ctx, tx, persistence.Platform{
				ID:          name,
				Type:        provider.Type,
				DisplayName: platformTypeLabel(provider.Type),
				Config:      sealed,
				Enabled:     true,
				CreatedAt:   now,
				UpdatedAt:   now,
			}, true); err != nil {
				return err
			}
			migrated++
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("registry: migration des comptes de messagerie: %w", err)
	}

	if migrated > 0 {
		logger.InfoContext(ctx, "registry: comptes de messagerie importés depuis la configuration", "platforms", migrated)
	}

	return nil
}

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

		opts := []whatsapp.OptionFunc{whatsapp.WithDBPath(c.SessionPath)}
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
