package registry

import (
	"fmt"
	"net/url"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/discord"
	"github.com/bornholm/go-courier/provider/mail"
	"github.com/bornholm/go-courier/provider/rocket"
	"github.com/bornholm/go-courier/provider/signal"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/config"
)

// buildCourierProviders construit un courier.Provider réel pour chaque
// fournisseur déclaré dans cfg.Courier.Providers.
//
// Tous les constructeurs sont paresseux (convention go-courier : aucune
// connexion avant Listen/Send) : construire ici ne touche pas le réseau, et
// une erreur de construction est toujours une erreur de configuration. Les
// champs requis sont déjà vérifiés au chargement (validateCourierProviders,
// mêmes structs) : le décodage ne peut échouer ici que si la configuration
// n'est pas passée par Load, ce que les erreurs ci-dessous couvrent quand
// même — jamais de panique sur une config construite à la main.
func buildCourierProviders(cfg *config.Config) (map[string]courier.Provider, error) {
	providers := make(map[string]courier.Provider, len(cfg.Courier.Providers))

	for name, cp := range cfg.Courier.Providers {
		provider, err := buildCourierProvider(cp)
		if err != nil {
			return nil, fmt.Errorf("fournisseur courier %q: %w", name, err)
		}
		providers[name] = provider
	}

	return providers, nil
}

func buildCourierProvider(cp config.CourierProvider) (courier.Provider, error) {
	switch cp.Type {
	case "whatsapp":
		var c config.WhatsAppProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.SessionPath == "" {
			return nil, fmt.Errorf("champ session_path requis et non vide")
		}

		return whatsapp.NewProvider(whatsapp.WithDBPath(c.SessionPath)), nil

	case "signal":
		var c config.SignalProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.Account == "" {
			return nil, fmt.Errorf("champ account requis et non vide")
		}

		opts := []signal.OptionFunc{signal.WithAccount(c.Account)}
		if c.Address != "" {
			opts = append(opts, signal.WithAddress(c.Address))
		}

		return signal.NewProvider(opts...), nil

	case "discord":
		var c config.DiscordProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.Token == "" {
			return nil, fmt.Errorf("champ token requis et non vide")
		}

		return discord.NewProvider(discord.WithToken(c.Token)), nil

	case "rocket":
		var c config.RocketProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.ServerURL == "" || c.Username == "" || c.Password == "" {
			return nil, fmt.Errorf("champs server_url, username et password requis")
		}

		serverURL, err := url.Parse(c.ServerURL)
		if err != nil {
			return nil, fmt.Errorf("server_url invalide: %w", err)
		}

		return rocket.NewProvider(
			rocket.WithServerURL(serverURL),
			rocket.WithCredentials(c.Username, c.Password),
		), nil

	case "mail":
		var c config.MailProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.IMAP.Address == "" || c.SMTP.Address == "" {
			return nil, fmt.Errorf("champs imap.address et smtp.address requis")
		}

		opts := []mail.OptionFunc{
			mail.WithIMAP(c.IMAP.Address, c.IMAP.Username, c.IMAP.Password),
			mail.WithSMTP(c.SMTP.Address, c.SMTP.Issuer, c.SMTP.Username, c.SMTP.Password),
		}
		if interval := c.IMAP.CheckInterval.Duration(); interval > 0 {
			opts = append(opts, mail.WithIMAPCheckInterval(interval))
		}
		if len(c.IMAP.Folders) > 0 {
			opts = append(opts, mail.WithIMAPFolders(c.IMAP.Folders...))
		}

		return mail.NewProvider(opts...), nil

	default:
		return nil, fmt.Errorf("type %q non supporté (types: %s)", cp.Type, config.SupportedCourierProviders)
	}
}
