package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// Exception TLS du membre.
//
// Un serveur de messagerie auto-hébergé présente souvent un certificat
// qu'aucune autorité publique n'a signé. La connexion échoue alors, et le
// formulaire — un hôte, un port, un mot de passe — ne laisse rien à faire.
//
// La réponse est celle du navigateur : montrer à la personne CE QUE le
// serveur présente, et épingler ce certificat-là. Un intermédiaire qui en
// présenterait un autre reste refusé, ce qu'un « ne rien vérifier »
// accepterait sans un mot.

// isCertificateError dit si l'échec vient de la vérification du
// certificat. C'est la distinction qui compte pour la personne : un mot de
// passe se corrige, un certificat s'accepte.
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

// logConnectionFailure journalise un échec avec de quoi le comprendre : le
// protocole, l'adresse du serveur, la présence d'une exception, et la
// cause telle qu'elle remonte. Jamais l'identifiant ni le mot de passe.
func logConnectionFailure(protocol, address string, hasException bool, err error) {
	attrs := []any{
		"protocol", protocol,
		"server", address,
		"has_tls_exception", hasException,
		"error", err.Error(),
	}
	// Un échec de certificat a sa propre issue — poser une exception — et
	// mérite d'être nommé comme tel plutôt que noyé dans « connexion
	// impossible ».
	if isCertificateError(err) {
		attrs = append(attrs, "cause", "certificat refusé")
	}

	slog.Warn("email: connexion au serveur impossible", attrs...)
}

// serverAddress compose le « hôte:port » d'un des deux serveurs.
func serverAddress(host string, port int) string {
	if host == "" || port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// inspectServerCertificate regarde ce que présente un serveur, sans rien
// accepter. Utilisé par l'interface après un échec, pour montrer le
// certificat avant que la personne ne décide.
func inspectServerCertificate(ctx context.Context, host string, port int) (pluginsdk.CertificateInfo, bool) {
	address := serverAddress(host, port)
	if address == "" {
		return pluginsdk.CertificateInfo{}, false
	}

	info, err := pluginsdk.InspectTLS(ctx, address)
	if err != nil {
		return pluginsdk.CertificateInfo{}, false
	}
	return info, true
}
