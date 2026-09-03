package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// startIMAPServerTLS démarre le serveur IMAP d'essai derrière un
// certificat auto-signé : exactement ce que présente une messagerie
// auto-hébergée, et ce que la vérification refuse.
func startIMAPServerTLS(t *testing.T) (host string, port int) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("clé: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificat: %v", err)
	}

	user := imapmemserver.NewUser(testUser, testPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("création d'INBOX: %v", err)
	}
	mem := imapmemserver.New()
	mem.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := tls.NewListener(raw, &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()

	addr := raw.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// Sans exception, un certificat auto-signé est refusé — et la cause est
// DITE. Elle était jetée : « IMAP connection failed », rien d'autre, ce qui
// rendait le diagnostic impossible à distance.
func TestDialIMAP_SelfSignedIsRefusedWithACause(t *testing.T) {
	host, port := startIMAPServerTLS(t)
	cfg := memberConfig{IMAPHost: host, IMAPPort: port, Username: testUser}

	_, err := dialIMAP(cfg, testPassword)
	if err == nil {
		t.Fatal("un certificat auto-signé a été accepté sans exception")
	}
	if !isCertificateError(err) {
		t.Errorf("l'échec n'a pas été reconnu comme un problème de certificat: %v", err)
	}
	// La cause remonte : sans elle, l'erreur ne distingue pas un
	// certificat d'un port fermé.
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Errorf("cause perdue: %v", err)
	}
	// Et le mot de passe n'y figure jamais.
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("le secret apparaît dans l'erreur: %v", err)
	}
}

// L'exception posée sur l'empreinte du certificat vu fait aboutir la
// connexion — et elle ne vaut que pour celui-là.
func TestDialIMAP_PinnedExceptionConnects(t *testing.T) {
	host, port := startIMAPServerTLS(t)

	info, ok := inspectServerCertificate(context.Background(), host, port)
	if !ok {
		t.Fatal("le certificat du serveur n'a pas pu être inspecté")
	}
	if !info.SelfSigned || info.VerifyError == "" {
		t.Errorf("inspection inattendue: %+v", info)
	}

	cfg := memberConfig{
		IMAPHost: host, IMAPPort: port, Username: testUser,
		IMAPTLSFingerprint: info.Fingerprint,
	}

	client, err := dialIMAP(cfg, testPassword)
	if err != nil {
		t.Fatalf("l'exception n'a pas été honorée: %v", err)
	}
	_ = client.Close()

	// Une empreinte quelconque n'ouvre rien : l'exception épingle, elle ne
	// débraye pas la vérification.
	cfg.IMAPTLSFingerprint = strings.Repeat("ab", 32)
	if _, err := dialIMAP(cfg, testPassword); err == nil {
		t.Error("une empreinte étrangère a été acceptée")
	}
}

// L'exception d'un serveur ne vaut pas pour un autre.
func TestDialIMAP_ExceptionIsPerServer(t *testing.T) {
	firstHost, firstPort := startIMAPServerTLS(t)
	otherHost, otherPort := startIMAPServerTLS(t)

	info, ok := inspectServerCertificate(context.Background(), firstHost, firstPort)
	if !ok {
		t.Fatal("inspection impossible")
	}

	cfg := memberConfig{
		IMAPHost: otherHost, IMAPPort: otherPort, Username: testUser,
		IMAPTLSFingerprint: info.Fingerprint,
	}
	if _, err := dialIMAP(cfg, testPassword); err == nil {
		t.Fatal("l'exception d'un serveur a ouvert la connexion à un autre")
	}
}

// Sans exception, la configuration TLS reste celle du système : rien de ce
// travail ne doit relâcher la vérification par défaut.
func TestTrustedTLSConfig_DefaultStaysStrict(t *testing.T) {
	cfg := pluginsdk.TrustedTLSConfig("imap.example.org", "")
	if cfg.InsecureSkipVerify {
		t.Error("la vérification est désactivée alors qu'aucune exception n'est posée")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Error("un vérificateur personnalisé est posé sans exception")
	}
}
