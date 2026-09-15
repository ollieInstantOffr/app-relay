package edge

import (
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
	"sync"
	"sync/atomic"
	"time"
)

// certStore owns the parsed certificates, keyed by their cert+key file pair.
// Server selection (by SNI) lives in the routing table; the table's handshake
// configs point at certEntry values, whose current certificate is swapped in
// place when the files change on disk (ACME renewal) or an OCSP response is
// stapled. Neither needs a configuration reload.
type certStore struct {
	placeholder *tls.Certificate

	mu      sync.RWMutex
	entries map[string]*certEntry
}

type certEntry struct {
	key string
	ref CertRef

	cur atomic.Pointer[tls.Certificate] // served; carries the OCSP staple

	mu    sync.Mutex // guards the fields below
	sum   [32]byte
	stamp fileStamp
	base  *tls.Certificate // as parsed, without staple
	ocsp  ocspState
}

type fileStamp struct {
	certSize, keySize int64
	certMod, keyMod   time.Time
}

func certKey(ref CertRef) string { return ref.CertFile + "\x00" + ref.KeyFile }

func newCertStore() (*certStore, error) {
	placeholder, err := selfSigned("localhost")
	if err != nil {
		return nil, err
	}
	return &certStore{placeholder: placeholder, entries: map[string]*certEntry{}}, nil
}

// cert returns the certificate to present.
func (e *certEntry) cert() *tls.Certificate { return e.cur.Load() }

// prepare loads refs into a new entry set without touching the live one.
// Entries whose files are unchanged are reused, keeping OCSP staples.
func (s *certStore) prepare(refs []CertRef) (map[string]*certEntry, error) {
	s.mu.RLock()
	prev := s.entries
	s.mu.RUnlock()
	out := make(map[string]*certEntry, len(refs))
	for _, ref := range refs {
		key := certKey(ref)
		if e, ok := out[key]; ok {
			if ref.OCSPStapling && !e.ref.OCSPStapling {
				e.ref.OCSPStapling = true
			}
			continue
		}
		certPEM, keyPEM, stamp, err := readPair(ref)
		if err != nil {
			return nil, err
		}
		sum := pairSum(certPEM, keyPEM)
		if old := prev[key]; old != nil && old.ref == ref {
			old.mu.Lock()
			same := old.sum == sum
			old.mu.Unlock()
			if same {
				out[key] = old
				continue
			}
		}
		c, err := parsePair(ref, certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		e := &certEntry{key: key, ref: ref, sum: sum, stamp: stamp, base: c}
		e.cur.Store(c)
		out[key] = e
	}
	return out, nil
}

func (s *certStore) swap(entries map[string]*certEntry) {
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
}

func (s *certStore) snapshot() []*certEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*certEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

// refresh reloads certificates whose files changed. A pair that fails to
// load keeps serving the previous certificate. It returns the ids reloaded
// and the errors encountered.
func (s *certStore) refresh() (reloaded []string, errs []error) {
	for _, e := range s.snapshot() {
		changed, err := e.reload()
		if err != nil {
			errs = append(errs, err)
		} else if changed {
			reloaded = append(reloaded, e.ref.ID)
		}
	}
	return reloaded, errs
}

func (e *certEntry) reload() (bool, error) {
	stamp, err := statPair(e.ref)
	if err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if stamp == e.stamp {
		return false, nil
	}
	certPEM, keyPEM, stamp, err := readPair(e.ref)
	if err != nil {
		return false, err
	}
	// Remember the stamp even on failure so a broken file is reported once
	// per change, not on every poll.
	e.stamp = stamp
	sum := pairSum(certPEM, keyPEM)
	if sum == e.sum {
		return false, nil
	}
	c, err := parsePair(e.ref, certPEM, keyPEM)
	if err != nil {
		return false, err
	}
	e.sum, e.base, e.ocsp = sum, c, ocspState{}
	e.cur.Store(c)
	return true, nil
}

// changedOnDisk reports whether any certificate file changed since it was
// loaded (by content, not just timestamps).
func (s *certStore) changedOnDisk() bool {
	for _, e := range s.snapshot() {
		certPEM, keyPEM, _, err := readPair(e.ref)
		if err != nil {
			continue
		}
		e.mu.Lock()
		changed := pairSum(certPEM, keyPEM) != e.sum
		e.mu.Unlock()
		if changed {
			return true
		}
	}
	return false
}

func statPair(ref CertRef) (fileStamp, error) {
	ci, err := os.Stat(ref.CertFile)
	if err != nil {
		return fileStamp{}, fmt.Errorf("certificate %s: %w", ref.ID, err)
	}
	ki, err := os.Stat(ref.KeyFile)
	if err != nil {
		return fileStamp{}, fmt.Errorf("certificate %s key: %w", ref.ID, err)
	}
	return fileStamp{certSize: ci.Size(), keySize: ki.Size(), certMod: ci.ModTime(), keyMod: ki.ModTime()}, nil
}

func readPair(ref CertRef) (certPEM, keyPEM []byte, stamp fileStamp, err error) {
	if stamp, err = statPair(ref); err != nil {
		return nil, nil, stamp, err
	}
	if certPEM, err = os.ReadFile(ref.CertFile); err != nil {
		return nil, nil, stamp, fmt.Errorf("certificate %s: %w", ref.ID, err)
	}
	if keyPEM, err = os.ReadFile(ref.KeyFile); err != nil {
		return nil, nil, stamp, fmt.Errorf("certificate %s key: %w", ref.ID, err)
	}
	return certPEM, keyPEM, stamp, nil
}

func pairSum(certPEM, keyPEM []byte) [32]byte {
	h := sha256.New()
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(keyPEM)
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

func parsePair(ref CertRef, certPEM, keyPEM []byte) (*tls.Certificate, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate %s: %w", ref.ID, err)
	}
	if c.Leaf == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
			c.Leaf = leaf
		}
	}
	return &c, nil
}

func selfSigned(cn string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
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
