package main

import (
	"fmt"

	gomail "gopkg.in/gomail.v2"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// dialSMTP ouvre — et referme — une connexion au serveur d'envoi, sans
// rien expédier. Elle existe pour le test de connexion : sans elle, un
// certificat refusé côté envoi ne se découvrait qu'au moment de confirmer
// un courriel, longtemps après avoir renseigné le formulaire.
func dialSMTP(cfg memberConfig, credential string) error {
	if cfg.SMTPInsecure {
		return nil
	}

	d := gomail.NewDialer(cfg.SMTPHost, cfg.SMTPPort, cfg.Username, credential)
	d.TLSConfig = pluginsdk.TrustedTLSConfig(cfg.SMTPHost, cfg.SMTPTLSFingerprint)
	if cfg.oauth() {
		d.Auth = &smtpXOAUTH2{username: cfg.Username, accessToken: credential}
	}

	closer, err := d.Dial()
	if err != nil {
		logConnectionFailure("smtp", serverAddress(cfg.SMTPHost, cfg.SMTPPort), cfg.SMTPTLSFingerprint != "", err)
		return fmt.Errorf("SMTP connection to %s failed: %w", serverAddress(cfg.SMTPHost, cfg.SMTPPort), err)
	}
	return closer.Close()
}

// sendEmail submits a message. replyTo carries the threading headers when
// answering an existing message — resolved by the plugin from the mailbox,
// never by the model.
func sendEmail(cfg memberConfig, credential, to, subject, body, inReplyTo string) error {
	m := gomail.NewMessage()
	m.SetHeader("From", cfg.From)
	m.SetHeader("To", to)
	m.SetHeader("Subject", subject)
	if inReplyTo != "" {
		m.SetHeader("In-Reply-To", inReplyTo)
		m.SetHeader("References", inReplyTo)
	}
	m.SetBody("text/plain", body)

	d := gomail.NewDialer(cfg.SMTPHost, cfg.SMTPPort, cfg.Username, credential)
	d.TLSConfig = pluginsdk.TrustedTLSConfig(cfg.SMTPHost, cfg.SMTPTLSFingerprint)
	if cfg.oauth() {
		d.Auth = &smtpXOAUTH2{username: cfg.Username, accessToken: credential}
	}
	if cfg.SMTPInsecure {
		// Serveur de test local : ni TLS ni authentification.
		d.SSL = false
		d.Auth = nil
	}

	if err := d.DialAndSend(m); err != nil {
		// Même leçon qu'en IMAP : sans la cause, un certificat refusé
		// ressemble à un mot de passe faux.
		logConnectionFailure("smtp", serverAddress(cfg.SMTPHost, cfg.SMTPPort), cfg.SMTPTLSFingerprint != "", err)
		return fmt.Errorf("SMTP submission to %s:%d failed: %w", cfg.SMTPHost, cfg.SMTPPort, err)
	}
	return nil
}
