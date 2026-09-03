package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// Exception TLS du membre.
//
// Un serveur d'agenda auto-hébergé présente souvent un certificat qu'aucune
// autorité publique n'a signé. La connexion échoue alors, et le formulaire
// — une adresse, un identifiant, un mot de passe — ne laisse rien à faire.
//
// La réponse est celle du navigateur : montrer à la personne CE QUE le
// serveur présente, et épingler ce certificat-là. Un intermédiaire qui en
// présenterait un autre reste refusé, ce qu'un « ne rien vérifier »
// accepterait sans un mot.

// newTransport construit le transport HTTP du client CalDAV, exception
// comprise. Sans exception, la vérification est celle du système.
func newTransport(cfg memberConfig) http.RoundTripper {
	host := tlsServerName(cfg.ServerURL)
	if host == "" {
		return http.DefaultTransport
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = pluginsdk.TrustedTLSConfig(host, cfg.TLSFingerprint)

	return transport
}

// tlsServerName extrait le nom d'hôte de l'adresse du serveur. Vide si
// l'adresse n'est pas exploitable — le client CalDAV le signalera de
// lui-même, avec un meilleur message.
func tlsServerName(serverURL string) string {
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Hostname()
}

// tlsAddress compose le « hôte:port » à inspecter. Le port par défaut est
// celui du schéma ; une adresse en http n'a pas de certificat à montrer.
func tlsAddress(serverURL string) string {
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Hostname() == "" || parsed.Scheme != "https" {
		return ""
	}

	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(parsed.Hostname(), port)
}

// isCertificateError dit si l'échec vient de la vérification du
// certificat. C'est la distinction qui compte pour la personne : une
// adresse fautive se corrige, un certificat s'accepte.
func isCertificateError(err error) bool {
	if err == nil {
		return false
	}

	var unknownAuthority x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var invalid x509.CertificateInvalidError
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostnameError) ||
		errors.As(err, &invalid) || errors.As(err, &recordHeader) {
		return true
	}

	// Les bibliothèques qui enveloppent l'erreur en texte perdent le type ;
	// le motif, lui, survit.
	message := strings.ToLower(err.Error())
	for _, needle := range []string{"x509:", "certificate", "certificat", "tls:"} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// inspectServerCertificate regarde ce que présente le serveur d'agenda,
// sans rien accepter. Utilisé par l'interface après un échec, pour montrer
// le certificat avant que la personne ne décide.
func inspectServerCertificate(ctx context.Context, serverURL string) (pluginsdk.CertificateInfo, bool) {
	address := tlsAddress(serverURL)
	if address == "" {
		return pluginsdk.CertificateInfo{}, false
	}

	info, err := pluginsdk.InspectTLS(ctx, address)
	if err != nil {
		return pluginsdk.CertificateInfo{}, false
	}
	return info, true
}
