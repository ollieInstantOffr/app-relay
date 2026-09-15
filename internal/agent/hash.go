package agent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
)

// BootstrapHash is the ConfigHash reported while the engine runs the built-in
// bootstrap configuration (nothing has been applied yet).
const BootstrapHash = "bootstrap"

// HashFiles returns a stable digest of a file set. It is used as the release
// id on the agent and stored with each config version by the relay app.
func HashFiles(files Files) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	var n [8]byte
	for _, k := range keys {
		binary.BigEndian.PutUint64(n[:], uint64(len(k)))
		h.Write(n[:])
		h.Write([]byte(k))
		binary.BigEndian.PutUint64(n[:], uint64(len(files[k])))
		h.Write(n[:])
		h.Write([]byte(files[k]))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func validHash(h string) bool {
	if len(h) < 8 || len(h) > 64 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HAProxy sockets inside RunDir. The agent's own socket is
// SocketPath(runDir, "haproxy") = <runDir>/haproxy.sock, so the HAProxy
// runtime API must use a different name.
const (
	HAProxyRuntimeSocketName = "haproxy-runtime.sock"
	HAProxyMasterSocketName  = "haproxy-master.sock"
)

// HAProxyRuntimeSocket is where the rendered haproxy.cfg should put
// `stats socket … level admin expose-fd listeners`.
func HAProxyRuntimeSocket(runDir string) string {
	return filepath.Join(runDir, HAProxyRuntimeSocketName)
}

// HAProxyMasterSocket is the master CLI socket (`-S`) used for reloads.
func HAProxyMasterSocket(runDir string) string { return filepath.Join(runDir, HAProxyMasterSocketName) }

// safeRelPath reports whether p is a clean relative path inside a release.
func safeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	c := filepath.ToSlash(filepath.Clean(p))
	if c != p || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return false
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." || seg == "" || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}
