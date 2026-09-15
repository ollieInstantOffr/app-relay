// Package render holds shared settings for the nginx and haproxy renderers.
package render

import "path/filepath"

// Env describes the runtime paths as seen from inside the engine containers.
// /data is mounted read-only into the nginx container.
type Env struct {
	DataDir       string          // /data
	CertDir       string          // /data/certs
	ACMEWebroot   string          // /data/acme (HTTP-01 challenge files)
	LogDir        string          // /var/log/relay
	RunDir        string          // /run/relay
	GeoIPCountry  string          // /data/geoip/GeoLite2-Country.mmdb ("" when absent)
	AdminUpstream string          // 127.0.0.1:8181
	Modules       map[string]bool // nginx modules reported by the agent: http_v3, stream, geoip2, auth_request
}

func DefaultEnv(dataDir, runDir, logDir string) Env {
	return Env{
		DataDir:       dataDir,
		CertDir:       filepath.Join(dataDir, "certs"),
		ACMEWebroot:   filepath.Join(dataDir, "acme"),
		LogDir:        logDir,
		RunDir:        runDir,
		AdminUpstream: "127.0.0.1:8181",
		Modules:       map[string]bool{},
	}
}

// CertPaths returns the PEM paths for a certificate id. The certs slice writes
// these files; renderers only reference them.
func (e Env) CertPaths(certID string) (fullchain, key string) {
	dir := filepath.Join(e.CertDir, certID)
	return filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
}

// DefaultCertPaths is the self-signed placeholder served for unknown SNI.
func (e Env) DefaultCertPaths() (fullchain, key string) { return e.CertPaths("_default") }
