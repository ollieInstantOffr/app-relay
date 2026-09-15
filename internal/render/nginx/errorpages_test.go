package nginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

func errorPagesSnapshot() *model.Snapshot {
	snap := &model.Snapshot{
		General:     model.GeneralSettings{HTTPPort: 80, HTTPSPort: 443},
		DefaultHost: model.DefaultHostSettings{Action: "close"},
		ErrorPages:  model.DefaultErrorPages(),
	}
	snap.ErrorPages.Enabled = true
	snap.AccessLists = []model.AccessList{{Meta: model.Meta{ID: "office"}, Name: "office",
		Rules: []model.IPRule{{Action: "allow", CIDR: "203.0.113.0/24"}, {Action: "deny", CIDR: "all"}}}}
	up := model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 3000}
	snap.Hosts = []model.ProxyHost{
		{Meta: model.Meta{ID: "app"}, Domains: []string{"app.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit", CacheAssets: true,
			Maintenance: model.Maintenance{Enabled: true, Message: "Back at 18:00 <soon>", BypassAccessListID: "office"}},
		{Meta: model.Meta{ID: "wiki"}, Domains: []string{"wiki.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit",
			ForwardAuth: model.ForwardAuth{Enabled: true, Provider: model.ForwardAuthRelay, PassRemoteUser: true}},
		{Meta: model.Meta{ID: "shop"}, Domains: []string{"shop.example.com"}, Enabled: true, Upstream: up, HSTS: "inherit",
			Maintenance: model.Maintenance{Enabled: true}},
	}
	return snap
}

func hostConf(t *testing.T, files agent.Files, domain string) string {
	t.Helper()
	for name, body := range files {
		if strings.HasPrefix(name, "conf.d/hosts/") && strings.Contains(body, domain) {
			return body
		}
	}
	t.Fatalf("no host file for %s", domain)
	return ""
}

func TestRenderErrorPagesMaintenanceAndRelayLogin(t *testing.T) {
	env := testEnv()
	env.ConfDir = "/etc/relay/nginx/current"
	env.Modules["auth_request"] = true
	files, err := Render(render.PrepareSnapshot(errorPagesSnapshot(), env), env)
	if err != nil {
		t.Fatal(err)
	}
	if p := files["errors/502.html"]; !strings.Contains(p, "Service unavailable") {
		t.Fatalf("errors/502.html = %q", p)
	}
	if p := files["errors/maintenance-app.html"]; !strings.Contains(p, "Back at 18:00 &lt;soon&gt;") {
		t.Fatalf("maintenance page = %q", p)
	}
	if p := files["errors/maintenance-shop.html"]; !strings.Contains(p, "Down for maintenance") {
		t.Fatalf("default maintenance page = %q", p)
	}

	app := hostConf(t, files, "app.example.com")
	for _, want := range []string{
		"error_page 502 /.relay-errors/502.html;",
		"error_page 503 /.relay-errors/maintenance-app.html;",
		"/etc/relay/nginx/current/errors/;",
		"if ($relay_maint_app)",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app host lacks %q", want)
		}
	}
	if strings.Contains(app, "error_page 503 /.relay-errors/503.html;") {
		t.Error("a host in maintenance must use the maintenance page for 503")
	}
	// The static asset location must not bypass maintenance.
	if strings.Count(app, "if ($relay_maint_app)") < 2 {
		t.Errorf("maintenance check missing in the asset cache location:\n%s", app)
	}

	global := files["conf.d/00-global.conf"]
	for _, want := range []string{"geo $relay_maint_app", "203.0.113.0/24 0;", "default 1;"} {
		if !strings.Contains(global, want) {
			t.Errorf("global conf lacks %q", want)
		}
	}

	shop := hostConf(t, files, "shop.example.com")
	if !strings.Contains(shop, "return 503;") || strings.Contains(shop, "relay_maint_shop") {
		t.Errorf("maintenance without bypass should return 503 directly:\n%s", shop)
	}

	wiki := hostConf(t, files, "wiki.example.com")
	for _, want := range []string{".relay/verify?host=wiki", "/.relay/login?host=wiki&rd=", "error_page 401 = @relay_signin;", "proxy_pass http://127.0.0.1:8181"} {
		if !strings.Contains(wiki, want) {
			t.Errorf("wiki host lacks %q:\n%s", want, wiki)
		}
	}
	// The forward-auth location redefines the error pages (no inheritance).
	signin := strings.Index(wiki, "error_page 401 = @relay_signin;")
	if signin < 0 || !strings.Contains(wiki[signin:], "error_page 502 /.relay-errors/502.html;") {
		t.Error("forward-auth locations must repeat the error pages")
	}
}

func TestRenderNoErrorPagesByDefault(t *testing.T) {
	snap := errorPagesSnapshot()
	snap.ErrorPages.Enabled = false
	snap.Hosts = snap.Hosts[1:2]
	snap.Hosts[0].ForwardAuth = model.ForwardAuth{}
	env := testEnv()
	files, err := Render(snap, env)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if strings.HasPrefix(name, "errors/") || strings.Contains(body, "error_page") || strings.Contains(body, "relay-errors") {
			t.Fatalf("%s has error page config without error pages enabled", name)
		}
	}
}

// TestDumpErrorPagesForNginxT writes the error page / maintenance / Relay
// login config to $RELAY_NGINX_RENDER_DIR for `nginx -t` in the engine image.
func TestDumpErrorPagesForNginxT(t *testing.T) {
	dir := os.Getenv("RELAY_NGINX_RENDER_DIR")
	if dir == "" {
		t.Skip("RELAY_NGINX_RENDER_DIR not set")
	}
	env := testEnv()
	env.GeoCountryFile = "" // the include only exists on a Relay host
	env.Modules["auth_request"] = true
	files, err := Render(render.PrepareSnapshot(errorPagesSnapshot(), env), env)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
}
