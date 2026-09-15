package edge

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// CertPair names a certificate on disk and the server names it serves.
type CertPair struct {
	ID       string   `json:"id"`
	Names    []string `json:"names"`
	CertFile string   `json:"certFile"`
	KeyFile  string   `json:"keyFile"`
}

// certStore picks certificates by SNI (exact name, then one-level wildcard)
// and reloads files whose content changed. Renewed certificates are picked up
// without a reload of anything else.
type certStore struct {
	mu       sync.RWMutex
	exact    map[string]*tls.Certificate
	wildcard map[string]*tls.Certificate // "example.com" for *.example.com
	fallback *tls.Certificate
	loaded   map[string]loadedCert // by CertFile
}

type loadedCert struct {
	sum  [32]byte
	cert *tls.Certificate
}

func newCertStore() (*certStore, error) {
	placeholder, err := selfSigned("localhost")
	if err != nil {
		return nil, err
	}
	return &certStore{exact: map[string]*tls.Certificate{}, wildcard: map[string]*tls.Certificate{}, fallback: placeholder, loaded: map[string]loadedCert{}}, nil
}

// prepare loads pairs into a new lookup table without touching the live one.
// Unchanged files reuse the already parsed certificate.
func (s *certStore) prepare(pairs []CertPair) (exact, wildcard map[string]*tls.Certificate, loaded map[string]loadedCert, err error) {
	exact, wildcard, loaded = map[string]*tls.Certificate{}, map[string]*tls.Certificate{}, map[string]loadedCert{}
	s.mu.RLock()
	prev := s.loaded
	s.mu.RUnlock()
	for _, p := range pairs {
		certPEM, err := os.ReadFile(p.CertFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("certificate %s: %w", p.ID, err)
		}
		keyPEM, err := os.ReadFile(p.KeyFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("certificate %s key: %w", p.ID, err)
		}
		sum := sha256.Sum256(append(append([]byte{}, certPEM...), keyPEM...))
		lc, ok := prev[p.CertFile]
		if !ok || lc.sum != sum {
			c, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("certificate %s: %w", p.ID, err)
			}
			if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
				c.Leaf = leaf
			}
			lc = loadedCert{sum: sum, cert: &c}
		}
		loaded[p.CertFile] = lc
		for _, n := range p.Names {
			n = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(n), "."))
			if rest, ok := strings.CutPrefix(n, "*."); ok {
				wildcard[rest] = lc.cert
			} else if n != "" {
				exact[n] = lc.cert
			}
		}
	}
	return exact, wildcard, loaded, nil
}

func (s *certStore) swap(exact, wildcard map[string]*tls.Certificate, loaded map[string]loadedCert) {
	s.mu.Lock()
	s.exact, s.wildcard, s.loaded = exact, wildcard, loaded
	s.mu.Unlock()
}

// lookup returns the certificate for a server name, or nil.
func (s *certStore) lookup(name string) *tls.Certificate {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c := s.exact[name]; c != nil {
		return c
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		if c := s.wildcard[name[i+1:]]; c != nil {
			return c
		}
	}
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. Unknown names get the
// self-signed placeholder so real certificates are never revealed to scanners.
func (s *certStore) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := s.lookup(hello.ServerName); c != nil {
		return c, nil
	}
	return s.fallback, nil
}

// changedOnDisk reports whether any loaded certificate file changed since it
// was loaded (e.g. after an ACME renewal).
func (s *certStore) changedOnDisk(pairs []CertPair) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range pairs {
		certPEM, err1 := os.ReadFile(p.CertFile)
		keyPEM, err2 := os.ReadFile(p.KeyFile)
		if err1 != nil || err2 != nil {
			continue
		}
		sum := sha256.Sum256(append(append([]byte{}, certPEM...), keyPEM...))
		if lc, ok := s.loaded[p.CertFile]; !ok || !bytes.Equal(lc.sum[:], sum[:]) {
			return true
		}
	}
	return false
}

func selfSigned(cn string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
