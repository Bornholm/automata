package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/bornholm/automata/internal/i18n"
	"net/smtp"
	"strings"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/secretbox"
	"github.com/bornholm/automata/internal/web/core"
)

// smtpSender envoie les codes de vérification par SMTP, avec les réglages
// du compte de messagerie de type « mail » désigné par web.mail_provider.
//
// Le compte vit dans la table platforms — les comptes ne se déclarent plus
// dans le fichier de configuration — et sa configuration est RELUE à chaque
// envoi : un mot de passe SMTP corrigé dans l'administration sert au code
// de vérification suivant, sans redémarrage. Un envoi est un événement
// rare (un code par vérification d'adresse), la lecture ne coûte rien.
type smtpSender struct {
	db      *persistence.DB
	secrets *secretbox.Box
	repo    *persistence.PlatformRepository
	// name est l'identifiant du compte dans la table platforms
	// (web.mail_provider).
	name string
}

// NewSMTPSender construit un core.MailSender adossé au compte de messagerie
// nommé par web.mail_provider, ou (nil, nil) si le champ est vide. Le
// compte lui-même n'est vérifié qu'À L'ENVOI : il se crée et se corrige en
// ligne, le démarrage n'a pas à le connaître.
func NewSMTPSender(cfg *config.Config, db *persistence.DB, secrets *secretbox.Box) (core.MailSender, error) {
	if cfg.Web.MailProvider == "" {
		return nil, nil
	}
	if db == nil || secrets == nil {
		return nil, fmt.Errorf("web: web.mail_provider requiert la base et le déchiffrement des comptes")
	}

	return &smtpSender{
		db:      db,
		secrets: secrets,
		repo:    persistence.NewPlatformRepository(),
		name:    cfg.Web.MailProvider,
	}, nil
}

// smtpSettings recharge les réglages SMTP du compte.
func (s *smtpSender) smtpSettings(ctx context.Context) (config.MailProviderConfig, error) {
	var (
		platform persistence.Platform
		found    bool
	)
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		platform, found, err = s.repo.FindByID(ctx, tx, s.name)
		return err
	})
	if err != nil {
		return config.MailProviderConfig{}, fmt.Errorf("web: lecture du compte mail %q: %w", s.name, err)
	}
	if !found {
		return config.MailProviderConfig{}, fmt.Errorf("web: web.mail_provider désigne %q, absent des comptes de messagerie — créez-le dans l'administration", s.name)
	}
	if platform.Type != "mail" {
		return config.MailProviderConfig{}, fmt.Errorf("web: le compte %q est de type %q, un compte mail est requis", s.name, platform.Type)
	}

	opened, err := s.secrets.Open(platform.Config)
	if err != nil {
		return config.MailProviderConfig{}, fmt.Errorf("web: déchiffrement du compte mail %q: %w", s.name, err)
	}

	var mailCfg config.MailProviderConfig
	if err := json.Unmarshal([]byte(opened), &mailCfg); err != nil {
		return config.MailProviderConfig{}, fmt.Errorf("web: configuration du compte mail %q illisible: %w", s.name, err)
	}
	if mailCfg.SMTP.Address == "" || mailCfg.SMTP.Issuer == "" {
		return config.MailProviderConfig{}, fmt.Errorf("web: compte mail %q: smtp.address et smtp.issuer requis", s.name)
	}

	return mailCfg, nil
}

// SendVerificationCode implémente core.MailSender.
func (s *smtpSender) SendVerificationCode(ctx context.Context, locale i18n.Locale, email, code string) error {
	settings, err := s.smtpSettings(ctx)
	if err != nil {
		return err
	}

	var body strings.Builder
	body.WriteString("From: Automata <" + settings.SMTP.Issuer + ">\r\n")
	body.WriteString("To: " + email + "\r\n")
	body.WriteString("Subject: " + i18n.T(locale, "mail.verification.subject", code) + "\r\n")
	body.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	body.WriteString(i18n.T(locale, "mail.verification.intro", code) + "\r\n\r\n")
	body.WriteString(i18n.T(locale, "mail.verification.instructions") + "\r\n\r\n")
	body.WriteString(i18n.T(locale, "mail.verification.ignore") + "\r\n")

	var auth smtp.Auth
	if settings.SMTP.Username != "" {
		host := settings.SMTP.Address
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		auth = smtp.PlainAuth("", settings.SMTP.Username, settings.SMTP.Password, host)
	}

	if err := smtp.SendMail(settings.SMTP.Address, auth, settings.SMTP.Issuer, []string{email}, []byte(body.String())); err != nil {
		return fmt.Errorf("web: envoi smtp: %w", err)
	}

	return nil
}
