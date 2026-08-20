package main

import (
	"fmt"
	"net/smtp"
)

// XOAUTH2 n'est pas dans go-sasl ni dans net/smtp : le mécanisme tient en
// une ligne (une chaîne d'initialisation, aucun échange), on l'implémente
// pour les deux bibliothèques plutôt que d'ajouter une dépendance.

// imapXOAUTH2 implémente sasl.Client pour go-imap.
type imapXOAUTH2 struct {
	username    string
	accessToken string
}

func (a *imapXOAUTH2) Start() (mech string, ir []byte, err error) {
	return "XOAUTH2", []byte(xoauth2(a.username, a.accessToken)), nil
}

// Next : le serveur ne répond que sur échec, avec un défi JSON auquel il
// faut répondre par une chaîne vide avant de recevoir l'erreur.
func (a *imapXOAUTH2) Next(challenge []byte) ([]byte, error) {
	return []byte(""), nil
}

// smtpXOAUTH2 implémente smtp.Auth pour net/smtp (via gomail).
type smtpXOAUTH2 struct {
	username    string
	accessToken string
}

func (a *smtpXOAUTH2) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		// Un jeton porteur ne traverse jamais un canal en clair.
		return "", nil, fmt.Errorf("XOAUTH2 requires a TLS connection")
	}
	return "XOAUTH2", []byte(xoauth2(a.username, a.accessToken)), nil
}

func (a *smtpXOAUTH2) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return []byte(""), nil
	}
	return nil, nil
}
