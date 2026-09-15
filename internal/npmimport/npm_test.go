package npmimport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// fixture creates a small NPM data folder with the real NPM schema subset.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE proxy_host (id INTEGER PRIMARY KEY, created_on TEXT, modified_on TEXT, owner_user_id INTEGER, is_deleted INTEGER DEFAULT 0,
			domain_names TEXT, forward_host TEXT, forward_port INTEGER, access_list_id INTEGER DEFAULT 0, certificate_id INTEGER DEFAULT 0,
			ssl_forced INTEGER DEFAULT 0, caching_enabled INTEGER DEFAULT 0, block_exploits INTEGER DEFAULT 0, advanced_config TEXT DEFAULT '',
			meta TEXT DEFAULT '{}', allow_websocket_upgrade INTEGER DEFAULT 0, http2_support INTEGER DEFAULT 0, forward_scheme TEXT DEFAULT 'http',
			enabled INTEGER DEFAULT 1, locations TEXT, hsts_enabled INTEGER DEFAULT 0, hsts_subdomains INTEGER DEFAULT 0)`,
		`CREATE TABLE redirection_host (id INTEGER PRIMARY KEY, is_deleted INTEGER DEFAULT 0, domain_names TEXT, forward_domain_name TEXT,
			preserve_path INTEGER, certificate_id INTEGER, ssl_forced INTEGER, block_exploits INTEGER, advanced_config TEXT, meta TEXT,
			http2_support INTEGER, enabled INTEGER, hsts_enabled INTEGER, hsts_subdomains INTEGER, forward_http_code INTEGER, forward_scheme TEXT)`,
		`CREATE TABLE stream (id INTEGER PRIMARY KEY, is_deleted INTEGER DEFAULT 0, incoming_port INTEGER, forwarding_host TEXT, forwarding_port INTEGER,
			tcp_forwarding INTEGER, udp_forwarding INTEGER, meta TEXT, enabled INTEGER, certificate_id INTEGER DEFAULT 0)`,
		`CREATE TABLE access_list (id INTEGER PRIMARY KEY, is_deleted INTEGER DEFAULT 0, name TEXT, meta TEXT, satisfy_any INTEGER, pass_auth INTEGER)`,
		`CREATE TABLE access_list_client (id INTEGER PRIMARY KEY, access_list_id INTEGER, address TEXT, directive TEXT, meta TEXT)`,
		`CREATE TABLE access_list_auth (id INTEGER PRIMARY KEY, access_list_id INTEGER, username TEXT, password TEXT, meta TEXT)`,
		`CREATE TABLE certificate (id INTEGER PRIMARY KEY, is_deleted INTEGER DEFAULT 0, provider TEXT, nice_name TEXT, domain_names TEXT, expires_on TEXT, meta TEXT)`,

		`INSERT INTO access_list VALUES (1, 0, 'lan-only', '{}', 1, 0)`,
		`INSERT INTO access_list_client VALUES (1, 1, '192.168.1.0/24', 'allow', '{}')`,
		`INSERT INTO access_list_auth VALUES (1, 1, 'jonas', 'plain-secret', '{}')`,
		`INSERT INTO access_list VALUES (2, 1, 'deleted-list', '{}', 0, 0)`,

		`INSERT INTO certificate VALUES (1, 0, 'letsencrypt', '', '["grafana.example.com"]', '', '{"letsencrypt_email":"a@b.c"}')`,
		`INSERT INTO certificate VALUES (2, 0, 'letsencrypt', 'wild', '["*.example.com","example.com"]', '', '{"dns_challenge":true,"dns_provider":"cloudflare"}')`,

		`INSERT INTO proxy_host (id, domain_names, forward_host, forward_port, access_list_id, certificate_id, ssl_forced, block_exploits,
			allow_websocket_upgrade, http2_support, forward_scheme, enabled, locations, hsts_enabled, advanced_config)
			VALUES (1, '["Grafana.example.com"]', '10.0.0.21', 3000, 1, 1, 1, 1, 1, 1, 'http', 1,
			'[{"path":"/api","advanced_config":"","forward_scheme":"https","forward_host":"10.0.0.22/v1","forward_port":8443}]', 1, '')`,
		`INSERT INTO proxy_host (id, domain_names, forward_host, forward_port, forward_scheme, enabled, locations, advanced_config)
			VALUES (2, '["taken.example.com"]', '10.0.0.30', 80, 'http', 0, '[]', 'client_max_body_size 0;')`,
		`INSERT INTO proxy_host (id, is_deleted, domain_names, forward_host, forward_port) VALUES (3, 1, '["gone.example.com"]', '10.0.0.40', 80)`,

		`INSERT INTO redirection_host VALUES (1, 0, '["old.example.com"]', 'new.example.com', 1, 0, 1, 0, '', '{}', 0, 1, 0, 0, 301, 'auto')`,
		`INSERT INTO stream VALUES (1, 0, 25565, '10.0.0.50', 25565, 1, 1, '{}', 1, 0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// Certificate files for cert 1 only.
	certDir := filepath.Join(dir, "letsencrypt", "live", "npm-1")
	os.MkdirAll(certDir, 0o755)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "grafana.example.com"}, Issuer: pkix.Name{CommonName: "R11"},
		DNSNames: []string{"grafana.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(60 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(certDir, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(certDir, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return dir
}

func TestNPMImport(t *testing.T) {
	npmDir := fixture(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	taken := &model.ProxyHost{Domains: []string{"taken.example.com"}, Upstream: model.Upstream{Scheme: "http", Host: "10.9.9.9", Port: 80}, Source: model.SourceManual}
	st.Hosts().Create(ctx, taken)
	cf := &model.DNSProvider{Name: "Cloudflare", Type: "cloudflare", Credentials: map[string]string{}, Zones: []string{"example.com"}}
	st.DNSProviders().Create(ctx, cf)

	s := New(app)
	pv, err := s.PreviewPath(ctx, npmDir)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Counts["hosts"].Total != 2 || pv.Counts["hosts"].Conflicts != 1 || pv.Counts["accessLists"].Total != 1 ||
		pv.Counts["certificates"].Total != 2 || pv.Counts["redirects"].Total != 1 || pv.Counts["streams"].Total != 1 {
		t.Fatalf("counts = %+v", pv.Counts)
	}
	var hostWarn, certWarn bool
	for _, it := range pv.Items {
		if it.Kind == "hosts" && it.NPMID == 2 && strings.Contains(strings.Join(it.Warnings, " "), "custom nginx") && strings.Contains(it.Conflict, "taken.example.com") {
			hostWarn = true
		}
		if it.Kind == "certificates" && it.NPMID == 2 && strings.Contains(strings.Join(it.Warnings, " "), "not found") {
			certWarn = true
		}
	}
	if !hostWarn || !certWarn {
		t.Fatalf("items = %+v", pv.Items)
	}
	// Preview writes nothing.
	if hosts, _ := st.Hosts().List(ctx); len(hosts) != 1 {
		t.Fatal("preview changed the store")
	}

	req := httptest.NewRequest("POST", "/api/import/npm/commit", nil).WithContext(core.WithActor(ctx, core.Actor{Type: core.ActorUser, Name: "jonas", Role: core.RoleAdmin}))
	res, err := s.Commit(req, pv.Token, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created["hosts"] != 1 || res.Skipped["hosts"] != 1 || res.Created["certificates"] != 2 || res.Created["accessLists"] != 1 ||
		res.Created["redirects"] != 1 || res.Created["streams"] != 1 || len(res.Errors) != 0 {
		t.Fatalf("commit = %+v", res)
	}
	if _, err := s.Commit(req, pv.Token, false); err == nil {
		t.Fatal("token reused")
	}

	hosts, _ := st.Hosts().List(ctx)
	var g model.ProxyHost
	for _, h := range hosts {
		if h.Domains[0] == "grafana.example.com" {
			g = h
		}
	}
	if g.ID == "" || g.Upstream.Host != "10.0.0.21" || g.Upstream.Port != 3000 || !g.ForceHTTPS || !g.Websockets || !g.HTTP2 || g.HSTS != "on" ||
		g.Source != model.SourceImport || len(g.Locations) != 1 || g.Locations[0].Upstream.Path != "/v1" || g.Locations[0].Upstream.Scheme != "https" {
		t.Fatalf("grafana host = %+v", g)
	}
	cert, err := st.Certificates().Get(ctx, g.CertificateID)
	if err != nil || cert.Status != model.CertStatusValid || cert.NotAfter == nil || !strings.HasPrefix(cert.KeyType, "ECDSA") {
		t.Fatalf("cert = %+v %v", cert, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "certs", cert.ID, "privkey.pem")); err != nil {
		t.Fatal("cert files not written")
	}
	lists, _ := st.AccessLists().List(ctx)
	if len(lists) != 1 || g.AccessListID != lists[0].ID || !lists[0].SatisfyAny || len(lists[0].Rules) != 2 || lists[0].Rules[1].CIDR != "all" {
		t.Fatalf("access lists = %+v", lists)
	}
	u := lists[0].BasicAuth.Users[0]
	if u.Password != "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("plain-secret")) != nil {
		t.Fatalf("basic auth user = %+v", u)
	}
	redirects, _ := st.Redirects().List(ctx)
	if len(redirects) != 1 || redirects[0].To != "https://new.example.com" || redirects[0].Code != 301 || !redirects[0].KeepPath || redirects[0].ForceHTTPS {
		t.Fatalf("redirects = %+v", redirects)
	}
	streams, _ := st.Streams().List(ctx)
	if len(streams) != 1 || streams[0].Protocol != "both" || streams[0].ListenPorts != "25565" || streams[0].ForwardPorts != "" {
		t.Fatalf("streams = %+v", streams)
	}

	// Second import with overwrite updates the conflicting host in place.
	pv2, err := s.PreviewPath(ctx, filepath.Join(npmDir, "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := s.Commit(req, pv2.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Updated["hosts"] != 2 || res2.Created["hosts"] != 0 {
		t.Fatalf("overwrite commit = %+v", res2)
	}
	updated, _ := st.Hosts().Get(ctx, taken.ID)
	if updated.Upstream.Host != "10.0.0.30" || updated.Enabled {
		t.Fatalf("overwritten host = %+v", updated)
	}

	// Uploading just the DB works too.
	f, _ := os.Open(filepath.Join(npmDir, "database.sqlite"))
	defer f.Close()
	pv3, err := s.PreviewUpload(ctx, f, "database.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if len(pv3.Warnings) == 0 || pv3.Counts["hosts"].Conflicts != 2 {
		t.Fatalf("upload preview = %+v", pv3)
	}
	if _, err := s.PreviewUpload(ctx, strings.NewReader("not sqlite"), "x"); err == nil {
		t.Fatal("garbage accepted")
	}
}
