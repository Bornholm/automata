package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
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

	info, ok := inspectServerCertificate(context.Background(), "imap", host, port)
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

	info, ok := inspectServerCertificate(context.Background(), "imap", firstHost, firstPort)
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

// serveSMTPUntilSTARTTLS répond aux commandes SMTP jusqu'à STARTTLS, qu'il
// accepte en rendant vrai — au tour de l'appelant de monter en TLS. Sur le
// canal chiffré, la même boucle sert le second EHLO et rend faux à la
// fermeture.
func serveSMTPUntilSTARTTLS(r io.Reader, w io.Writer) bool {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return false
		}

		switch command := strings.ToUpper(line); {
		case strings.HasPrefix(command, "EHLO"):
			if _, err := w.Write([]byte("250-test\r\n250 STARTTLS\r\n")); err != nil {
				return false
			}
		case strings.HasPrefix(command, "STARTTLS"):
			if _, err := w.Write([]byte("220 ready\r\n")); err != nil {
				return false
			}
			return true
		case strings.HasPrefix(command, "QUIT"):
			_, _ = w.Write([]byte("221 bye\r\n"))
			return false
		default:
			if _, err := w.Write([]byte("250 OK\r\n")); err != nil {
				return false
			}
		}
	}
}

// startSMTPServerSTARTTLS démarre un serveur de soumission d'essai qui
// commence EN CLAIR et ne monte en TLS que sur STARTTLS, derrière un
// certificat auto-signé — le port 587 d'une messagerie auto-hébergée.
//
// Il ne parle que ce qu'il faut pour montrer un certificat : bannière,
// EHLO, STARTTLS, poignée de main. C'est exactement le chemin que
// l'inspection doit savoir suivre.
func startSMTPServerSTARTTLS(t *testing.T) (host string, port int) {
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
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("écoute: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()

				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

				if _, err := conn.Write([]byte("220 test ESMTP\r\n")); err != nil {
					return
				}

				// En clair d'abord, puis, une fois STARTTLS accepté, le même
				// dialogue sur le canal chiffré : un client renvoie un EHLO
				// après la poignée de main, et un serveur qui raccroche là
				// n'est pas un serveur de soumission.
				upgraded := serveSMTPUntilSTARTTLS(conn, conn)
				if !upgraded {
					return
				}

				tlsConn := tls.Server(conn, tlsConfig)
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				serveSMTPUntilSTARTTLS(tlsConn, tlsConn)
			}()
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)

	return "127.0.0.1", addr.Port
}

// Sur un port de soumission, le certificat n'apparaît qu'après STARTTLS.
// Une poignée de main directe s'y heurte à la bannière en clair, et
// l'inspection ne montrait alors RIEN : la personne lisait l'erreur de
// certificat sans jamais pouvoir l'accepter, le panneau d'exception exigeant
// un certificat pour s'afficher. Vu sur smtp.cadoles.com:587 le 2026-09-03.
func TestInspectServerCertificate_SMTPSubmissionPortUsesSTARTTLS(t *testing.T) {
	host, port := startSMTPServerSTARTTLS(t)

	if _, ok := inspectServerCertificate(context.Background(), "imap", host, port); ok {
		t.Fatal("une poignée de main directe a prétendu réussir sur un port STARTTLS")
	}

	info, ok := inspectServerCertificate(context.Background(), "smtp", host, port)
	if !ok {
		t.Fatal("le certificat présenté après STARTTLS n'a pas été inspecté")
	}
	if !info.SelfSigned || info.VerifyError == "" {
		t.Errorf("inspection inattendue: %+v", info)
	}
	// L'empreinte est ce que la personne compare, et ce que l'exception
	// épingle : sans elle, l'écran n'a rien à proposer.
	if info.Fingerprint == "" {
		t.Error("empreinte vide")
	}
}
