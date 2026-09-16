// Package pair provides tunnel identities, one-time pairing tokens, pinned
// mutual-TLS configs and the pairing handshake between a home Relay and its
// gateway.
package pair

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	fingerprintPrefix = "sha256:"
	identityValidity  = 20 * 365 * 24 * time.Hour
)

// Identity is a long-term Ed25519 key with a self-signed certificate.
type Identity struct {
	Cert tls.Certificate
	Pub  ed25519.PublicKey
}

// NewIdentity generates an Ed25519 key and a self-signed certificate valid for 20 years.
func NewIdentity(commonName string) (Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return Identity{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial.Add(serial, big.NewInt(1)),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(identityValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return Identity{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf},
		Pub:  pub,
	}, nil
}

// MarshalPEM encodes the certificate ("CERTIFICATE") and the PKCS #8 private key ("PRIVATE KEY").
func (id Identity) MarshalPEM() (certPEM, keyPEM []byte, err error) {
	if len(id.Cert.Certificate) != 1 {
		return nil, nil, errors.New("pair: identity must have exactly one certificate")
	}
	priv, ok := id.Cert.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, errors.New("pair: identity key is not Ed25519")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id.Cert.Certificate[0]})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// LoadIdentity parses the output of MarshalPEM. It requires exactly one
// Ed25519 certificate whose public key matches the private key.
func LoadIdentity(certPEM, keyPEM []byte) (Identity, error) {
	certDER, err := singlePEM(certPEM, "CERTIFICATE")
	if err != nil {
		return Identity{}, err
	}
	keyDER, err := singlePEM(keyPEM, "PRIVATE KEY")
	if err != nil {
		return Identity{}, err
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return Identity{}, fmt.Errorf("pair: parse certificate: %w", err)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return Identity{}, errors.New("pair: certificate key is not Ed25519")
	}
	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return Identity{}, fmt.Errorf("pair: parse private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return Identity{}, errors.New("pair: private key is not Ed25519")
	}
	if !pub.Equal(priv.Public()) {
		return Identity{}, errors.New("pair: private key does not match certificate")
	}
	return Identity{
		Cert: tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: priv, Leaf: leaf},
		Pub:  pub,
	}, nil
}

func singlePEM(data []byte, typ string) ([]byte, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != typ {
		return nil, fmt.Errorf("pair: expected a PEM %q block", typ)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("pair: unexpected data after PEM %q block", typ)
	}
	return block.Bytes, nil
}

// Fingerprint returns "sha256:" followed by the unpadded base64url encoding
// (RFC 4648 §5) of the SHA-256 of the key's DER SubjectPublicKeyInfo — 50
// characters in total. This is the pin format stored by both sides.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := spkiHash(pub)
	return fingerprintPrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

// Fingerprint is the fingerprint of the identity's public key.
func (id Identity) Fingerprint() string { return Fingerprint(id.Pub) }

// ValidFingerprint reports whether s is well-formed as returned by Fingerprint.
func ValidFingerprint(s string) bool {
	rest, ok := strings.CutPrefix(s, fingerprintPrefix)
	if !ok || len(rest) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(rest)
	return err == nil && len(b) == sha256.Size
}

func spkiHash(pub ed25519.PublicKey) [sha256.Size]byte {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		// Only fails for a wrong-length key, which callers never pass.
		panic("pair: marshal Ed25519 public key: " + err.Error())
	}
	return sha256.Sum256(der)
}

// certKey parses a DER certificate and returns its Ed25519 public key.
func certKey(der []byte) (ed25519.PublicKey, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return leafKey(c)
}

func leafKey(c *x509.Certificate) (ed25519.PublicKey, error) {
	pub, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("pair: certificate key is not Ed25519")
	}
	return pub, nil
}
