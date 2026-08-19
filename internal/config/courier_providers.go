package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// SupportedCourierProviders énumère, pour les messages d'erreur, les types
// acceptés de courier.providers.<nom>.type.
const SupportedCourierProviders = `"whatsapp", "signal", "discord", "rocket", "mail", "rest"`

// Les structs suivantes typent les champs libres (Extra) de chaque type de
// fournisseur courier. Elles servent deux fois : à la validation au
// chargement (champs requis vérifiés AVANT le démarrage, comme pour
// llm_clients) et à la construction réelle dans internal/registry — une
// seule définition, aucune divergence possible entre ce que la validation
// accepte et ce que la construction attend.

// WhatsAppProviderConfig configure un fournisseur WhatsApp (whatsmeow).
type WhatsAppProviderConfig struct {
	// SessionPath est le chemin de la base SQLite portant la session liée.
	SessionPath string `yaml:"session_path"`
}

// SignalProviderConfig configure un fournisseur Signal, adossé au daemon
// signal-cli et à son interface JSON-RPC.
type SignalProviderConfig struct {
	// Address du daemon : "tcp://hôte:port" ou "unix:///chemin/socket".
	Address string `yaml:"address"`
	// Account est le numéro E.164 du compte local (ex: "+33612345678").
	Account string `yaml:"account"`
}

// DiscordProviderConfig configure un fournisseur Discord.
type DiscordProviderConfig struct {
	Token string `yaml:"token"`
}

// RocketProviderConfig configure un fournisseur Rocket.Chat.
type RocketProviderConfig struct {
	ServerURL string `yaml:"server_url"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
}

// MailProviderConfig configure un fournisseur courriel (IMAP + SMTP).
type MailProviderConfig struct {
	IMAP struct {
		Address  string `yaml:"address"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
		// CheckInterval est la période de relève. Vide = défaut du provider
		// (une minute).
		CheckInterval Duration `yaml:"check_interval"`
		// Folders liste les dossiers relevés. Vide = INBOX.
		Folders []string `yaml:"folders"`
	} `yaml:"imap"`
	SMTP struct {
		Address string `yaml:"address"`
		// Issuer est l'adresse d'expéditeur des messages sortants.
		Issuer   string `yaml:"issuer"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"smtp"`
}

// DecodeExtra décode les champs libres du fournisseur dans target, par un
// aller-retour YAML : les champs imbriqués (mail.imap.*) sont déjà des maps
// génériques à ce stade, et yaml sait les re-projeter sur une struct typée.
func (p CourierProvider) DecodeExtra(target any) error {
	raw, err := yaml.Marshal(p.Extra)
	if err != nil {
		return fmt.Errorf("encodage des champs du fournisseur: %w", err)
	}
	if err := yaml.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("décodage des champs du fournisseur: %w", err)
	}
	return nil
}

// validateCourierProviders vérifie que chaque fournisseur déclaré a un type
// connu et ses champs requis : un identifiant manquant doit refuser le
// chargement, pas échouer au premier message (même principe que
// validateLLMClients).
func validateCourierProviders(cfg *Config) []error {
	var errs []error

	for _, name := range sortedKeys(cfg.Courier.Providers) {
		cp := cfg.Courier.Providers[name]
		prefix := fmt.Sprintf("courier.providers.%s", name)

		fail := func(field, message string) {
			errs = append(errs, fmt.Errorf("%s.%s: %s", prefix, field, message))
		}

		switch cp.Type {
		case "whatsapp":
			var c WhatsAppProviderConfig
			if err := cp.DecodeExtra(&c); err != nil {
				fail("", err.Error())
				continue
			}
			if c.SessionPath == "" {
				fail("session_path", "requis (base SQLite de la session liée)")
			}
		case "signal":
			var c SignalProviderConfig
			if err := cp.DecodeExtra(&c); err != nil {
				fail("", err.Error())
				continue
			}
			if c.Account == "" {
				fail("account", "requis (numéro E.164 du compte, ex: \"+33612345678\")")
			}
			// address est facultative : le défaut du provider vise le daemon
			// local sur son port standard.
		case "discord":
			var c DiscordProviderConfig
			if err := cp.DecodeExtra(&c); err != nil {
				fail("", err.Error())
				continue
			}
			if c.Token == "" {
				fail("token", "requis (jeton de bot Discord)")
			}
		case "rocket":
			var c RocketProviderConfig
			if err := cp.DecodeExtra(&c); err != nil {
				fail("", err.Error())
				continue
			}
			if c.ServerURL == "" {
				fail("server_url", "requis")
			}
			if c.Username == "" || c.Password == "" {
				fail("username/password", "requis")
			}
		case "mail":
			var c MailProviderConfig
			if err := cp.DecodeExtra(&c); err != nil {
				fail("", err.Error())
				continue
			}
			if c.IMAP.Address == "" || c.IMAP.Username == "" || c.IMAP.Password == "" {
				fail("imap", "address, username et password requis (réception)")
			}
			if c.SMTP.Address == "" || c.SMTP.Issuer == "" {
				fail("smtp", "address et issuer requis (envoi)")
			}
			if interval := c.IMAP.CheckInterval.Duration(); interval < 0 || (interval > 0 && interval < time.Second) {
				fail("imap.check_interval", "doit valoir au moins 1s")
			}
		case "rest":
			var c RestProviderConfig
			if err := cp.DecodeExtra(&c); err != nil {
				fail("", err.Error())
				continue
			}
			if c.Address == "" {
				fail("address", "requis (adresse d'écoute de l'API)")
			}
			if len(c.Users) == 0 {
				fail("users", "au moins une identité requise : sans jeton, l'API refuse tout")
			}
			for i, user := range c.Users {
				if user.Token == "" || user.ID == "" {
					fail(fmt.Sprintf("users[%d]", i), "token et id requis")
				}
			}
		case "":
			errs = append(errs, fmt.Errorf("%s.type: requis (types: %s)", prefix, SupportedCourierProviders))
		default:
			errs = append(errs, fmt.Errorf("%s.type: %q non supporté (types: %s)", prefix, cp.Type, SupportedCourierProviders))
		}
	}

	return errs
}

// RestProviderConfig configure un fournisseur REST : une API HTTP JSON où
// les messages entrants sont postés et les sortants lus en flux
// d'événements. Aucune messagerie réelle derrière — c'est le compte des
// tests de bout en bout, celui qui permet d'éprouver une conversation, un
// rattachement par jeton ou une confirmation d'achat sans dépendre de
// WhatsApp ni d'un téléphone.
type RestProviderConfig struct {
	// Address est l'adresse d'écoute de l'API (ex. « 127.0.0.1:8095 »).
	// À garder locale ou derrière un proxy : elle porte des
	// conversations.
	Address string `yaml:"address"`
	// Users associe un jeton porteur à une identité. Sans jeton, l'API
	// refuse tout : exposer des conversations à qui passe serait un
	// mauvais marché. Les jetons sont des secrets, donc à passer par
	// `env`.
	Users []RestProviderUser `yaml:"users"`
	// CORSOrigins autorise un client web local. Vide = aucun en-tête
	// CORS.
	CORSOrigins []string `yaml:"cors_origins"`
}

// RestProviderUser est une identité de l'API REST.
type RestProviderUser struct {
	// Token est le jeton porteur présenté en « Authorization: Bearer ».
	Token string `yaml:"token"`
	// ID est l'identifiant d'expéditeur vu par Automata : c'est lui que
	// l'on retrouve dans les origines et les rattachements.
	ID string `yaml:"id"`
	// DisplayName est le nom affiché ; vide, l'identifiant fait office.
	DisplayName string `yaml:"display_name"`
}
