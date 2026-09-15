package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// releases manages config releases on disk:
//
//	<root>/releases/<hash>/…   one directory per applied file set
//	<root>/current              symlink → releases/<hash> (atomically swapped)
//	<root>/history.json         activated hashes, oldest first
//	<root>/staging/<rand>/…     scratch dirs for /v1/validate
type releases struct {
	root string
	keep int
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (r *releases) dir(hash string) string { return filepath.Join(r.root, "releases", hash) }
func (r *releases) currentLink() string    { return filepath.Join(r.root, "current") }

func (r *releases) init() error {
	for _, d := range []string{filepath.Join(r.root, "releases"), filepath.Join(r.root, "staging")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// Staging dirs never survive a restart.
	entries, _ := os.ReadDir(filepath.Join(r.root, "staging"))
	for _, e := range entries {
		os.RemoveAll(filepath.Join(r.root, "staging", e.Name()))
	}
	return nil
}

// current returns the active release hash ("" when none).
func (r *releases) current() string {
	target, err := os.Readlink(r.currentLink())
	if err != nil {
		return ""
	}
	h := filepath.Base(target)
	if _, err := os.Stat(r.dir(h)); err != nil {
		return ""
	}
	return h
}

func writeTree(dir string, files Files) error {
	for p, content := range files {
		if !safeRelPath(p) {
			return fmt.Errorf("invalid file path %q", p)
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// stage writes files to a scratch directory. The caller must call cleanup.
func (r *releases) stage(files Files) (string, func(), error) {
	dir := filepath.Join(r.root, "staging", randHex(8))
	cleanup := func() { os.RemoveAll(dir) }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", cleanup, err
	}
	if err := writeTree(dir, files); err != nil {
		cleanup()
		return "", cleanup, err
	}
	return dir, cleanup, nil
}

// write materialises a release. An existing release with the same hash is
// reused (same hash = same content).
func (r *releases) write(hash string, files Files) (dir string, created bool, err error) {
	dir = r.dir(hash)
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir, false, nil
	}
	tmp := filepath.Join(r.root, "releases", ".tmp-"+hash+"-"+randHex(4))
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", false, err
	}
	if err := writeTree(tmp, files); err != nil {
		os.RemoveAll(tmp)
		return "", false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return "", false, err
	}
	return dir, true, nil
}

func (r *releases) remove(hash string) {
	if hash == "" || hash == r.current() {
		return
	}
	for _, h := range r.history() {
		if h == hash {
			return
		}
	}
	os.RemoveAll(r.dir(hash))
}

// swap atomically points current at hash (symlink written to a temp name,
// then renamed over the old link).
func (r *releases) swap(hash string) error {
	if _, err := os.Stat(r.dir(hash)); err != nil {
		return fmt.Errorf("release %s: %w", hash, err)
	}
	tmp := filepath.Join(r.root, ".current-"+randHex(4))
	if err := os.Symlink(filepath.Join("releases", hash), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.currentLink()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// activate swaps to hash and records it in the history.
func (r *releases) activate(hash string) error {
	if err := r.swap(hash); err != nil {
		return err
	}
	h := r.history()
	if len(h) == 0 || h[len(h)-1] != hash {
		h = append(h, hash)
	}
	if len(h) > 50 {
		h = h[len(h)-50:]
	}
	return r.saveHistory(h)
}

// revert re-activates prev and drops the newest history entry (used when an
// apply fails after the swap).
func (r *releases) revert(failed, prev string) error {
	h := r.history()
	if len(h) > 0 && h[len(h)-1] == failed {
		h = h[:len(h)-1]
	}
	if prev == "" {
		os.Remove(r.currentLink())
		return r.saveHistory(h)
	}
	if err := r.swap(prev); err != nil {
		return err
	}
	if len(h) == 0 || h[len(h)-1] != prev {
		h = append(h, prev)
	}
	return r.saveHistory(h)
}

// previous returns the release activated before the current one.
func (r *releases) previous() string {
	h := r.history()
	cur := r.current()
	for i := len(h) - 1; i >= 0; i-- {
		if h[i] == cur {
			for j := i - 1; j >= 0; j-- {
				if h[j] != cur {
					if _, err := os.Stat(r.dir(h[j])); err == nil {
						return h[j]
					}
				}
			}
			break
		}
	}
	return ""
}

// rollback activates the previous release, removing the current one from
// the history. Returns the new current hash.
func (r *releases) rollback() (string, error) {
	prev := r.previous()
	if prev == "" {
		return "", errors.New("no previous release to roll back to")
	}
	cur := r.current()
	if err := r.swap(prev); err != nil {
		return "", err
	}
	h := r.history()
	for len(h) > 0 && h[len(h)-1] != prev {
		h = h[:len(h)-1]
	}
	if len(h) == 0 {
		h = []string{prev}
	}
	_ = cur
	return prev, r.saveHistory(h)
}

func (r *releases) history() []string {
	b, err := os.ReadFile(filepath.Join(r.root, "history.json"))
	if err != nil {
		return nil
	}
	var h []string
	json.Unmarshal(b, &h)
	return h
}

func (r *releases) saveHistory(h []string) error {
	b, _ := json.Marshal(h)
	tmp := filepath.Join(r.root, ".history-"+randHex(4))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.root, "history.json"))
}

// prune removes releases beyond the newest keep entries of the history.
func (r *releases) prune() {
	keep := map[string]bool{r.current(): true, BootstrapHash: true}
	h := r.history()
	for i := len(h) - 1; i >= 0 && len(keep) < r.keep+2; i-- {
		keep[h[i]] = true
	}
	entries, err := os.ReadDir(filepath.Join(r.root, "releases"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(r.root, "releases", e.Name()))
		}
	}
	// Trim history entries whose directories are gone.
	kept := h[:0]
	for _, x := range h {
		if _, err := os.Stat(r.dir(x)); err == nil {
			kept = append(kept, x)
		}
	}
	r.saveHistory(kept)
}

// lines counts lines across all files of a release.
func (r *releases) lines(hash string) int {
	if hash == "" {
		return 0
	}
	n := 0
	filepath.WalkDir(r.dir(hash), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			n += strings.Count(string(b), "\n")
			if len(b) > 0 && b[len(b)-1] != '\n' {
				n++
			}
		}
		return nil
	})
	return n
}

func (r *releases) readFile(hash, name string) string {
	b, _ := os.ReadFile(filepath.Join(r.dir(hash), name))
	return string(b)
}

// marker files (e.g. "stopped") live next to current.
func (r *releases) setMarker(name string, on bool) {
	p := filepath.Join(r.root, name)
	if on {
		os.WriteFile(p, []byte("1"), 0o644)
	} else {
		os.Remove(p)
	}
}

func (r *releases) marker(name string) bool {
	_, err := os.Stat(filepath.Join(r.root, name))
	return err == nil
}
