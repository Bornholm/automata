package registry

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/bornholm/go-courier"
	"github.com/bornholm/go-courier/provider/discord"
	"github.com/bornholm/go-courier/provider/mail"
	"github.com/bornholm/go-courier/provider/rest"
	"github.com/bornholm/go-courier/provider/rocket"
	"github.com/bornholm/go-courier/provider/signal"
	"github.com/bornholm/go-courier/provider/whatsapp"

	"github.com/bornholm/automata/internal/config"
)

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

	case "rest":
		var c config.RestProviderConfig
		if err := cp.DecodeExtra(&c); err != nil {
			return nil, err
		}
		if c.Address == "" {
			return nil, fmt.Errorf("champ address requis et non vide")
		}
		if len(c.Users) == 0 {
			return nil, fmt.Errorf("champ users requis : au moins une identité avec son jeton")
		}

		tokens := make(map[string]courier.User, len(c.Users))
		for _, user := range c.Users {
			if user.Token == "" || user.ID == "" {
				return nil, fmt.Errorf("chaque entrée de users demande token et id")
			}
			displayName := user.DisplayName
			if displayName == "" {
				displayName = user.ID
			}
			tokens[user.Token] = courier.NewUser(courier.UserID(user.ID), displayName)
		}

		opts := []rest.OptionFunc{
			rest.WithAddress(c.Address),
			rest.WithTokens(tokens),
			// L'identité sous laquelle Automata parle : c'est elle que
			// la règle de mention des groupes compare aux mentions
			// reçues.
			rest.WithSelf(courier.NewUser("automata", "Automata")),
			// Un canal dont l'identifiant commence par « group- » est
			// traité comme un groupe : c'est ce qui permet d'éprouver
			// dans le même compte une conversation privée et une
			// conversation de groupe, qui ne suivent pas les mêmes
			// règles de mémoire ni de permissions.
			rest.WithChannelResolver(func(channelID courier.ChannelID) courier.Channel {
				kind := courier.ChannelKindDirect
				if strings.HasPrefix(string(channelID), "group-") {
					kind = courier.ChannelKindGroup
				}
				return courier.NewChannel(channelID, kind, string(channelID))
			}),
		}
		if len(c.CORSOrigins) > 0 {
			opts = append(opts, rest.WithCORSOrigins(c.CORSOrigins...))
		}

		return rest.NewProvider(opts...), nil

	default:
		return nil, fmt.Errorf("type %q non supporté (types: %s)", cp.Type, config.SupportedCourierProviders)
	}
}
