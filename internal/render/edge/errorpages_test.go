package edge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

func TestRenderErrorPagesMaintenanceAndRelayLogin(t *testing.T) {
	up := model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 3000}
	snap := &model.Snapshot{
		General:     model.GeneralSettings{HTTPPort: 80, HTTPSPort: 443},
		DefaultHost: model.DefaultHostSettings{Action: "close"},
		ErrorPages:  model.DefaultErrorPages(),
		AccessLists: []model.AccessList{{Meta: model.Meta{ID: "office"}, Name: "office",
			Rules: []model.IPRule{{Action: "allow", CIDR: "203.0.113.0/24"}, {Action: "deny", CIDR: "all"}}}},
		Hosts: []model.ProxyHost{
			{Meta: model.Meta{ID: "app"}, Domains: []string{"app.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit",
				Maintenance: model.Maintenance{Enabled: true, Message: "Back at 18:00 <soon>", BypassAccessListID: "office"}},
			{Meta: model.Meta{ID: "wiki"}, Domains: []string{"wiki.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit",
				ForwardAuth: model.ForwardAuth{Enabled: true, Provider: model.ForwardAuthRelay}},
		},
	}
	snap.ErrorPages.Enabled = true
	env := testEnv()
	cfg, _ := mustRender(t, render.PrepareSnapshot(snap, env), env)

	if len(cfg.ErrorPages) != len(render.ErrorPageCodes) || !strings.Contains(cfg.ErrorPages["502"], "Service unavailable") {
		t.Fatalf("error pages = %v", cfg.ErrorPages)
	}
	app := findHost(t, cfg, "app")
	if app.Maintenance == nil || !strings.Contains(app.Maintenance.Page, "Back at 18:00 &lt;soon&gt;") {
		t.Fatalf("maintenance = %+v", app.Maintenance)
	}
	equal(t, "bypass", app.Maintenance.Bypass, []edgecfg.ExemptRule{{CIDR: "203.0.113.0/24", Exempt: true}})
	equal(t, "bypass default", app.Maintenance.BypassDefault, false)

	wiki := findHost(t, cfg, "wiki")
	if wiki.ForwardAuth == nil || wiki.ForwardAuth.VerifyURL != "http://127.0.0.1:8181/.relay/verify?host=wiki" || wiki.ForwardAuth.SignInURL != "/.relay/login?host=wiki" {
		t.Fatalf("forward auth = %+v", wiki.ForwardAuth)
	}
	var portal *edgecfg.Location
	for i := range wiki.Locations {
		if wiki.Locations[i].Path == render.PortalPrefix {
			portal = &wiki.Locations[i]
		}
	}
	if portal == nil || portal.ForwardAuth || !portal.SkipMaintenance || portal.Upstream.Port != 8181 {
		t.Fatalf("portal location = %+v", portal)
	}

	snap.ErrorPages.Enabled = false
	cfg, _ = mustRender(t, snap, env)
	if cfg.ErrorPages != nil {
		t.Fatal("error pages must be omitted when disabled")
	}
}

// The rendered error pages, maintenance and Relay login config must pass the
// data plane's own check (`relay edge check`).
func TestErrorPagesConfigPassesEdgeCheck(t *testing.T) {
	up := model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 3000}
	snap := &model.Snapshot{
		General:     model.GeneralSettings{HTTPPort: 80, HTTPSPort: 443},
		DefaultHost: model.DefaultHostSettings{Action: "close"},
		ErrorPages:  model.DefaultErrorPages(),
		AccessLists: []model.AccessList{{Meta: model.Meta{ID: "office"}, Name: "office", Rules: []model.IPRule{{Action: "allow", CIDR: "203.0.113.0/24"}}}},
		Hosts: []model.ProxyHost{
			{Meta: model.Meta{ID: "app"}, Domains: []string{"app.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit",
				Maintenance: model.Maintenance{Enabled: true, BypassAccessListID: "office"}},
			{Meta: model.Meta{ID: "wiki"}, Domains: []string{"wiki.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit",
				ForwardAuth: model.ForwardAuth{Enabled: true, Provider: model.ForwardAuthRelay, PassRemoteUser: true}},
		},
	}
	snap.ErrorPages.Enabled = true
	dir := t.TempDir()
	env := testEnv()
	env.DataDir, env.CertDir, env.LogDir, env.ACMEWebroot = dir, filepath.Join(dir, "certs"), dir, filepath.Join(dir, "acme")
	files, err := Render(render.PrepareSnapshot(snap, env), env)
	if err != nil {
		t.Fatal(err)
	}
	writeTestCert(t, filepath.Join(env.CertDir, "_default"))
	rel := filepath.Join(dir, "release")
	for p, c := range files {
		full := filepath.Join(rel, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := edgecfg.Check(rel); err != nil {
		t.Fatalf("edge check: %v", err)
	}
}

// writeTestCert writes a self-signed fullchain.pem / privkey.pem into dir.
func writeTestCert(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0o600)
}
