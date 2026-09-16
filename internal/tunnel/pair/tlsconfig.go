package pair

import (
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
)

// ALPN is the protocol negotiated for the pairing handshake. It is the only
// protocol a connection without a pin may speak.
const ALPN = "relay-pair/1"

// ClientConfig returns a TLS 1.3 client config presenting id and trusting the
// server only by pin. Chain verification is skipped (InsecureSkipVerify) and
// replaced by VerifyConnection, which requires exactly one Ed25519 certificate,
// a negotiated protocol from alpn and, when pin != "", a matching Fingerprint.
//
// pin "" means "accept any gateway key" and is only for pairing: it panics
// unless alpn is exactly [ALPN], since that is a programming error. It also
// panics when alpn is empty. Callers must not replace VerifyConnection or
// enable session resumption on the returned config.
func ClientConfig(id Identity, pin string, alpn ...string) *tls.Config {
	if len(alpn) == 0 {
		panic("pair: ClientConfig needs at least one ALPN protocol")
	}
	if pin == "" && (len(alpn) != 1 || alpn[0] != ALPN) {
		panic("pair: ClientConfig without a pin is only allowed for ALPN " + ALPN)
	}
	cert := id.Cert
	protos := slices.Clone(alpn)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		NextProtos:         protos,
		InsecureSkipVerify: true, // trust comes from the pin checked in VerifyConnection
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPeer(cs, pin, protos)
		},
	}
}

// ServerConfig returns a TLS 1.3 server config presenting id and requiring a
// client certificate, verified like ClientConfig. pin is called once per
// handshake; "" accepts any Ed25519 client certificate but then only ALPN may
// be negotiated, so an unpaired client can reach nothing but pairing. It panics
// when pin is nil or alpn is empty. Session tickets are disabled so every
// connection runs the verification.
func ServerConfig(id Identity, pin func() string, alpn ...string) *tls.Config {
	if pin == nil {
		panic("pair: ServerConfig needs a pin func")
	}
	if len(alpn) == 0 {
		panic("pair: ServerConfig needs at least one ALPN protocol")
	}
	protos := slices.Clone(alpn)
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		NextProtos:             protos,
		Certificates:           []tls.Certificate{id.Cert},
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPeer(cs, pin(), protos)
		},
	}
}

func verifyPeer(cs tls.ConnectionState, pin string, alpn []string) error {
	if cs.Version != tls.VersionTLS13 {
		return errors.New("pair: TLS 1.3 required")
	}
	if cs.DidResume {
		return errors.New("pair: session resumption not allowed")
	}
	if cs.NegotiatedProtocol == "" || !slices.Contains(alpn, cs.NegotiatedProtocol) {
		return fmt.Errorf("pair: unexpected ALPN protocol %q", cs.NegotiatedProtocol)
	}
	fp, err := PeerFingerprint(cs)
	if err != nil {
		return err
	}
	if pin == "" {
		if cs.NegotiatedProtocol != ALPN {
			return errors.New("pair: unpaired peer may only negotiate " + ALPN)
		}
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(fp), []byte(pin)) != 1 {
		return errors.New("pair: peer key does not match pin")
	}
	return nil
}

// PeerFingerprint returns the fingerprint of the peer's certificate on a
// completed handshake. The peer must have presented exactly one Ed25519 certificate.
func PeerFingerprint(cs tls.ConnectionState) (string, error) {
	if len(cs.PeerCertificates) != 1 {
		return "", fmt.Errorf("pair: peer presented %d certificates, want 1", len(cs.PeerCertificates))
	}
	pub, err := leafKey(cs.PeerCertificates[0])
	if err != nil {
		return "", err
	}
	return Fingerprint(pub), nil
}
