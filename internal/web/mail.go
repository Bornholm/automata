package web

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"

	"github.com/bornholm/automata/internal/config"
)

// smtpSender envoie les codes de vérification par SMTP, avec les réglages
// du provider courier « mail » désigné par web.mail_provider. Le provider
// go-courier lui-même est orienté réponse à un fil existant : un courriel
// sortant vers une adresse arbitraire s'envoie plus simplement en direct.
type smtpSender struct {
	addr     string
	issuer   string
	username string
	password string
}

// NewSMTPSender construit un MailSender depuis la configuration, ou (nil,
// nil) si web.mail_provider est vide. Le provider et son type sont déjà
// validés par config.Validate.
func NewSMTPSender(cfg *config.Config) (MailSender, error) {
	if cfg.Web.MailProvider == "" {
		return nil, nil
	}

	provider := cfg.Courier.Providers[cfg.Web.MailProvider]
	var mailCfg config.MailProviderConfig
	if err := provider.DecodeExtra(&mailCfg); err != nil {
		return nil, fmt.Errorf("web: configuration du provider mail %q: %w", cfg.Web.MailProvider, err)
	}
	if mailCfg.SMTP.Address == "" || mailCfg.SMTP.Issuer == "" {
		return nil, fmt.Errorf("web: provider mail %q: smtp.address et smtp.issuer requis", cfg.Web.MailProvider)
	}

	return &smtpSender{
		addr:     mailCfg.SMTP.Address,
		issuer:   mailCfg.SMTP.Issuer,
		username: mailCfg.SMTP.Username,
		password: mailCfg.SMTP.Password,
	}, nil
}

// SendVerificationCode implémente MailSender.
func (s *smtpSender) SendVerificationCode(_ context.Context, email, code string) error {
	var body strings.Builder
	body.WriteString("From: Automata <" + s.issuer + ">\r\n")
	body.WriteString("To: " + email + "\r\n")
	body.WriteString("Subject: Votre code Automata : " + code + "\r\n")
	body.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	body.WriteString("Voici votre code de vérification : " + code + "\r\n\r\n")
	body.WriteString("Saisissez-le dans la page de profil ouverte depuis votre conversation avec Automata. Il expire dans dix minutes.\r\n\r\n")
	body.WriteString("Si vous n'êtes pas à l'origine de cette demande, ignorez simplement ce message.\r\n")

	var auth smtp.Auth
	if s.username != "" {
		host := s.addr
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		auth = smtp.PlainAuth("", s.username, s.password, host)
	}

	if err := smtp.SendMail(s.addr, auth, s.issuer, []string{email}, []byte(body.String())); err != nil {
		return fmt.Errorf("web: envoi smtp: %w", err)
	}

	return nil
}
