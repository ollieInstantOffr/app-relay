package edge

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func shutdownQuick(s *Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Shutdown(ctx)
	s.Close()
}

// waitAccessLine waits for an access.log line matching pred.
func waitAccessLine(t testing.TB, logDir string, pred func(map[string]string) bool) map[string]string {
	t.Helper()
	var found map[string]string
	ok := waitFor(t, 5*time.Second, func() bool {
		data, _ := os.ReadFile(filepath.Join(logDir, "access.log"))
		for _, l := range strings.Split(string(data), "\n") {
			if l == "" {
				continue
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("invalid access log line %q: %v", l, err)
			}
			if pred(m) {
				found = m
				return true
			}
		}
		return false
	})
	if !ok {
		data, _ := os.ReadFile(filepath.Join(logDir, "access.log"))
		t.Fatalf("no matching access log line in:\n%s", data)
	}
	return found
}

func TestBlockExploitsHost(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, nil)
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.BlockExploits = true
	cfg.Hosts = []Host{h, proxyHost("open", []string{"open.test"}, up.ref())}
	e := startEnv(t, cfg, dir)
	if res := e.get("http://h.test/ok?q=1"); res.StatusCode != 200 {
		t.Fatalf("clean request: %d", res.StatusCode)
	}
	for _, tt := range []struct{ url, ua string }{
		{"http://h.test/?id=1%20union%20select%20pw", ""},
		{"http://h.test/.git/config", ""},
		{"http://h.test/", "sqlmap/1.0"},
	} {
		res := e.get(tt.url, "User-Agent", tt.ua)
		if res.StatusCode != 403 || strings.Contains(strings.ToLower(res.body), "nginx") {
			t.Errorf("%s (%s): %d", tt.url, tt.ua, res.StatusCode)
		}
	}
	if res := e.get("http://open.test/.git/config"); res.StatusCode != 200 {
		t.Errorf("host without block exploits: %d", res.StatusCode)
	}
}

func TestLocations(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, nil)
	base := up.ref()
	base.Path = "/grafana/"
	cfg.Hosts = []Host{{ID: "h", Domains: []string{"h.test"}, Locations: []Location{
		{Path: "/app/admin/", Kind: "deny"},
		{Path: "/app/", Kind: "proxy", Upstream: up.ref(), StripPrefix: true},
		{Path: "/base/", Kind: "proxy", Upstream: base},
		{Path: "/both/", Kind: "proxy", Upstream: base, StripPrefix: true},
		{Path: "/private", Kind: "deny"},
		{Path: "/", Kind: "proxy", Upstream: up.ref()},
	}}}
	e := startEnv(t, cfg, dir)
	tests := []struct {
		path, wantURI string
		status        int
		location      string
	}{
		{"/app/x/y?q=1", "/x/y?q=1", 200, ""},
		{"/app/", "/", 200, ""},
		{"/app/sp%20ace", "/sp%20ace", 200, ""},
		{"/base/x?q", "/grafana/base/x?q", 200, ""},
		{"/both/x", "/grafana/x", 200, ""},
		{"/raw/a%2Fb/%7e?x=%41", "/raw/a%2Fb/%7e?x=%41", 200, ""},
		{"/x//merged//y", "/x//merged//y", 200, ""},
		{"/app/admin/secret", "", 403, ""},
		{"/app//admin/secret", "", 403, ""},
		{"/private/x", "", 403, ""},
		{"/app?keep=1", "", 301, fmt.Sprintf("http://h.test:%d/app/?keep=1", cfg.HTTPPort)},
		{"/base", "", 301, fmt.Sprintf("http://h.test:%d/base/", cfg.HTTPPort)},
		{"/a/../../etc", "", 400, ""},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest("GET", "http://h.test/", nil)
		req.URL.Opaque = tt.path // send the path exactly as written
		req.URL.Path, req.URL.RawQuery = "", ""
		if p, q, ok := strings.Cut(tt.path, "?"); ok {
			req.URL.Opaque, req.URL.RawQuery = p, q
		}
		res := e.do(req)
		if res.StatusCode != tt.status {
			t.Errorf("%s: status %d, want %d (%q)", tt.path, res.StatusCode, tt.status, res.body)
			continue
		}
		if tt.wantURI != "" {
			if got, _ := field(res.body, "uri"); got != tt.wantURI {
				t.Errorf("%s: upstream uri %q, want %q", tt.path, got, tt.wantURI)
			}
		}
		if tt.location != "" && res.Header.Get("Location") != tt.location {
			t.Errorf("%s: Location %q, want %q", tt.path, res.Header.Get("Location"), tt.location)
		}
	}
}

func TestProxyHeadersAndVariables(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, nil)
	cfg.Hosts = []Host{{ID: "h", Domains: []string{"h.test"}, Locations: []Location{
		{Path: "/strip/", Kind: "proxy", Upstream: up.ref(), StripPrefix: true, Headers: []Header{{Name: "X-Uri", Value: "$uri"}}},
		{Path: "/", Kind: "proxy", Upstream: up.ref(), Headers: []Header{
			{Name: "X-Custom", Value: "$host|$http_host|$remote_addr|$scheme|$request_uri|$uri|$is_args$args|$server_port|$request_method|$http_x_foo"},
			{Name: "X-Forwarded-Proto", Value: ""},
			{Name: "X_under", Value: "set"},
		}},
	}}}
	e := startEnv(t, cfg, dir)
	res := e.get("http://edge/p/a?b=1", "Host", "H.test:8080", "X-Foo", "foo", "X-Forwarded-For", "203.0.113.9", "X_under", "client", "X-Request-Id", "spoofed")
	want := map[string]string{
		"host":             "h.test",
		"x-real-ip":        "127.0.0.1",
		"x-forwarded-for":  "203.0.113.9, 127.0.0.1",
		"x-forwarded-host": "h.test",
		"x-forwarded-port": strconv.Itoa(cfg.HTTPPort),
		"x_under":          "set",
		"x-custom":         fmt.Sprintf("h.test|H.test:8080|127.0.0.1|http|/p/a?b=1|/p/a|?b=1|%d|GET|foo", cfg.HTTPPort),
	}
	for k, v := range want {
		if got, _ := field(res.body, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if _, ok := field(res.body, "x-forwarded-proto"); ok {
		t.Error("empty header value must remove the header")
	}
	if id, _ := field(res.body, "x-request-id"); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("X-Request-ID = %q", id)
	}
	res = e.get("http://h.test/strip/deep/x", "X_under", "client")
	if got, _ := field(res.body, "x-uri"); got != "/deep/x" {
		t.Errorf("$uri after strip = %q", got)
	}
	if _, ok := field(res.body, "x_under"); ok {
		t.Error("client header with underscore was forwarded")
	}
}

// wsUpstream completes a websocket-style upgrade and echoes bytes.
func wsUpstream(t *testing.T) *upstream {
	return newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "upgrade required", http.StatusUpgradeRequired)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(c, brw)
	}))
}

func rawUpgrade(t *testing.T, port int, host, path string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", path, host)
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	return c, br, status
}

func TestWebsocketUpgrade(t *testing.T) {
	cfg, dir := newConfig(t)
	up := wsUpstream(t)
	h := Host{ID: "h", Domains: []string{"h.test"}, ReadTimeoutSec: 1, Locations: []Location{
		{Path: "/ws", Kind: "proxy", Upstream: up.ref(), Websockets: true},
		{Path: "/", Kind: "proxy", Upstream: up.ref()},
	}}
	cfg.Hosts = []Host{h}
	e := startEnv(t, cfg, dir)

	c, br, status := rawUpgrade(t, cfg.HTTPPort, "h.test", "/ws")
	defer c.Close()
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade status %q", status)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	// Idle read timeout (1 s) closes the tunnel.
	c.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected the idle websocket to be closed")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("idle close took %v", d)
	}

	c2, _, status := rawUpgrade(t, cfg.HTTPPort, "h.test", "/plain")
	c2.Close()
	if strings.Contains(status, "101") {
		t.Fatal("upgrade allowed on a location without websockets")
	}
	_ = e
}

func TestForwardAuth(t *testing.T) {
	var lastAuth atomic.Pointer[http.Request]
	auth := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Clone(context.Background()))
		switch r.Header.Get("X-Test-Auth") {
		case "allow":
			w.Header().Set("Remote-User", "alice")
			w.Header().Set("Remote-Groups", "admins")
			w.WriteHeader(204)
		case "deny":
			w.Header().Set("X-Reason", "policy")
			w.WriteHeader(403)
			io.WriteString(w, "denied by policy")
		case "boom":
			w.WriteHeader(502)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sso"`)
			w.WriteHeader(401)
		}
	}))
	app := newUpstream(t, nil)
	mk := func(signIn string, passUser bool) Host {
		return Host{ID: "h", Domains: []string{"h.test"},
			ForwardAuth: &ForwardAuth{VerifyURL: auth.URL + "/verify", SignInURL: signIn, PassRemoteUser: passUser, PassRemoteGroups: passUser},
			Locations: []Location{
				{Path: "/public/", Kind: "proxy", Upstream: app.ref()},
				{Path: "/", Kind: "proxy", Upstream: app.ref(), ForwardAuth: true},
			}}
	}

	t.Run("pass", func(t *testing.T) {
		cfg, dir := newConfig(t)
		cfg.Hosts = []Host{mk("", true)}
		e := startEnv(t, cfg, dir)
		res := e.get("http://h.test/p?a=1", "X-Test-Auth", "allow", "Remote-User", "mallory", "Remote-Groups", "root")
		if res.StatusCode != 200 {
			t.Fatalf("allow: %d %q", res.StatusCode, res.body)
		}
		if u, _ := field(res.body, "remote-user"); u != "alice" {
			t.Errorf("Remote-User = %q", u)
		}
		if g, _ := field(res.body, "remote-groups"); g != "admins" {
			t.Errorf("Remote-Groups = %q", g)
		}
		ar := lastAuth.Load()
		wantURL := "http://h.test/p?a=1" // $http_host as sent by the client
		if ar.Method != "GET" || ar.RequestURI != "/verify" || ar.Header.Get("X-Original-URL") != wantURL ||
			ar.Header.Get("X-Forwarded-Uri") != "/p?a=1" || ar.Header.Get("X-Original-Method") != "GET" ||
			ar.Header.Get("X-Real-IP") != "127.0.0.1" || ar.Host != strings.TrimPrefix(auth.URL, "http://") {
			t.Errorf("auth subrequest: %s %s host=%s %v", ar.Method, ar.RequestURI, ar.Host, ar.Header)
		}
		res = e.get("http://h.test/public/x", "Remote-User", "mallory")
		if u, _ := field(res.body, "remote-user"); res.StatusCode != 200 || u != "mallory" {
			t.Errorf("location without forward auth: %d remote-user=%q", res.StatusCode, u)
		}
		res = e.get("http://h.test/p", "X-Test-Auth", "deny")
		if res.StatusCode != 403 || res.body != "denied by policy" || res.Header.Get("X-Reason") != "policy" {
			t.Errorf("deny: %d %q %v", res.StatusCode, res.body, res.Header)
		}
		res = e.get("http://h.test/p")
		if res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") != `Bearer realm="sso"` {
			t.Errorf("401: %d %v", res.StatusCode, res.Header)
		}
		if res := e.get("http://h.test/p", "X-Test-Auth", "boom"); res.StatusCode != 500 {
			t.Errorf("unexpected verify status: %d", res.StatusCode)
		}
	})

	t.Run("strip without pass", func(t *testing.T) {
		cfg, dir := newConfig(t)
		cfg.Hosts = []Host{mk("", false)}
		e := startEnv(t, cfg, dir)
		res := e.get("http://h.test/p", "X-Test-Auth", "allow", "Remote-User", "mallory")
		if _, ok := field(res.body, "remote-user"); res.StatusCode != 200 || ok {
			t.Errorf("client Remote-User must be stripped: %d %q", res.StatusCode, res.body)
		}
	})

	t.Run("sign in", func(t *testing.T) {
		cfg, dir := newConfig(t)
		h := mk("https://sso.test/login?app=x", false)
		cfg.AccessLists = map[string]AccessList{"basic": {BasicAuth: &BasicAuth{Realm: "r", UsersFile: "htpasswd/basic"}}}
		os.MkdirAll(filepath.Join(dir, "htpasswd"), 0o755)
		os.WriteFile(filepath.Join(dir, "htpasswd", "basic"), []byte("u:{PLAIN}p\n"), 0o644)
		h.Locations = append(h.Locations[:1:1], Location{Path: "/basic/", Kind: "proxy", Upstream: app.ref(), ForwardAuth: true, AccessListID: "basic"}, h.Locations[1])
		cfg.Hosts = []Host{h}
		e := startEnv(t, cfg, dir)
		want := "https://sso.test/login?app=x&rd=http%3A%2F%2Fh.test%2Fp%3Fa%3D1"
		res := e.get("http://h.test/p?a=1")
		if res.StatusCode != 302 || res.Header.Get("Location") != want {
			t.Errorf("sign-in redirect: %d %q, want %q", res.StatusCode, res.Header.Get("Location"), want)
		}
		res = e.get("http://h.test/basic/x", "X-Test-Auth", "allow")
		if res.StatusCode != 302 || !strings.HasPrefix(res.Header.Get("Location"), "https://sso.test/login?app=x&rd=") {
			t.Errorf("basic auth 401 on a forward-auth location must redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
		}
		req, _ := http.NewRequest("GET", "http://h.test/basic/x", nil)
		req.SetBasicAuth("u", "p")
		req.Header.Set("X-Test-Auth", "allow")
		if res := e.do(req); res.StatusCode != 200 {
			t.Errorf("basic + forward auth pass: %d", res.StatusCode)
		}
	})
}

func TestAccessListsViaConfig(t *testing.T) {
	up := newUpstream(t, nil)
	rules := func(r ...IPRule) []IPRule { return r }
	tests := []struct {
		name   string
		list   AccessList
		denyAl bool
		user   string
		want   int
	}{
		{"first match deny", AccessList{Rules: rules(IPRule{false, "127.0.0.1"}, IPRule{true, "all"})}, false, "", 403},
		{"first match allow", AccessList{Rules: rules(IPRule{true, "127.0.0.0/8"}, IPRule{false, "all"})}, false, "", 200},
		{"implicit deny all", AccessList{Rules: rules(IPRule{true, "10.0.0.0/8"}, IPRule{false, "all"})}, false, "", 403},
		{"no match allowed", AccessList{Rules: rules(IPRule{false, "10.0.0.0/8"})}, false, "", 200},
		{"deny all location", AccessList{}, true, "", 403},
		{"satisfy all no creds", AccessList{Rules: rules(IPRule{true, "127.0.0.1"}, IPRule{false, "all"}), BasicAuth: &BasicAuth{UsersFile: "users"}}, false, "", 401},
		{"satisfy all creds", AccessList{Rules: rules(IPRule{true, "127.0.0.1"}, IPRule{false, "all"}), BasicAuth: &BasicAuth{UsersFile: "users"}}, false, "u", 200},
		{"satisfy all ip denied", AccessList{Rules: rules(IPRule{false, "all"}), BasicAuth: &BasicAuth{UsersFile: "users"}}, false, "u", 403},
		{"satisfy any ip", AccessList{Rules: rules(IPRule{true, "127.0.0.1"}, IPRule{false, "all"}), BasicAuth: &BasicAuth{UsersFile: "users"}, SatisfyAny: true}, false, "", 200},
		{"satisfy any creds", AccessList{Rules: rules(IPRule{false, "all"}), BasicAuth: &BasicAuth{UsersFile: "users"}, SatisfyAny: true}, false, "u", 200},
		{"satisfy any neither", AccessList{Rules: rules(IPRule{false, "all"}), BasicAuth: &BasicAuth{UsersFile: "users"}, SatisfyAny: true}, false, "", 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, dir := newConfig(t)
			os.WriteFile(filepath.Join(dir, "users"), []byte("u:$apr1$Q0VhdM3G$Z797o9kqseSZMgOLfRlWq0\n"), 0o644)
			loc := Location{Path: "/", Kind: "proxy", Upstream: up.ref(), AccessListID: "al", DenyAll: tt.denyAl}
			if tt.denyAl {
				loc.AccessListID = ""
			}
			cfg.AccessLists = map[string]AccessList{"al": tt.list}
			cfg.Hosts = []Host{{ID: "h", Domains: []string{"h.test"}, Locations: []Location{loc}}}
			os.MkdirAll(filepath.Join(dir, "acme", ".well-known", "acme-challenge"), 0o755)
			os.WriteFile(filepath.Join(dir, "acme", ".well-known", "acme-challenge", "t0k-en_1"), []byte("acme"), 0o644)
			e := startEnv(t, cfg, dir)
			req, _ := http.NewRequest("GET", "http://h.test/x", nil)
			if tt.user != "" {
				req.SetBasicAuth(tt.user, "s3cret-Pass")
			}
			res := e.do(req)
			if res.StatusCode != tt.want {
				t.Fatalf("status %d, want %d", res.StatusCode, tt.want)
			}
			if tt.want == 401 && res.Header.Get("WWW-Authenticate") != `Basic realm="Restricted", charset="UTF-8"` {
				t.Errorf("WWW-Authenticate = %q", res.Header.Get("WWW-Authenticate"))
			}
			if res := e.get("http://h.test/.well-known/acme-challenge/t0k-en_1"); res.StatusCode != 200 || res.body != "acme" {
				t.Errorf("ACME must bypass access checks: %d", res.StatusCode)
			}
		})
	}
}

func TestRedirectsAndACME(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, nil)
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.PathRedirects = []PathRedirect{
		{From: "/old", To: "https://new.test/base/", Code: 308, KeepPath: true},
		{From: "/docs", To: "https://docs.test", Code: 302},
		{From: "/v", To: "https://$host/new$is_args$args", Code: 301},
	}
	cfg.Hosts = []Host{h}
	cfg.Redirects = []RedirectGroup{
		{Domains: []string{"r.test"}, Whole: &Redirect{To: "https://target.test/", Code: 301, KeepPath: true}, Paths: []PathRedirect{{From: "/special", To: "https://special.test", Code: 307}}},
		{Domains: []string{"paths.test"}, Paths: []PathRedirect{{From: "/a", To: "https://a.test/", Code: 302, KeepPath: true}}},
	}
	os.MkdirAll(filepath.Join(dir, "acme", ".well-known", "acme-challenge"), 0o755)
	os.WriteFile(filepath.Join(dir, "acme", ".well-known", "acme-challenge", "tok_1-A"), []byte("key-auth"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret"), []byte("nope"), 0o644)
	e := startEnv(t, cfg, dir)
	tests := []struct {
		url    string
		status int
		loc    string
	}{
		{"http://h.test/old/a/b?x=1", 308, "https://new.test/base/a/b?x=1"},
		{"http://h.test/old", 308, "https://new.test/base"},
		{"http://h.test/old%20x/y", 200, ""},
		{"http://h.test/oldx", 200, ""},
		{"http://h.test/docs/x?y", 302, "https://docs.test"},
		{"http://h.test/v?q=1", 301, "https://h.test/new?q=1"},
		{"http://r.test/x/y?z", 301, "https://target.test/x/y?z"},
		{"http://r.test/special/1", 307, "https://special.test"},
		{"http://paths.test/a/b", 302, "https://a.test/b"},
		{"http://paths.test/zzz", 404, ""},
		{"http://h.test/.well-known/acme-challenge/tok_1-A", 200, ""},
		{"http://r.test/.well-known/acme-challenge/tok_1-A", 200, ""},
		{"http://unknown.test/.well-known/acme-challenge/tok_1-A", 200, ""},
		{"http://h.test/.well-known/acme-challenge/missing", 404, ""},
		{"http://h.test/.well-known/acme-challenge/..%2F..%2Fsecret", 200, ""}, // normalizes to /secret: proxied, never read from disk
		{"http://h.test/.well-known/acme-challenge/a.b", 404, ""},
	}
	for _, tt := range tests {
		res := e.get(tt.url)
		if res.StatusCode != tt.status || res.Header.Get("Location") != tt.loc {
			t.Errorf("%s: %d %q, want %d %q", tt.url, res.StatusCode, res.Header.Get("Location"), tt.status, tt.loc)
		}
		if strings.Contains(res.body, "nope") {
			t.Errorf("%s: file outside the webroot served", tt.url)
		}
		if tt.status == 200 && strings.HasSuffix(tt.url, "tok_1-A") {
			if res.body != "key-auth" || res.Header.Get("Content-Type") != "text/plain" {
				t.Errorf("%s: %q %q", tt.url, res.body, res.Header.Get("Content-Type"))
			}
		}
	}
}

func TestMaxBody(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, nil)
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.MaxBodyBytes = 10
	cfg.Hosts = []Host{h}
	e := startEnv(t, cfg, dir)
	post := func(body io.Reader, length int64) int {
		req, _ := http.NewRequest("POST", "http://h.test/upload", body)
		req.ContentLength = length
		return e.do(req).StatusCode
	}
	if s := post(strings.NewReader("0123456789"), 10); s != 200 {
		t.Errorf("body at the limit: %d", s)
	}
	if s := post(strings.NewReader("0123456789a"), 11); s != 413 {
		t.Errorf("Content-Length over the limit: %d", s)
	}
	if s := post(io.MultiReader(strings.NewReader(strings.Repeat("x", 100))), -1); s != 413 {
		t.Errorf("chunked body over the limit: %d", s)
	}
}

func TestGzip(t *testing.T) {
	cfg, dir := newConfig(t)
	big := strings.Repeat("<p>hello</p>", 200)
	up := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("ETag", `"v1"`)
			io.WriteString(w, big)
		case "/small":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"a":1}`)
		case "/png":
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, big)
		case "/encoded":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "br")
			io.WriteString(w, big)
		}
	}))
	cfg.Hosts = []Host{proxyHost("h", []string{"h.test"}, up.ref())}
	e := startEnv(t, cfg, dir)

	res := e.get("http://h.test/big", "Accept-Encoding", "gzip, br")
	if res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Vary") != "Accept-Encoding" || res.Header.Get("ETag") != `W/"v1"` {
		t.Fatalf("gzip headers: %v", res.Header)
	}
	zr, err := gzip.NewReader(strings.NewReader(res.body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != big {
		t.Fatal("gzip body mismatch")
	}
	line := waitAccessLine(t, cfg.LogDir, func(m map[string]string) bool { return m["uri"] == "/big" })
	if n, _ := strconv.Atoi(line["bytes_sent"]); n != len(res.body) {
		t.Errorf("bytes_sent %d, want compressed size %d", n, len(res.body))
	}
	res = e.get("http://h.test/big")
	if res.Header.Get("Content-Encoding") != "" || res.body != big || res.Header.Get("Vary") != "Accept-Encoding" {
		t.Errorf("identity response: %v", res.Header)
	}
	for _, p := range []string{"/small", "/png", "/encoded"} {
		res := e.get("http://h.test"+p, "Accept-Encoding", "gzip")
		if res.Header.Get("Content-Encoding") == "gzip" {
			t.Errorf("%s must not be gzipped", p)
		}
	}
}

func TestAssetCache(t *testing.T) {
	cfg, dir := newConfig(t)
	var hits atomic.Int64
	var failing atomic.Bool
	up := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(503)
			return
		}
		n := hits.Add(1)
		switch {
		case strings.HasPrefix(r.URL.Path, "/slow"):
			time.Sleep(200 * time.Millisecond)
		case strings.HasPrefix(r.URL.Path, "/short"):
			w.Header().Set("Cache-Control", "max-age=1")
		case strings.HasPrefix(r.URL.Path, "/cookie"):
			w.Header().Set("Set-Cookie", "a=b")
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", w.Header().Get("Cache-Control"))
		fmt.Fprintf(w, "body %d %s", n, r.Method)
	}))
	cfg.Hosts = []Host{{ID: "h", Domains: []string{"h.test"}, Locations: []Location{{Path: "/", Kind: "proxy", Upstream: up.ref(), Cache: true}}}}
	e := startEnv(t, cfg, dir)

	first := e.get("http://h.test/app.js?v=1")
	second := e.get("http://h.test/app.js?v=1")
	if first.body != "body 1 GET" || second.body != first.body || hits.Load() != 1 {
		t.Fatalf("cache miss/hit: %q %q hits=%d", first.body, second.body, hits.Load())
	}
	if second.Header.Get("Cache-Control") != "max-age=2592000" || second.Header.Get("Expires") == "" {
		t.Errorf("expires headers: %v", second.Header)
	}
	req, _ := http.NewRequest("HEAD", "http://h.test/app.js?v=1", nil)
	if res := e.do(req); res.StatusCode != 200 || hits.Load() != 1 {
		t.Errorf("HEAD from cache: %d hits=%d", res.StatusCode, hits.Load())
	}
	if res := e.get("http://h.test/app.js?v=2"); res.body != "body 2 GET" {
		t.Errorf("distinct request URI must be a distinct key: %q", res.body)
	}
	e.get("http://h.test/api")
	e.get("http://h.test/api")
	if hits.Load() != 4 {
		t.Errorf("non-asset paths must not be cached: hits=%d", hits.Load())
	}
	e.get("http://h.test/cookie.css")
	e.get("http://h.test/cookie.css")
	if hits.Load() != 6 {
		t.Errorf("Set-Cookie responses must not be cached: hits=%d", hits.Load())
	}

	// Coalescing: concurrent misses fetch once.
	hits.Store(0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.get("http://h.test/slow.png")
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("concurrent misses fetched %d times", hits.Load())
	}

	// Stale on upstream failure.
	fresh := e.get("http://h.test/short.css")
	time.Sleep(1100 * time.Millisecond)
	failing.Store(true)
	stale := e.get("http://h.test/short.css")
	if stale.StatusCode != 200 || stale.body != fresh.body {
		t.Errorf("stale on 503: %d %q, want %q", stale.StatusCode, stale.body, fresh.body)
	}
	if res := e.get("http://h.test/never.css"); res.StatusCode != 503 {
		t.Errorf("uncached failure: %d", res.StatusCode)
	}
}

func TestUpstreamErrors(t *testing.T) {
	cfg, dir := newConfig(t)
	closed := freePort(t, "tcp")
	slow := newUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	dead := Host{ID: "dead", Domains: []string{"dead.test"}, Locations: []Location{{Path: "/", Kind: "proxy", Upstream: Upstream{Scheme: "http", Host: "127.0.0.1", Port: closed}}}}
	timeout := Host{ID: "slow", Domains: []string{"slow.test"}, ReadTimeoutSec: 1, Locations: []Location{{Path: "/", Kind: "proxy", Upstream: slow.ref()}}}
	cfg.Hosts = []Host{dead, timeout}
	e := startEnv(t, cfg, dir)
	res := e.get("http://dead.test/")
	if res.StatusCode != 502 || strings.Contains(strings.ToLower(res.body), "nginx") || !strings.Contains(res.body, "502 Bad Gateway") {
		t.Errorf("502: %d %q", res.StatusCode, res.body)
	}
	start := time.Now()
	res = e.get("http://slow.test/")
	if res.StatusCode != 504 || time.Since(start) > 2500*time.Millisecond {
		t.Errorf("504: %d after %v", res.StatusCode, time.Since(start))
	}
	line := waitAccessLine(t, cfg.LogDir, func(m map[string]string) bool { return m["host_id"] == "dead" })
	if line["status"] != "502" || line["upstream_status"] != "502" || line["upstream_addr"] != "127.0.0.1:"+strconv.Itoa(closed) {
		t.Errorf("502 access line: %v", line)
	}
	if !waitFor(t, 2*time.Second, func() bool { return strings.Contains(e.stderr.String(), "[error] upstream http://127.0.0.1:") }) {
		t.Errorf("upstream error not logged: %s", e.stderr.String())
	}
}

func TestAccessLogFormat(t *testing.T) {
	cfg, dir := newConfig(t)
	up := newUpstream(t, nil)
	cfg.Hosts = []Host{proxyHost("h1", []string{"h.test"}, up.ref())}
	e := startEnv(t, cfg, dir)
	req, _ := http.NewRequest("POST", "http://h.test/path?q=1", strings.NewReader("hello"))
	req.Header.Set("User-Agent", "test-agent")
	req.Header.Set("Referer", "http://ref.test/")
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.SetBasicAuth("bob", "wrong")
	res := e.do(req)
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	var raw string
	waitFor(t, 5*time.Second, func() bool {
		data, _ := os.ReadFile(filepath.Join(cfg.LogDir, "access.log"))
		raw = string(data)
		return strings.Contains(raw, "\n")
	})
	line := strings.SplitN(raw, "\n", 2)[0]
	// Keys in the exact nginx log_format order, all string values.
	dec := json.NewDecoder(strings.NewReader(line))
	dec.Token()
	var keys []string
	values := map[string]string{}
	for dec.More() {
		k, _ := dec.Token()
		v, _ := dec.Token()
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%v is not a string", k)
		}
		keys = append(keys, k.(string))
		values[k.(string)] = s
	}
	wantKeys := "ts host_id host method uri protocol scheme status bytes_sent request_length request_time upstream_addr upstream_status upstream_connect_time upstream_header_time upstream_response_time remote_addr user_agent referer accept x_forwarded_for request_id ssl_protocol remote_user"
	if strings.Join(keys, " ") != wantKeys {
		t.Fatalf("keys:\n%s\nwant:\n%s", strings.Join(keys, " "), wantKeys)
	}
	num := regexp.MustCompile(`^\d+\.\d{3}$`)
	checks := map[string]func(string) bool{
		"ts":                     func(v string) bool { return num.MatchString(v) },
		"host_id":                func(v string) bool { return v == "h1" },
		"host":                   func(v string) bool { return v == "h.test" },
		"method":                 func(v string) bool { return v == "POST" },
		"uri":                    func(v string) bool { return v == "/path?q=1" },
		"protocol":               func(v string) bool { return v == "HTTP/1.1" },
		"scheme":                 func(v string) bool { return v == "http" },
		"status":                 func(v string) bool { return v == "200" },
		"bytes_sent":             func(v string) bool { n, _ := strconv.Atoi(v); return n == len(res.body) },
		"request_length":         func(v string) bool { n, _ := strconv.Atoi(v); return n > 100 },
		"request_time":           func(v string) bool { return num.MatchString(v) },
		"upstream_addr":          func(v string) bool { return v == net.JoinHostPort(up.host, strconv.Itoa(up.port)) },
		"upstream_status":        func(v string) bool { return v == "200" },
		"upstream_connect_time":  func(v string) bool { return num.MatchString(v) },
		"upstream_header_time":   func(v string) bool { return num.MatchString(v) },
		"upstream_response_time": func(v string) bool { return num.MatchString(v) },
		"remote_addr":            func(v string) bool { return v == "127.0.0.1" },
		"user_agent":             func(v string) bool { return v == "test-agent" },
		"referer":                func(v string) bool { return v == "http://ref.test/" },
		"accept":                 func(v string) bool { return v == "text/plain" },
		"x_forwarded_for":        func(v string) bool { return v == "198.51.100.1" },
		"request_id":             func(v string) bool { return regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(v) },
		"ssl_protocol":           func(v string) bool { return v == "" },
		"remote_user":            func(v string) bool { return v == "bob" },
	}
	for k, ok := range checks {
		if !ok(values[k]) {
			t.Errorf("%s = %q", k, values[k])
		}
	}
	if !bytes.HasSuffix([]byte(raw), []byte("\n")) {
		t.Error("line not terminated")
	}
}
