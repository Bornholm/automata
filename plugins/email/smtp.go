package main

import (
	"fmt"

	gomail "gopkg.in/gomail.v2"
)

// sendEmail submits a message. replyTo carries the threading headers when
// answering an existing message — resolved by the plugin from the mailbox,
// never by the model.
func sendEmail(cfg memberConfig, password, to, subject, body, inReplyTo string) error {
	m := gomail.NewMessage()
	m.SetHeader("From", cfg.From)
	m.SetHeader("To", to)
	m.SetHeader("Subject", subject)
	if inReplyTo != "" {
		m.SetHeader("In-Reply-To", inReplyTo)
		m.SetHeader("References", inReplyTo)
	}
	m.SetBody("text/plain", body)

	d := gomail.NewDialer(cfg.SMTPHost, cfg.SMTPPort, cfg.Username, password)
	if cfg.SMTPInsecure {
		// Serveur de test local : ni TLS ni authentification.
		d.SSL = false
		d.Auth = nil
	}

	if err := d.DialAndSend(m); err != nil {
		return fmt.Errorf("SMTP submission failed")
	}
	return nil
}
