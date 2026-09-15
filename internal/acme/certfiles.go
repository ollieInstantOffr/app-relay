package acme

// PEM parsing, certificate inspection, key matching and atomic file writes.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

// CertInfo is what Relay shows about an issued or uploaded certificate.
type CertInfo struct {
	Domains     []string
	NotBefore   time.Time
	NotAfter    time.Time
	Issuer      string
	KeyType     string
	Fingerprint string
	Chain       []string
	Leaf        *x509.Certificate
	Certs       []*x509.Certificate // ordered leaf → root
}

// parseCertificates decodes every CERTIFICATE block in data.
func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := bytes.TrimSpace(data)
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			if strings.Contains(block.Type, "PRIVATE KEY") {
				return nil, errors.New("The certificate field contains a private key — paste it into the private key field instead")
			}
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("Could not parse certificate: %v", err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("No PEM certificate found (expected -----BEGIN CERTIFICATE-----)")
	}
	return certs, nil
}

// orderChain returns certs ordered leaf → root. The leaf is the certificate
// that did not sign any other certificate in the bundle.
func orderChain(certs []*x509.Certificate) []*x509.Certificate {
	if len(certs) < 2 {
		return certs
	}
	signs := func(parent, child *x509.Certificate) bool {
		return bytes.Equal(parent.RawSubject, child.RawIssuer) && child.CheckSignatureFrom(parent) == nil
	}
	leafIdx := 0
	for i, c := range certs {
		isIssuer := false
		for j, o := range certs {
			if i != j && signs(c, o) {
				isIssuer = true
				break
			}
		}
		if !isIssuer {
			leafIdx = i
			break
		}
	}
	ordered := []*x509.Certificate{certs[leafIdx]}
	used := map[int]bool{leafIdx: true}
	for len(ordered) < len(certs) {
		cur := ordered[len(ordered)-1]
		next := -1
		for i, c := range certs {
			if !used[i] && !bytes.Equal(cur.RawSubject, cur.RawIssuer) && signs(c, cur) {
				next = i
				break
			}
		}
		if next < 0 {
			break
		}
		used[next] = true
		ordered = append(ordered, certs[next])
	}
	for i, c := range certs { // keep unrelated extras at the end
		if !used[i] {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func keyTypeName(pub any) string {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		return "ECDSA " + k.Curve.Params().Name
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	case ed25519.PublicKey:
		return "Ed25519"
	}
	return "unknown"
}

func certName(c *x509.Certificate) string {
	if c.Subject.CommonName != "" {
		return c.Subject.CommonName
	}
	if len(c.Subject.Organization) > 0 {
		return c.Subject.Organization[0]
	}
	if len(c.DNSNames) > 0 {
		return c.DNSNames[0]
	}
	return c.Subject.String()
}

// InspectPEM parses a fullchain PEM and extracts the display metadata.
func InspectPEM(fullchain []byte) (*CertInfo, error) {
	certs, err := parseCertificates(fullchain)
	if err != nil {
		return nil, err
	}
	certs = orderChain(certs)
	leaf := certs[0]
	info := &CertInfo{
		NotBefore:   leaf.NotBefore.UTC(),
		NotAfter:    leaf.NotAfter.UTC(),
		Issuer:      leaf.Issuer.CommonName,
		KeyType:     keyTypeName(leaf.PublicKey),
		Fingerprint: fingerprint(leaf.Raw),
		Leaf:        leaf,
		Certs:       certs,
	}
	if info.Issuer == "" && len(leaf.Issuer.Organization) > 0 {
		info.Issuer = leaf.Issuer.Organization[0]
	}
	info.Domains = append(info.Domains, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		info.Domains = append(info.Domains, ip.String())
	}
	if len(info.Domains) == 0 && leaf.Subject.CommonName != "" {
		info.Domains = []string{leaf.Subject.CommonName}
	}
	for _, c := range certs {
		info.Chain = append(info.Chain, certName(c))
	}
	last := certs[len(certs)-1]
	if !bytes.Equal(last.RawSubject, last.RawIssuer) {
		// the root is usually not part of the bundle: name it from the issuer
		name := last.Issuer.CommonName
		if name == "" && len(last.Issuer.Organization) > 0 {
			name = last.Issuer.Organization[0]
		}
		if name != "" {
			info.Chain = append(info.Chain, name)
		}
	}
	return info, nil
}

// applyInfo copies inspected metadata onto a certificate entity.
func applyInfo(c *model.Certificate, info *CertInfo) {
	nb, na := info.NotBefore, info.NotAfter
	c.NotBefore = &nb
	c.NotAfter = &na
	c.Issuer = info.Issuer
	c.KeyType = info.KeyType
	c.Fingerprint = info.Fingerprint
	c.Chain = info.Chain
	if len(info.Domains) > 0 {
		c.Domains = info.Domains
	}
}

// parsePrivateKey decodes a PKCS#1, PKCS#8 or SEC 1 PEM private key.
func parsePrivateKey(data []byte) (crypto.Signer, error) {
	rest := bytes.TrimSpace(data)
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			continue
		}
		if x509.IsEncryptedPEMBlock(block) || block.Type == "ENCRYPTED PRIVATE KEY" { //nolint:staticcheck // detection only
			return nil, errors.New("The private key is encrypted — remove the passphrase first (openssl pkey -in key.pem -out plain.pem)")
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("Could not parse RSA private key: %v", err)
			}
			return k, nil
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("Could not parse EC private key: %v", err)
			}
			return k, nil
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("Could not parse private key: %v", err)
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("Unsupported private key type")
			}
			return s, nil
		}
	}
	return nil, errors.New("No PEM private key found (expected -----BEGIN PRIVATE KEY-----)")
}

type publicKeyEqualer interface{ Equal(crypto.PublicKey) bool }

func keyMatches(cert *x509.Certificate, key crypto.Signer) bool {
	pub, ok := key.Public().(publicKeyEqualer)
	return ok && pub.Equal(cert.PublicKey)
}

func encodePrivateKey(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func encodeCerts(certs []*x509.Certificate) []byte {
	var buf bytes.Buffer
	for _, c := range certs {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return buf.Bytes()
}

// UploadedCert is a validated custom certificate upload.
type UploadedCert struct {
	Info      *CertInfo
	Fullchain []byte
	Key       []byte
}

// ValidateUpload checks PEM encoding, that the key matches the leaf and that
// the certificate is not expired. The returned PEMs are normalised.
func ValidateUpload(certPEM, keyPEM []byte, now time.Time) (*UploadedCert, error) {
	info, err := InspectPEM(certPEM)
	if err != nil {
		return nil, (model.Errs{"certificate": err.Error()}).Err()
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, (model.Errs{"privateKey": err.Error()}).Err()
	}
	if !keyMatches(info.Leaf, key) {
		return nil, (model.Errs{"privateKey": "The private key does not match the certificate"}).Err()
	}
	if now.After(info.NotAfter) {
		return nil, (model.Errs{"certificate": "The certificate expired on " + info.NotAfter.Format("2006-01-02")}).Err()
	}
	keyOut, err := encodePrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &UploadedCert{Info: info, Fullchain: encodeCerts(info.Certs), Key: keyOut}, nil
}

// writeCertFiles atomically replaces fullchain.pem (0644) and privkey.pem (0640).
func writeCertFiles(fullchainPath, keyPath string, fullchain, key []byte) error {
	dir := filepath.Dir(fullchainPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpChain, err := writeTemp(dir, fullchain, 0o644)
	if err != nil {
		return err
	}
	tmpKey, err := writeTemp(dir, key, 0o640)
	if err != nil {
		os.Remove(tmpChain)
		return err
	}
	if err := os.Rename(tmpKey, keyPath); err != nil {
		os.Remove(tmpChain)
		os.Remove(tmpKey)
		return err
	}
	if err := os.Rename(tmpChain, fullchainPath); err != nil {
		os.Remove(tmpChain)
		return err
	}
	return nil
}

func writeTemp(dir string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// selfSigned creates an ECDSA P-256 self-signed certificate.
func selfSigned(cn string, validity time.Duration, now time.Time) (chainPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Relay"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodePrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}
