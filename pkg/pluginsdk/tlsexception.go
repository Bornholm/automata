package pluginsdk

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Trusting one certificate a person chose to trust.
//
// Self-hosted mail and calendar servers routinely present a certificate no
// public authority signed: self-signed, an internal CA, or simply expired.
// The connection then fails, and there is nothing the member can do about
// it from a form that only asks for a host and a password.
//
// What is offered here is the browser's answer, not a blanket "skip
// verification": the member is shown WHAT the server presented and pins
// that exact certificate. A machine-in-the-middle presenting a different
// one is still refused, which a blanket skip would happily accept.

// TLSInspectTimeout bounds an inspection handshake.
const TLSInspectTimeout = 10 * time.Second

// CertificateInfo describes what a server presented, in the terms a person
// needs in order to decide whether to trust it.
type CertificateInfo struct {
	// Subject and Issuer are the human-readable distinguished names.
	Subject   string
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
	// Fingerprint is the SHA-256 of the leaf certificate, lowercase hex
	// without separators. It is what TrustedTLSConfig pins.
	Fingerprint string
	// SelfSigned reports a certificate that signed itself — the common
	// case on a personal server.
	SelfSigned bool
	// VerifyError is why the usual verification refused it, in plain
	// terms. Empty when the certificate verifies normally, in which case
	// no exception is needed at all.
	VerifyError string
}

// FormattedFingerprint returns the fingerprint the way certificate
// viewers show it: uppercase hex, colon-separated. Reading it off a
// screen to compare with the server's own is the whole point.
func (c CertificateInfo) FormattedFingerprint() string {
	upper := strings.ToUpper(c.Fingerprint)

	var b strings.Builder
	for i := 0; i < len(upper); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(upper[i:min(i+2, len(upper))])
	}
	return b.String()
}

// InspectTLS opens a TLS connection to address ("host:port") WITHOUT
// verifying anything, and reports what the server presented. It exists so
// a member can see a certificate before deciding to trust it; nothing is
// trusted as a result of calling it.
func InspectTLS(ctx context.Context, address string) (CertificateInfo, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return CertificateInfo{}, fmt.Errorf("address %q is not host:port: %w", address, err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, TLSInspectTimeout)
	defer cancel()

	dialer := &tls.Dialer{Config: &tls.Config{
		// Deliberately: the point is to LOOK at what is served, including
		// what verification would reject. Nothing here trusts it.
		InsecureSkipVerify: true,
		ServerName:         host,
	}}

	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return CertificateInfo{}, err
	}
	defer func() { _ = conn.Close() }()

	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return CertificateInfo{}, errors.New("the server presented no certificate")
	}

	return describeCertificate(state.PeerCertificates, host), nil
}

// InspectSMTPSTARTTLS reports what an SMTP server presents on a port that
// starts in the clear and upgrades with STARTTLS — the submission port 587,
// almost always. Nothing is trusted as a result of calling it.
//
// It exists because InspectTLS cannot see that certificate at all: a direct
// handshake against 587 is answered by the SMTP greeting, not by a TLS
// server hello, and fails with "wrong version number". The member was then
// shown the connection error with no way to act on it, while the exception
// panel — which needs a certificate to display — stayed hidden. Observed on
// smtp.cadoles.com:587, 2026-09-03.
//
// Inspection must therefore reach the certificate the same way the real
// connection does. What the caller decides here is only HOW to reach it;
// what is presented is what gets pinned either way.
func InspectSMTPSTARTTLS(ctx context.Context, address string) (CertificateInfo, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return CertificateInfo{}, fmt.Errorf("address %q is not host:port: %w", address, err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, TLSInspectTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return CertificateInfo{}, err
	}
	defer func() { _ = conn.Close() }()

	// The whole exchange — greeting, EHLO, STARTTLS, handshake — is bounded
	// by the same deadline: a server that answers the greeting and then
	// stalls must not hold the request open.
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return CertificateInfo{}, err
	}
	defer func() { _ = client.Close() }()

	if ok, _ := client.Extension("STARTTLS"); !ok {
		return CertificateInfo{}, errors.New("the server does not offer STARTTLS on this port")
	}

	// Deliberately: the point is to LOOK at what is served, including what
	// verification would reject. Nothing here trusts it.
	if err := client.StartTLS(&tls.Config{InsecureSkipVerify: true, ServerName: host}); err != nil {
		return CertificateInfo{}, err
	}

	state, ok := client.TLSConnectionState()
	if !ok || len(state.PeerCertificates) == 0 {
		return CertificateInfo{}, errors.New("the server presented no certificate")
	}

	return describeCertificate(state.PeerCertificates, host), nil
}

// describeCertificate builds the report, re-running the normal
// verification to say precisely what is wrong with the chain.
func describeCertificate(chain []*x509.Certificate, serverName string) CertificateInfo {
	leaf := chain[0]
	sum := sha256.Sum256(leaf.Raw)

	info := CertificateInfo{
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		NotBefore:   leaf.NotBefore,
		NotAfter:    leaf.NotAfter,
		Fingerprint: hex.EncodeToString(sum[:]),
		SelfSigned:  leaf.Subject.String() == leaf.Issuer.String(),
	}

	if err := verifyChain(chain, serverName); err != nil {
		info.VerifyError = err.Error()
	}

	return info
}

// verifyChain runs the standard verification against the system roots.
func verifyChain(chain []*x509.Certificate, serverName string) error {
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}

	_, err := chain[0].Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Intermediates: intermediates,
	})
	return err
}

// TrustedTLSConfig returns the TLS settings for a connection to
// serverName, honouring the member's exception.
//
// fingerprint empty gives ordinary verification — the normal case, and the
// one every member starts from. With a fingerprint, the certificate is
// accepted when it either verifies normally OR matches the pin exactly.
// Keeping both branches matters: a server whose certificate becomes
// properly signed keeps working, and the exception does not quietly turn
// into "trust anything from now on".
func TrustedTLSConfig(serverName, fingerprint string) *tls.Config {
	pinned := normalizeFingerprint(fingerprint)
	if pinned == "" {
		return &tls.Config{ServerName: serverName}
	}

	return &tls.Config{
		ServerName: serverName,
		// The library check is turned off so that VerifyPeerCertificate
		// below can run at all; it does the full verification itself and
		// refuses everything that neither verifies nor matches the pin.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pinnedVerifier(serverName, pinned),
	}
}

// pinnedVerifier accepts a chain that verifies normally, or whose leaf is
// exactly the certificate the member accepted.
func pinnedVerifier(serverName, pinned string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("the server presented no certificate")
		}

		sum := sha256.Sum256(rawCerts[0])
		if hex.EncodeToString(sum[:]) == pinned {
			return nil
		}

		chain := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("unreadable certificate: %w", err)
			}
			chain = append(chain, cert)
		}

		if err := verifyChain(chain, serverName); err != nil {
			// Naming both reasons is what tells an operator whether the
			// server renewed its certificate or something else answered.
			return fmt.Errorf("the certificate neither verifies nor matches the accepted exception: %w", err)
		}
		return nil
	}
}

// normalizeFingerprint accepts what a person may paste — colons, spaces,
// any case — and returns the lowercase hex form, or empty when it is not a
// SHA-256 fingerprint at all.
func normalizeFingerprint(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			return r
		case r >= 'A' && r <= 'F':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, raw)

	if len(cleaned) != sha256.Size*2 {
		return ""
	}
	return cleaned
}

// NormalizeTLSFingerprint is normalizeFingerprint, exported for plugins
// that store what a member pasted. It returns "" for anything that is not
// a SHA-256 fingerprint, which is the value to store when refusing it.
func NormalizeTLSFingerprint(raw string) string { return normalizeFingerprint(raw) }
