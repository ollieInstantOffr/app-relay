package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// FormatVersion is the archive layout version written to manifest.json.
const FormatVersion = 1

// excludedTables are high-volume or self-referential tables that are not
// part of a backup. Tables whose name starts with "<excluded>_" (FTS shadow
// tables, partitions) are skipped as well.
var excludedTables = []string{"access_log", "error_log", "metrics_minute", "lb_samples", "schema_migrations", "backups"}

func tableExcluded(name string) bool {
	for _, t := range excludedTables {
		if name == t || strings.HasPrefix(name, t+"_") {
			return true
		}
	}
	return false
}

// Manifest describes an archive.
type Manifest struct {
	Format      int            `json:"format"`
	Version     string         `json:"version"` // relay version that wrote it
	Created     time.Time      `json:"created"`
	Counts      map[string]int `json:"counts"`
	Tables      []string       `json:"tables"`
	PrivateKeys bool           `json:"privateKeys"`
}

// ErrWrongPassphrase is returned when an archive cannot be decrypted.
var ErrWrongPassphrase = errors.New("wrong passphrase, or not a Relay backup")

// isPrivateKeyFile reports whether a cert file holds key material.
func isPrivateKeyFile(name string) bool {
	base := strings.ToLower(path.Base(name))
	return base == "privkey.pem" || strings.HasSuffix(base, ".key") || strings.Contains(base, "private")
}

// counts computes the manifest counts from the live store.
func counts(ctx context.Context, st *store.Store) map[string]int {
	c := map[string]int{}
	for key, table := range map[string]string{
		"hosts":        model.KindHost,
		"redirects":    model.KindRedirect,
		"streams":      model.KindStream,
		"accessLists":  model.KindAccessList,
		"certificates": model.KindCertificate,
		"backends":     model.KindBackend,
		"frontends":    model.KindFrontend,
		"users":        "users",
	} {
		c[key] = st.CountRows(ctx, table)
	}
	return c
}

// writeArchive streams an encrypted archive of the store and cert files to w.
func writeArchive(ctx context.Context, w io.Writer, st *store.Store, certDir, passphrase, version string, includeKeys bool, logN int) (*Manifest, error) {
	rcpt, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	if logN > 0 {
		rcpt.SetWorkFactor(logN)
	}
	enc, err := age.Encrypt(w, rcpt)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC()

	tables, err := st.ListTables(ctx)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: FormatVersion, Version: version, Created: now, Counts: counts(ctx, st), Tables: []string{}, PrivateKeys: includeKeys}
	for _, t := range tables {
		if !tableExcluded(t) {
			m.Tables = append(m.Tables, t)
		}
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := writeTarFile(tw, "manifest.json", mb, now); err != nil {
		return nil, err
	}
	for _, t := range m.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d, err := st.DumpTable(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("dump %s: %w", t, err)
		}
		b, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		if err := writeTarFile(tw, "store/"+t+".json", b, now); err != nil {
			return nil, err
		}
	}
	if certDir != "" {
		err := filepath.WalkDir(certDir, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if de.IsDir() || !de.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(certDir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !includeKeys && isPrivateKeyFile(rel) {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			info, _ := de.Info()
			mod := now
			if info != nil {
				mod = info.ModTime()
			}
			return writeTarFile(tw, "certs/"+rel, data, mod)
		})
		if err != nil {
			return nil, fmt.Errorf("certs: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return m, nil
}

func writeTarFile(tw *tar.Writer, name string, data []byte, mod time.Time) error {
	mode := int64(0o644)
	if strings.HasPrefix(name, "certs/") && isPrivateKeyFile(name) {
		mode = 0o600
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: mod, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// Archive is a decrypted, parsed backup.
type Archive struct {
	Manifest Manifest
	Tables   map[string]*store.TableDump
	Certs    map[string][]byte // relative path (slash separated) → content
}

const maxEntrySize = 512 << 20

// readArchive decrypts and parses an archive fully into memory.
func readArchive(r io.Reader, passphrase string) (*Archive, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	dec, err := age.Decrypt(r, id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) || strings.Contains(err.Error(), "incorrect passphrase") || strings.Contains(err.Error(), "header") {
			return nil, ErrWrongPassphrase
		}
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		return nil, fmt.Errorf("not a Relay backup archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	a := &Archive{Tables: map[string]*store.TableDump{}, Certs: map[string][]byte{}}
	var haveManifest bool
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxEntrySize {
			return nil, fmt.Errorf("archive entry %s is too large", h.Name)
		}
		name := path.Clean(h.Name)
		if strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || name == ".." {
			return nil, fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxEntrySize))
		if err != nil {
			return nil, err
		}
		switch {
		case name == "manifest.json":
			if err := json.Unmarshal(data, &a.Manifest); err != nil {
				return nil, fmt.Errorf("invalid manifest: %w", err)
			}
			haveManifest = true
		case strings.HasPrefix(name, "store/") && strings.HasSuffix(name, ".json"):
			table := strings.TrimSuffix(strings.TrimPrefix(name, "store/"), ".json")
			if strings.Contains(table, "/") || tableExcluded(table) {
				continue
			}
			d, err := store.DecodeTableDump(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", table, err)
			}
			a.Tables[table] = d
		case strings.HasPrefix(name, "certs/"):
			a.Certs[strings.TrimPrefix(name, "certs/")] = data
		}
	}
	if !haveManifest {
		return nil, errors.New("not a Relay backup: manifest.json missing")
	}
	if a.Manifest.Format < 1 || a.Manifest.Format > FormatVersion {
		return nil, fmt.Errorf("unsupported backup format %d (this Relay understands up to %d)", a.Manifest.Format, FormatVersion)
	}
	for _, t := range a.Manifest.Tables {
		if _, ok := a.Tables[t]; !ok && !tableExcluded(t) {
			return nil, fmt.Errorf("archive is incomplete: table %s missing", t)
		}
	}
	if _, ok := a.Tables["settings"]; !ok {
		return nil, errors.New("archive is incomplete: settings missing")
	}
	return a, nil
}

// writeCerts writes restored certificate files below certDir. Existing
// private keys are kept when the archive has none.
func writeCerts(certDir string, files map[string][]byte) error {
	for rel, data := range files {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		dst := filepath.Join(certDir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if isPrivateKeyFile(rel) {
			mode = 0o600
		}
		tmp := dst + ".restore"
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	return nil
}
