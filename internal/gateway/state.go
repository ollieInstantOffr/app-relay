package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/instantoffr/relay/internal/tunnel/pair"
)

// Files in <data dir>/gateway.
const (
	StateDirName = "gateway"
	CertFileName = "identity-cert.pem"
	KeyFileName  = "identity-key.pem"
	PairFileName = "pairing.json"
)

// identityName is the certificate common name of a gateway identity.
const identityName = "relay-gateway"

// Pairing is the persisted pairing state (pairing.json).
type Pairing struct {
	Schema   int       `json:"schema"`  // 1
	HomePin  string    `json:"homePin"` // fingerprint of the paired home's key
	PairedAt time.Time `json:"pairedAt"`
}

// stateDir is where the gateway keeps its identity and pairing state.
func stateDir(dataDir string) string { return filepath.Join(dataDir, StateDirName) }

// loadOrCreateIdentity loads the identity in dir, generating one on first use.
func loadOrCreateIdentity(dir string) (id pair.Identity, created bool, err error) {
	id, err = loadIdentity(dir)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return id, false, err
	}
	// A key without a certificate is left over from an interrupted first
	// start: no home can have pinned it yet, so it is replaced.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return id, false, err
	}
	if id, err = pair.NewIdentity(identityName); err != nil {
		return id, false, err
	}
	certPEM, keyPEM, err := id.MarshalPEM()
	if err != nil {
		return id, false, err
	}
	// The key goes first: a certificate on disk implies its key is there too.
	if err := writeFileAtomic(filepath.Join(dir, KeyFileName), keyPEM, 0o600); err != nil {
		return id, false, err
	}
	if err := writeFileAtomic(filepath.Join(dir, CertFileName), certPEM, 0o644); err != nil {
		return id, false, err
	}
	return id, true, nil
}

// loadIdentity loads an existing identity. It fails with an os.ErrNotExist
// error when there is no certificate, and refuses a certificate without its
// key rather than silently replacing the identity a home has pinned.
func loadIdentity(dir string) (pair.Identity, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFileName))
	if err != nil {
		return pair.Identity{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFileName))
	if errors.Is(err, os.ErrNotExist) {
		return pair.Identity{}, fmt.Errorf("gateway: %s exists but %s is missing (delete the certificate to create a new identity, then pair again)", CertFileName, KeyFileName)
	}
	if err != nil {
		return pair.Identity{}, err
	}
	id, err := pair.LoadIdentity(certPEM, keyPEM)
	if err != nil {
		return pair.Identity{}, fmt.Errorf("gateway: %s: %w", dir, err)
	}
	return id, nil
}

// loadPairing reads pairing.json; nil means not paired.
func loadPairing(dir string) (*Pairing, error) {
	path := filepath.Join(dir, PairFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pairing
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("gateway: %s: %w", path, err)
	}
	if p.Schema != 1 {
		return nil, fmt.Errorf("gateway: %s: unsupported schema %d (expected 1)", path, p.Schema)
	}
	if !pair.ValidFingerprint(p.HomePin) {
		return nil, fmt.Errorf("gateway: %s: invalid home pin", path)
	}
	return &p, nil
}

func savePairing(dir string, p *Pairing) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, PairFileName), append(data, '\n'), 0o600)
}

// deletePairing removes pairing.json; it reports whether there was one.
func deletePairing(dir string) (bool, error) {
	err := os.Remove(filepath.Join(dir, PairFileName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	syncDir(dir)
	return true, nil
}

// writeFileAtomic writes data to a temporary file in the same directory and
// renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
