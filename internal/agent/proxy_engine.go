package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// ProxyEngineFileName is the file in the shared run directory where the relay
// app records the selected proxy engine ("nginx" or "edge"). Agents of proxy
// engines read it on a fresh config root to decide whether to start.
const ProxyEngineFileName = "proxy-engine"

// ProxyEngineFile returns the selection file path inside runDir.
func ProxyEngineFile(runDir string) string { return filepath.Join(runDir, ProxyEngineFileName) }

// ReadProxyEngine returns the selected proxy engine (nginx when the file is
// missing or unreadable).
func ReadProxyEngine(runDir string) string {
	b, err := os.ReadFile(ProxyEngineFile(runDir))
	if err != nil {
		return EngineNginx
	}
	return NormalizeProxyEngine(strings.TrimSpace(string(b)))
}

// WriteProxyEngine records the selected proxy engine atomically. It is a
// no-op when the file already holds engine.
func WriteProxyEngine(runDir, engine string) error {
	engine = NormalizeProxyEngine(engine)
	p := ProxyEngineFile(runDir)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == engine {
		return nil
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp-" + randHex(4)
	if err := os.WriteFile(tmp, []byte(engine+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
