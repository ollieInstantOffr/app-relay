package nginx

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

func boolp(b bool) *bool { return &b }

func timep(t time.Time) *time.Time { return &t }

func testEnv() render.Env {
	env := render.DefaultEnv("/data", "/run/relay", "/var/log/relay")
	env.GeoIPCountry = "/data/geoip/dbip-country-lite.mmdb"
	env.GeoCountryFile = "/data/geoip/nginx/countries-test.conf"
	env.Modules = map[string]bool{"stream": true, "http_v3": true, "auth_request": true, "ipv6": true}
	return env
}

// richSnapshot exercises every renderer feature.
func richSnapshot() *model.Snapshot {
	snap := &model.Snapshot{
		General:     store.DefaultGeneral(),
		TLS:         store.DefaultTLS(),
		DefaultHost: model.DefaultHostSettings{Action: "404"},
		HAProxy:     store.DefaultHAProxy(),
	}
	snap.General.HTTP3 = true
	snap.TLS.HSTS.Enabled = true
	snap.Blocklist.Entries = []model.BlockEntry{{CIDR: "203.0.113.88"}, {CIDR: "198.51.100.0/24"}, {CIDR: "not-an-ip"}}
	snap.Certificates = []model.Certificate{
		{Meta: model.Meta{ID: "certwild"}, Name: "*.home.lan", Domains: []string{"*.home.lan"}, Provider: model.CertLetsEncrypt, Status: model.CertStatusFailed, NotAfter: timep(time.Now().Add(60 * 24 * time.Hour))},
		{Meta: model.Meta{ID: "certpend"}, Name: "vault.home.lan", Domains: []string{"vault.home.lan"}, Provider: model.CertLetsEncrypt, Status: model.CertStatusPending},
	}
	snap.AccessLists = []model.AccessList{
		{Meta: model.Meta{ID: "lanonly"}, Name: "lan-only", Rules: []model.IPRule{{Action: "allow", CIDR: "192.168.0.0/16"}, {Action: "allow", CIDR: "10.0.0.0/8"}}},
		{Meta: model.Meta{ID: "family"}, Name: "family", Rules: []model.IPRule{{Action: "allow", CIDR: "192.168.1.0/24"}},
			BasicAuth:  model.BasicAuth{Enabled: true, Realm: "Family only", Users: []model.BasicAuthUser{{Username: "mira", PasswordHash: "$2a$10$abcdefghijklmnopqrstuv"}, {Username: "jonas", PasswordHash: "$2a$10$zyxwvutsrqponmlkjihgfe"}, {Username: "nohash"}}},
			SatisfyAny: true},
	}
	snap.Hosts = []model.ProxyHost{
		{
			Meta: model.Meta{ID: "hostcloud"}, Domains: []string{"cloud.home.lan"}, Enabled: true,
			Upstream:   model.Upstream{Scheme: "https", Host: "10.0.0.30", Port: 443},
			Websockets: true, BlockExploits: true, CacheAssets: true, AccessListID: "family",
			CertificateID: "certwild", ForceHTTPS: true, HTTP2: true, HSTS: "inherit", UpstreamTLSVerify: false, CipherProfile: "modern",
			Locations: []model.Location{
				{ID: "l1", Path: "/office/", Kind: model.LocationProxy, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.36", Port: 9980}, Websockets: true, StripPrefix: true,
					Headers: []model.Header{{Name: "X-Forwarded-Prefix", Value: "/office"}, {Name: "Bad Header", Value: "x"}}},
				{ID: "l2", Path: "/.well-known/", Kind: model.LocationSame, NoAuth: true},
				{ID: "l3", Path: "/metrics", Kind: model.LocationDeny},
			},
			ForwardAuth: model.ForwardAuth{Enabled: true, Provider: "authelia", VerifyURL: "http://10.0.0.5:9091/api/verify?rd=https://auth.home.lan", SignInURL: "https://auth.home.lan/", PassRemoteUser: true, PassRemoteGroups: true, SkipWellKnown: true},
			RateLimit:   model.RateLimit{Enabled: true, RequestsPerSecond: 30, Burst: 60, ExemptAccessListID: "lanonly"},
			GeoBlock:    model.GeoBlock{Enabled: true, AllowCountries: []string{"de", "US"}},
			NoIndex:     true, MaxBodySize: "10g", ProxyReadTimeout: 300, ProxySendTimeout: 300,
			CustomNginx: "proxy_hide_header X-Powered-By;",
		},
		{
			Meta: model.Meta{ID: "hostgrafana"}, Domains: []string{"grafana.home.lan"}, Enabled: true,
			Upstream:      model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000, Path: "/grafana/"},
			CertificateID: "certwild", HTTP2: true, HTTP3: boolp(false), HSTS: "off", AccessListID: "lanonly",
		},
		{
			Meta: model.Meta{ID: "hostvault"}, Domains: []string{"vault.home.lan"}, Enabled: true,
			Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.40", Port: 8200}, CertificateID: "certpend", ForceHTTPS: true,
		},
		{Meta: model.Meta{ID: "hostold"}, Domains: []string{"old-wiki.home.lan"}, Enabled: false, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.9", Port: 80}},
		{Meta: model.Meta{ID: "hostapp"}, Domains: []string{"home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 10080, BackendID: "web"}},
	}
	snap.Redirects = []model.Redirect{
		{Meta: model.Meta{ID: "r1"}, Domains: []string{"www.example.com"}, To: "https://example.com", Code: 301, KeepPath: true, Enabled: true},
		{Meta: model.Meta{ID: "r2"}, Domains: []string{"wiki.home.lan"}, To: "https://docs.home.lan/wiki/", Code: 302, CertificateID: "certwild", ForceHTTPS: true, Enabled: true},
		{Meta: model.Meta{ID: "r3"}, Domains: []string{"home.lan"}, FromPath: "/old-blog", To: "https://blog.example.com", Code: 308, KeepPath: true, Enabled: true},
	}
	snap.Backends = []model.Backend{{Meta: model.Meta{ID: "pg"}, Name: "postgres-ro", Mode: "tcp"}}
	snap.Frontends = []model.Frontend{{Meta: model.Meta{ID: "fe"}, Name: "pg-local", Mode: "tcp", Bind: "127.0.0.1:15432", DefaultBackendID: "pg", Enabled: true}}
	snap.Streams = []model.Stream{
		{Meta: model.Meta{ID: "s1"}, Name: "minecraft", Protocol: "tcp", ListenAddress: "0.0.0.0", ListenPorts: "25565", ForwardHost: "10.0.0.50", IdleTimeout: "10m", Enabled: true},
		{Meta: model.Meta{ID: "s2"}, Name: "valheim", Protocol: "udp", ListenAddress: "0.0.0.0", ListenPorts: "2456-2458", ForwardHost: "10.0.0.61", ProxyProtocol: true, Enabled: true},
		{Meta: model.Meta{ID: "s3"}, Name: "dns", Protocol: "both", ListenPorts: "5353-5354", ForwardHost: "pihole.lan", ForwardPorts: "53-54", Enabled: true},
		{Meta: model.Meta{ID: "s4"}, Name: "shifted", Protocol: "tcp", ListenPorts: "7000-7001", ForwardHost: "10.0.0.7", ForwardPorts: "8000-8001", Enabled: true},
		{Meta: model.Meta{ID: "s5"}, Name: "postgres", Protocol: "tcp", ListenAddress: "10.0.0.1", ListenPorts: "5433", BackendID: "pg", Enabled: true},
		{Meta: model.Meta{ID: "s6"}, Name: "off", Protocol: "tcp", ListenPorts: "9999", ForwardHost: "10.0.0.1", Enabled: false},
	}
	return snap
}

func mustContain(t *testing.T, file, content string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(content, w) {
			t.Errorf("%s: missing %q\n---\n%s", file, w, content)
		}
	}
}

func mustNotContain(t *testing.T, file, content string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(content, b) {
			t.Errorf("%s: unexpected %q\n---\n%s", file, b, content)
		}
	}
}

func renderRich(t *testing.T) agent.Files {
	t.Helper()
	files, err := Render(richSnapshot(), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestDumpForNginxT writes a rendered config (without the geo include, which needs a
// database file) to $RELAY_NGINX_RENDER_DIR so it can be checked with
// `nginx -t` inside the engine image.
func TestDumpForNginxT(t *testing.T) {
	dir := os.Getenv("RELAY_NGINX_RENDER_DIR")
	if dir == "" {
		t.Skip("RELAY_NGINX_RENDER_DIR not set")
	}
	env := testEnv()
	env.GeoCountryFile = "" // the include only exists on a Relay host
	files, err := Render(richSnapshot(), env)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
}

func TestRenderDeterministic(t *testing.T) {
	a, _ := Render(richSnapshot(), testEnv())
	b, _ := Render(richSnapshot(), testEnv())
	if agent.HashFiles(a) != agent.HashFiles(b) {
		t.Fatal("render output is not deterministic")
	}
}

func TestRenderFileSet(t *testing.T) {
	files := renderRich(t)
	for _, p := range []string{
		"nginx.conf", "conf.d/00-global.conf", "conf.d/10-default.conf", "snippets/proxy-headers.conf", "snippets/acme-challenge.conf",
		"snippets/block-exploits.conf", "snippets/tls-modern.conf", "snippets/tls-intermediate.conf", "snippets/tls-old.conf",
		"conf.d/hosts/cloud.home.lan.conf", "conf.d/hosts/grafana.home.lan.conf", "conf.d/hosts/vault.home.lan.conf", "conf.d/hosts/home.lan.conf",
		"conf.d/redirects/www.example.com.conf", "conf.d/redirects/wiki.home.lan.conf",
		"streams/minecraft.conf", "streams/valheim.conf", "streams/dns.conf", "streams/shifted.conf", "streams/postgres.conf",
		"htpasswd/family",
	} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing file %s", p)
		}
	}
	for p := range files {
		if strings.Contains(p, "old-wiki") || p == "streams/off.conf" || p == "htpasswd/lanonly" {
			t.Errorf("unexpected file %s", p)
		}
	}
}

func TestRenderMainConf(t *testing.T) {
	f := renderRich(t)["nginx.conf"]
	if strings.Contains(f, "load_module") || strings.Contains(f, "/etc/nginx/modules") {
		t.Fatal("static modules must not be loaded")
	}
	mustContain(t, "nginx.conf", f,
		"error_log /var/log/relay/error.log warn;",
		"log_format relay_json escape=json",
		"access_log /var/log/relay/access.log relay_json;",
		"map $http_upgrade $connection_upgrade {",
		"server_names_hash_bucket_size 128;",
		"client_max_body_size 1m;",
		"gzip on;",
		"keys_zone=relay_assets:32m",
		"include conf.d/*.conf;",
		"include conf.d/hosts/*.conf;",
		"stream {",
		"log_format relay_stream_json escape=json",
		"access_log /var/log/relay/stream-access.log relay_stream_json;",
		"include streams/*.conf;",
	)
	mustNotContain(t, "nginx.conf", f, "resolver", "daemon")

	env := testEnv()
	delete(env.Modules, "stream")
	s := richSnapshot()
	s.Streams = nil
	files, err := Render(s, env)
	if err != nil {
		t.Fatal(err)
	}
	mustNotContain(t, "nginx.conf (no stream)", files["nginx.conf"], "stream {")
	s.Streams = richSnapshot().Streams
	if _, err := Render(s, env); err == nil {
		t.Error("expected an error for streams without the stream module")
	}
}

func TestRenderGlobal(t *testing.T) {
	f := renderRich(t)["conf.d/00-global.conf"]
	mustContain(t, "00-global", f,
		"geo $relay_blocked {", "203.0.113.88 1;", "198.51.100.0/24 1;", "# skipped invalid entry not-an-ip",
		"geo $relay_geo_country {", `default "--";`, `10.0.0.0/8 "";`, `fc00::/7 "";`, "include /data/geoip/nginx/countries-test.conf;",
		"geo $relay_rl_exempt_hostcloud {", "192.168.0.0/16 1;", "map $relay_rl_exempt_hostcloud $relay_rl_key_hostcloud {",
		"limit_req_zone $relay_rl_key_hostcloud zone=relay_rl_hostcloud:10m rate=30r/s;",
		"map $relay_geo_country $relay_geo_allow_hostcloud {", "DE 1;", "US 1;", `"" 1;`,
	)
	// blocklist ordering is stable (sorted)
	if strings.Index(f, "198.51.100.0/24") > strings.Index(f, "203.0.113.88") {
		t.Error("blocklist not sorted")
	}
}

func TestRenderTLSHost(t *testing.T) {
	f := renderRich(t)["conf.d/hosts/cloud.home.lan.conf"]
	mustContain(t, "cloud", f,
		"set $relay_host_id hostcloud;",
		"listen 80;", "listen [::]:80;",
		"return 301 https://$host$request_uri;",
		"listen 443 ssl;", "listen [::]:443 ssl;", "listen 443 quic;",
		"http2 on;",
		"ssl_certificate /data/certs/certwild/fullchain.pem;",
		"ssl_certificate_key /data/certs/certwild/privkey.pem;",
		"include snippets/tls-modern.conf;",
		`add_header Strict-Transport-Security "max-age=15768000; includeSubDomains" always;`,
		`add_header Alt-Svc 'h3=":443"; ma=86400' always;`,
		"if ($relay_blocked) {",
		"if ($relay_geo_allow_hostcloud = 0) {",
		"include snippets/block-exploits.conf;",
		`add_header X-Robots-Tag "noindex, nofollow" always;`,
		"client_max_body_size 10g;",
		"proxy_read_timeout 300s;", "proxy_send_timeout 300s;",
		"limit_req zone=relay_rl_hostcloud burst=60 nodelay;", "limit_req_status 429;",
		"proxy_ssl_server_name on;", "proxy_ssl_verify off;",
		"location = /.relay/auth {", "internal;", `proxy_pass http://10.0.0.5:9091/api/verify?rd=https://auth.home.lan;`,
		"location @relay_signin {", `return 302 https://auth.home.lan/?rd=$scheme://$http_host$request_uri;`,
		"auth_request /.relay/auth;", "auth_request_set $relay_remote_user $upstream_http_remote_user;",
		"proxy_set_header Remote-User $relay_remote_user;", "proxy_set_header Remote-Groups $relay_remote_groups;",
		"error_page 401 = @relay_signin;",
		"# access list family", "allow 192.168.1.0/24;", "deny all;", `auth_basic "Family only";`, "auth_basic_user_file htpasswd/family;", "satisfy any;",
		"location /office/ {", `rewrite ^/office/?(.*)$ /$1 break;`, "proxy_pass http://10.0.0.36:9980;",
		"proxy_set_header X-Forwarded-Prefix /office;",
		"proxy_set_header Upgrade $http_upgrade;", "proxy_set_header Connection $connection_upgrade;",
		"location /.well-known/ {", "# no access list or forward auth for this path",
		"location /metrics {", "return 403;",
		"location / {", "proxy_pass https://10.0.0.30:443;",
		`location ~* "\\.(?:css|js|mjs|map|png|jpe?g|gif|ico|svg|webp|avif|bmp|woff2?|ttf|otf|eot|mp4|webm|ogg|mp3|wav|pdf)$" {`,
		"proxy_cache relay_assets;", "proxy_cache_valid 200 301 302 30d;",
		"include snippets/acme-challenge.conf;",
		"# Custom nginx snippet", "proxy_hide_header X-Powered-By;",
	)
	mustNotContain(t, "cloud", f, "Bad Header", "reuseport", "ssl_stapling", "listen 443 ssl default_server")
	// Longest prefix first.
	if !(strings.Index(f, "location /.well-known/ {") < strings.Index(f, "location /office/ {") &&
		strings.Index(f, "location /metrics {") < strings.Index(f, "location /office/ {") &&
		strings.Index(f, "location /office/ {") < strings.LastIndex(f, "location / {")) {
		t.Error("locations not sorted longest prefix first")
	}
	// The ACME snippet is only on the :80 server when HTTPS is forced.
	if strings.Count(f, "include snippets/acme-challenge.conf;") != 1 {
		t.Error("expected exactly one ACME include")
	}
	// Balanced braces.
	if strings.Count(f, "{") != strings.Count(f, "}") {
		t.Error("unbalanced braces")
	}
}

func TestRenderHTTPOnlyAndPaths(t *testing.T) {
	files := renderRich(t)
	g := files["conf.d/hosts/grafana.home.lan.conf"]
	mustContain(t, "grafana", g,
		"listen 80;", "listen 443 ssl;", "include snippets/tls-intermediate.conf;",
		"# access list lan-only", "allow 192.168.0.0/16;", "allow 10.0.0.0/8;", "deny all;",
		`rewrite ^(.*)$ /grafana$1 break;`, "proxy_pass http://10.0.0.21:3000;",
	)
	mustNotContain(t, "grafana", g, "quic", "Strict-Transport-Security", "Alt-Svc", "auth_basic", "proxy_ssl_verify", "Upgrade")

	v := files["conf.d/hosts/vault.home.lan.conf"]
	mustContain(t, "vault", v, "# certificate vault.home.lan has not been issued yet (pending): serving HTTP only until it is valid", "listen 80;", "proxy_pass http://10.0.0.40:8200;")
	mustNotContain(t, "vault", v, "ssl", "return 301")

	h := files["conf.d/hosts/home.lan.conf"]
	mustContain(t, "home.lan", h, "proxy_pass http://127.0.0.1:10080;", "# Redirect /old-blog → https://blog.example.com",
		`location ~ "^/old-blog(?<relay_rest>/.*)?$" {`, `return 308 https://blog.example.com$relay_rest$is_args$args;`)
	if _, ok := files["conf.d/redirects/home.lan.conf"]; ok {
		t.Error("path redirect for a host domain must be rendered inside the host")
	}
}

func TestRenderRedirectsAndDefault(t *testing.T) {
	files := renderRich(t)
	www := files["conf.d/redirects/www.example.com.conf"]
	mustContain(t, "www", www, "server_name www.example.com;", `set $relay_host_id "";`, "return 301 https://example.com$request_uri;", "listen 80;")
	mustNotContain(t, "www", www, "ssl")

	wiki := files["conf.d/redirects/wiki.home.lan.conf"]
	mustContain(t, "wiki", wiki, "return 301 https://$host$request_uri;", "listen 443 ssl;", "return 302 https://docs.home.lan/wiki/;", "ssl_certificate /data/certs/certwild/fullchain.pem;")

	d := files["conf.d/10-default.conf"]
	mustContain(t, "default", d,
		"listen 80 default_server;", "listen [::]:80 default_server;", "listen 443 ssl default_server;",
		"listen 443 quic reuseport default_server;", "listen [::]:443 quic reuseport default_server;",
		"server_name _;", `set $relay_host_id "";`,
		"ssl_certificate /data/certs/_default/fullchain.pem;",
		"include snippets/acme-challenge.conf;", "return 404 '<!doctype html>",
		"listen 127.0.0.1:18080;", "stub_status;",
	)
	mustNotContain(t, "default", d, "ssl_reject_handshake")
	if strings.Count(strings.Join(mapValues(files), "\n"), "reuseport") != 2 {
		t.Error("reuseport must appear exactly once per address (v4 + v6) across all servers")
	}

	acme := files["snippets/acme-challenge.conf"]
	mustContain(t, "acme", acme, "location ^~ /.well-known/acme-challenge/ {", "root /data/acme;")

	s := richSnapshot()
	for action, want := range map[string]string{"close": "return 444;", "redirect": "return 302 https://home.lan;", "host": "proxy_pass http://10.0.0.21:3000;"} {
		s.DefaultHost = model.DefaultHostSettings{Action: action, RedirectTo: "https://home.lan", HostID: "hostgrafana", CertificateID: "certwild"}
		out, err := Render(s, testEnv())
		if err != nil {
			t.Fatal(err)
		}
		mustContain(t, "default/"+action, out["conf.d/10-default.conf"], want, "ssl_certificate /data/certs/certwild/fullchain.pem;")
	}
}

func mapValues(m agent.Files) []string {
	out := []string{}
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestRenderStreams(t *testing.T) {
	files := renderRich(t)
	mustContain(t, "minecraft", files["streams/minecraft.conf"], "listen 0.0.0.0:25565;", "set $relay_stream_id s1;", "proxy_timeout 10m;", "proxy_pass 10.0.0.50:25565;")
	mustContain(t, "valheim", files["streams/valheim.conf"], "listen 0.0.0.0:2456-2458 udp;", "proxy_pass 10.0.0.61:$server_port;", "proxy_protocol on;")
	dns := files["streams/dns.conf"]
	mustContain(t, "dns", dns, "listen 5353;", "listen 5353 udp;", "proxy_pass pihole.lan:53;", "listen 5354;", "proxy_pass pihole.lan:54;")
	if strings.Count(dns, "server {") != 2 {
		t.Error("hostname range must render one server per port")
	}
	mustContain(t, "shifted", files["streams/shifted.conf"], "map $server_port $relay_stream_port_s4 {", "7000 8000;", "7001 8001;", "proxy_pass 10.0.0.7:$relay_stream_port_s4;")
	mustContain(t, "postgres", files["streams/postgres.conf"], "listen 10.0.0.1:5433;", "proxy_pass 127.0.0.1:15432;")

	s := richSnapshot()
	s.Frontends = nil
	if _, err := Render(s, testEnv()); err == nil || !strings.Contains(err.Error(), "postgres-ro") {
		t.Errorf("expected missing frontend error, got %v", err)
	}
	s = richSnapshot()
	s.Streams[3].ForwardPorts = "8000-8005"
	if _, err := Render(s, testEnv()); err == nil {
		t.Error("expected range size mismatch error")
	}
}

func TestRenderHtpasswd(t *testing.T) {
	f := renderRich(t)["htpasswd/family"]
	if f != "# family\njonas:$2a$10$zyxwvutsrqponmlkjihgfe\nmira:$2a$10$abcdefghijklmnopqrstuv\n" {
		t.Errorf("htpasswd = %q", f)
	}
}

func TestRenderForwardAuthNeedsModule(t *testing.T) {
	env := testEnv()
	delete(env.Modules, "auth_request")
	if _, err := Render(richSnapshot(), env); err == nil || !strings.Contains(err.Error(), "auth_request") {
		t.Errorf("expected auth_request error, got %v", err)
	}
}

func TestRenderGeoSkippedWithoutDatabase(t *testing.T) {
	env := testEnv()
	env.GeoCountryFile = ""
	files, err := Render(richSnapshot(), env)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "global", files["conf.d/00-global.conf"], "# Geo-blocking is configured on 1 host(s) but skipped: the country database isn't downloaded yet")
	mustContain(t, "cloud", files["conf.d/hosts/cloud.home.lan.conf"], "# geo-block skipped")
	mustNotContain(t, "cloud", files["conf.d/hosts/cloud.home.lan.conf"], "relay_geo_allow")
}

func TestRenderNoIPv6NoQUIC(t *testing.T) {
	env := testEnv()
	env.Modules = map[string]bool{"stream": true, "auth_request": true}
	files, err := Render(richSnapshot(), env)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		mustNotContain(t, p, c, "[::]", "quic", "Alt-Svc", "geoip2 /data")
	}
}

func TestRenderCustomPorts(t *testing.T) {
	s := richSnapshot()
	s.General.HTTPPort, s.General.HTTPSPort = 8080, 8443
	files, err := Render(s, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "cloud", files["conf.d/hosts/cloud.home.lan.conf"], "listen 8080;", "listen 8443 ssl;", "return 301 https://$host:8443$request_uri;", `h3=":8443"`)
	mustContain(t, "default", files["conf.d/10-default.conf"], "listen 8080 default_server;", "listen 8443 ssl default_server;")
}

func TestRenderHostPreview(t *testing.T) {
	s := richSnapshot()
	draft := model.ProxyHost{Domains: []string{"new.home.lan"}, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.99", Port: 8080}, Websockets: true}
	out, err := RenderHost(s, &draft, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "preview", out, "server_name new.home.lan;", "proxy_pass http://10.0.0.99:8080;", "proxy_set_header Upgrade $http_upgrade;")
	full := WithHost(s, &draft)
	if len(full.Hosts) != len(s.Hosts)+1 {
		t.Error("WithHost must append new hosts")
	}
	edited := s.Hosts[1]
	edited.Upstream.Port = 3001
	if got := WithHost(s, &edited); len(got.Hosts) != len(s.Hosts) || got.Hosts[1].Upstream.Port != 3001 || s.Hosts[1].Upstream.Port != 3000 {
		t.Error("WithHost must replace by id without mutating the input")
	}
	st, err := RenderStream(s, &model.Stream{Meta: model.Meta{ID: "x"}, Name: "x", Protocol: "tcp", ListenPorts: "1234", ForwardHost: "10.0.0.1"}, testEnv())
	if err != nil || !strings.Contains(st, "proxy_pass 10.0.0.1:1234;") {
		t.Errorf("stream preview = %q, %v", st, err)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/office": "/office", "a b": `"a b"`, `x"y`: `"x\"y"`, "": `""`, "a;b": `"a;b"`, "$host": "$host", "{x}": `"{x}"`,
	} {
		if got := q(in); got != want {
			t.Errorf("q(%q) = %q, want %q", in, got, want)
		}
	}
	if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(safeID("AbC-12.x")) {
		t.Error("safeID")
	}
}

func TestRenderStreamVariableWithoutStreams(t *testing.T) {
	s := richSnapshot()
	s.Streams = nil
	files, err := Render(s, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "nginx.conf", files["nginx.conf"], "map $protocol $relay_stream_id {", `default "";`)
	with := renderRich(t)["nginx.conf"]
	mustNotContain(t, "nginx.conf (streams)", with, "map $protocol $relay_stream_id")
}

func tunnelEnv() render.Env {
	env := testEnv()
	env.Modules["http_realip"] = true
	env.Modules["stream_realip"] = true
	return env
}

func tunnelSnapshot() *model.Snapshot {
	snap := richSnapshot()
	snap.Hosts[0].TunnelGatewayID = "gw1"   // cloud: TLS, ForceHTTPS, HTTP/3
	snap.Hosts[1].TunnelGatewayID = "gw1"   // grafana: TLS without redirect, no HTTP/3
	snap.Streams[0].TunnelGatewayID = "gw1" // minecraft tcp
	snap.Streams[1].TunnelGatewayID = "gw1" // valheim udp: not carried
	snap.Streams[2].TunnelGatewayID = "gw1" // dns both → hostname, shifted ports
	return snap
}

func TestRenderTunnel(t *testing.T) {
	files, err := Render(tunnelSnapshot(), tunnelEnv())
	if err != nil {
		t.Fatal(err)
	}
	main := files["nginx.conf"]
	mustContain(t, "nginx.conf", main,
		"set_real_ip_from unix:;", "real_ip_header proxy_protocol;",
		"map $proxy_protocol_tlv_0xe0 $relay_tunnel {", "map $proxy_protocol_addr $relay_forwarded_port {",
		"map $proxy_protocol_addr $relay_alt_svc {", `'"tunnel":"$relay_tunnel"'`)
	mustContain(t, "proxy-headers", files["snippets/proxy-headers.conf"], "X-Forwarded-Port $relay_forwarded_port;")

	def := files["conf.d/10-default.conf"]
	mustContain(t, "default", def,
		"listen unix:/run/relay/tunnel/nginx-http.sock proxy_protocol default_server;",
		"listen unix:/run/relay/tunnel/nginx-https.sock ssl proxy_protocol default_server;",
		"ssl_reject_handshake on;")

	cloud := files["conf.d/hosts/cloud.home.lan.conf"]
	// The HTTPS redirect server and the TLS server both listen on the tunnel.
	if strings.Count(cloud, "listen unix:/run/relay/tunnel/nginx-http.sock proxy_protocol;") != 1 ||
		strings.Count(cloud, "listen unix:/run/relay/tunnel/nginx-https.sock ssl proxy_protocol;") != 1 {
		t.Errorf("cloud tunnel listens:\n%s", cloud)
	}
	mustContain(t, "cloud", cloud, "add_header Alt-Svc $relay_alt_svc always;")

	grafana := files["conf.d/hosts/grafana.home.lan.conf"]
	mustContain(t, "grafana", grafana, "listen unix:/run/relay/tunnel/nginx-http.sock proxy_protocol;", "listen unix:/run/relay/tunnel/nginx-https.sock ssl proxy_protocol;")
	mustNotContain(t, "vault (not published)", files["conf.d/hosts/vault.home.lan.conf"], "unix:")

	mc := files["streams/minecraft.conf"]
	mustContain(t, "minecraft", mc, "listen unix:/run/relay/tunnel/nginx-stream-25565.sock proxy_protocol;", "set_real_ip_from unix:;", "proxy_pass 10.0.0.50:25565;")
	mustNotContain(t, "valheim", files["streams/valheim.conf"], "unix:")
	dns := files["streams/dns.conf"]
	mustContain(t, "dns", dns, "nginx-stream-5353.sock", "nginx-stream-5354.sock", "proxy_pass pihole.lan:54;")
	if strings.Count(dns, "server {") != 4 {
		t.Errorf("dns: expected 2 public + 2 tunnel servers:\n%s", dns)
	}
	mustContain(t, "stream block", main, "map $proxy_protocol_tlv_0xe0 $relay_tunnel {")

	// Nothing published: no tunnel directives, but $relay_tunnel still exists for the log format.
	plain := renderRich(t)
	mustNotContain(t, "plain nginx.conf", plain["nginx.conf"], "real_ip_header", "unix:")
	mustContain(t, "plain nginx.conf", plain["nginx.conf"], "map $host $relay_tunnel {", "map $protocol $relay_tunnel {")
	mustNotContain(t, "plain default", plain["conf.d/10-default.conf"], "unix:")

	// Custom HTTPS port: redirects through the tunnel go to 443.
	s := tunnelSnapshot()
	s.General.HTTPSPort = 8443
	files, err = Render(s, tunnelEnv())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "custom port", files["nginx.conf"], "map $proxy_protocol_addr $relay_https_port {", `"" ":8443";`)
	mustContain(t, "custom port", files["conf.d/hosts/cloud.home.lan.conf"], "return 301 https://$host$relay_https_port$request_uri;")

	// The realip modules are required.
	if _, err := Render(tunnelSnapshot(), testEnv()); err == nil || !strings.Contains(err.Error(), "realip") {
		t.Errorf("expected missing realip module error, got %v", err)
	}
}

// TestDumpTunnelForNginxT is TestDumpForNginxT with tunnels.
func TestDumpTunnelForNginxT(t *testing.T) {
	dir := os.Getenv("RELAY_NGINX_TUNNEL_RENDER_DIR")
	if dir == "" {
		t.Skip("RELAY_NGINX_TUNNEL_RENDER_DIR not set")
	}
	env := tunnelEnv()
	env.GeoCountryFile = ""
	files, err := Render(tunnelSnapshot(), env)
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
}
