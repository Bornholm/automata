package pluginsdk

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
)

// selfSignedServer starts a TLS listener with a freshly minted
// self-signed certificate — exactly what a personal mail or calendar
// server presents, and what ordinary verification refuses.
func selfSignedServer(t *testing.T) (address string, certificate tls.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("clé: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
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
	certificate = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
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
			// La poignée de main suffit à ce qu'on vérifie ici.
			go func() {
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.Close()
			}()
		}
	}()

	return listener.Addr().String(), certificate
}

// dial vérifie qu'une connexion aboutit avec la configuration donnée.
func dialWith(address string, cfg *tls.Config) error {
	conn, err := tls.Dial("tcp", address, cfg)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Sans exception, un certificat auto-signé est refusé : c'est le
// comportement à préserver, et la raison d'être de tout le reste.
func TestTrustedTLSConfig_RefusesWithoutAnException(t *testing.T) {
	address, _ := selfSignedServer(t)
	host, _, _ := net.SplitHostPort(address)

	if err := dialWith(address, TrustedTLSConfig(host, "")); err == nil {
		t.Fatal("un certificat auto-signé a été accepté sans exception")
	}
}

// InspectTLS montre ce que le serveur présente — sans rien accepter — et
// l'exception posée sur cette empreinte fait aboutir la connexion.
func TestInspectTLS_ThenPinnedFingerprintConnects(t *testing.T) {
	address, _ := selfSignedServer(t)
	host, _, _ := net.SplitHostPort(address)

	info, err := InspectTLS(context.Background(), address)
	if err != nil {
		t.Fatalf("InspectTLS: %v", err)
	}

	if !info.SelfSigned {
		t.Error("un certificat auto-signé n'a pas été reconnu comme tel")
	}
	// La cause du refus est dite : c'est elle qui manquait à l'exploitant.
	if info.VerifyError == "" {
		t.Error("aucune raison donnée au refus de vérification")
	}
	if !strings.Contains(info.Subject, "localhost") {
		t.Errorf("sujet inattendu: %q", info.Subject)
	}
	// L'empreinte se lit à l'écran pour être comparée à celle du serveur.
	formatted := info.FormattedFingerprint()
	if strings.Count(formatted, ":") != 31 || strings.ToLower(strings.ReplaceAll(formatted, ":", "")) != info.Fingerprint {
		t.Errorf("empreinte mal formatée: %q", formatted)
	}

	if err := dialWith(address, TrustedTLSConfig(host, info.Fingerprint)); err != nil {
		t.Fatalf("l'exception n'a pas été honorée: %v", err)
	}

	// Ce que le membre a pu recopier, avec ses séparateurs, vaut la même
	// chose.
	if err := dialWith(address, TrustedTLSConfig(host, formatted)); err != nil {
		t.Errorf("empreinte recopiée avec séparateurs refusée: %v", err)
	}
}

// Une exception vaut pour UN certificat. Un autre serveur, présentant le
// sien, reste refusé — ce qu'un « ne rien vérifier » accepterait sans
// broncher.
func TestTrustedTLSConfig_ExceptionDoesNotTrustAnythingElse(t *testing.T) {
	accepted, _ := selfSignedServer(t)
	other, _ := selfSignedServer(t)

	info, err := InspectTLS(context.Background(), accepted)
	if err != nil {
		t.Fatalf("InspectTLS: %v", err)
	}

	host, _, _ := net.SplitHostPort(other)
	err = dialWith(other, TrustedTLSConfig(host, info.Fingerprint))
	if err == nil {
		t.Fatal("un certificat étranger a été accepté par l'exception d'un autre")
	}
	if !strings.Contains(err.Error(), "exception") {
		t.Errorf("message peu explicite: %v", err)
	}
}

func TestNormalizeTLSFingerprint(t *testing.T) {
	valid := strings.Repeat("AB", 32)

	if got := NormalizeTLSFingerprint(valid); got != strings.ToLower(valid) {
		t.Errorf("empreinte valide altérée: %q", got)
	}
	// Séparateurs et espaces, tels qu'on les recopie d'un navigateur.
	spaced := strings.Join(strings.Split(valid, ""), " ")
	if got := NormalizeTLSFingerprint(spaced); got != strings.ToLower(valid) {
		t.Errorf("empreinte espacée refusée: %q", got)
	}

	for _, bad := range []string{"", "pas une empreinte", strings.Repeat("ab", 16)} {
		if got := NormalizeTLSFingerprint(bad); got != "" {
			t.Errorf("NormalizeTLSFingerprint(%q) = %q, attendu vide", bad, got)
		}
	}
}
