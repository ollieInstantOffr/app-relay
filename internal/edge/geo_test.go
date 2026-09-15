package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/geoip/mmdbtest"
)

func TestGeoBlocking(t *testing.T) {
	cfg, dir := newConfig(t)
	db := filepath.Join(dir, "country.mmdb")
	if err := os.WriteFile(db, mmdbtest.Build(map[string]string{"2.0.0.0/16": "NO", "5.0.0.0/8": "SE"}), 0o644); err != nil {
		t.Fatal(err)
	}
	up := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.AllowCountries = []string{"NO"}
	open := proxyHost("o", []string{"open.test"}, up.ref())
	cfg.Hosts = []Host{h, open}
	cfg.GeoIPDatabase = db
	e := startEnv(t, cfg, dir)
	handler := &listenerHandler{srv: e.srv, role: &listenerRole{kind: "http", scheme: "http", port: 80, portStr: "80"}}

	for _, tc := range []struct {
		host, remote string
		want         int
	}{
		{"h.test", "2.0.1.1:4000", http.StatusOK},        // allowed country
		{"h.test", "5.1.1.1:4000", http.StatusForbidden}, // other country
		{"h.test", "8.8.8.8:4000", http.StatusForbidden}, // not in the database
		{"h.test", "192.168.1.10:4000", http.StatusOK},   // private
		{"h.test", "[::1]:4000", http.StatusOK},          // loopback
		{"open.test", "5.1.1.1:4000", http.StatusOK},     // host without geo-blocking
	} {
		req := httptest.NewRequest("GET", "http://"+tc.host+"/", nil)
		req.RemoteAddr = tc.remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s from %s: status %d, want %d", tc.host, tc.remote, rec.Code, tc.want)
		}
	}
}

func TestGeoConfigErrors(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.AllowCountries = []string{"no"}
	cfg.Hosts = []Host{h}

	_, err := compile(cfg, dir, compileEnv{})
	if err == nil || !strings.Contains(err.Error(), "needs geoipDatabase") || !strings.Contains(err.Error(), `"no"`) {
		t.Fatalf("err = %v", err)
	}
	cfg.GeoIPDatabase = filepath.Join(dir, "missing.mmdb")
	h.AllowCountries = []string{"NO"}
	cfg.Hosts = []Host{h}
	if _, err := compile(cfg, dir, compileEnv{}); err == nil || !strings.Contains(err.Error(), "geoipDatabase") {
		t.Fatalf("missing database: err = %v", err)
	}
}
