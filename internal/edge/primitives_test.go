package edge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestVerifyPassword(t *testing.T) {
	bc, err := bcrypt.GenerateFromPassword([]byte("s3cret-Pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	bcY := "$2y$" + string(bc)[4:]
	tests := []struct {
		name, hash, password string
		want                 bool
	}{
		{"apr1", "$apr1$Q0VhdM3G$Z797o9kqseSZMgOLfRlWq0", "s3cret-Pass", true},
		{"apr1 wrong", "$apr1$Q0VhdM3G$Z797o9kqseSZMgOLfRlWq0", "s3cret-pass", false},
		{"md5-crypt", "$1$abcdefgh$7oCutnXWNzbgYX1P2k7Rl.", "s3cret-Pass", true},
		{"md5-crypt wrong", "$1$abcdefgh$7oCutnXWNzbgYX1P2k7Rl.", "", false},
		{"apr1 long password", "$apr1$xY12$Q0dZlJUB8zeydyjVS/nl5/", "a-much-longer-password-than-sixteen-bytes", true},
		{"bcrypt 2a", string(bc), "s3cret-Pass", true},
		{"bcrypt 2y", bcY, "s3cret-Pass", true},
		{"bcrypt 2y wrong", bcY, "s3cret-Pass!", false},
		{"sha", "{SHA}" + "W6ph5Mm5Pz8GgiULbPgzG37mj9g=", "password", true},
		{"ssha", "{SSHA}" + "2fmrMeeIdE4mDxkZLlnd4RwnlBZzYWx0", "secret", false},
		{"plain", "{PLAIN}pw", "pw", true},
		{"unknown scheme", "pw", "pw", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyPassword(tt.hash, tt.password); got != tt.want {
				t.Fatalf("verifyPassword(%q, %q) = %v, want %v", tt.hash, tt.password, got, tt.want)
			}
		})
	}
}

func TestBasicAuthCheckAndCache(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "htpasswd")
	os.WriteFile(file, []byte("# users\n\nalice:$apr1$Q0VhdM3G$Z797o9kqseSZMgOLfRlWq0\nbob:{PLAIN}x:comment\n"), 0o644)
	users, err := loadHtpasswd(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[1].PasswordHash != "{PLAIN}x" {
		t.Fatalf("users = %+v", users)
	}
	b := newBasicAuth("", users)
	req := func(u, p string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth(u, p)
		return r
	}
	if u, ok := b.check(req("alice", "s3cret-Pass")); !ok || u != "alice" {
		t.Fatal("valid credentials rejected")
	}
	if len(b.cache) != 1 {
		t.Fatalf("cache size = %d, want 1", len(b.cache))
	}
	if _, ok := b.check(req("alice", "s3cret-Pass")); !ok {
		t.Fatal("cached credentials rejected")
	}
	for _, c := range [][2]string{{"alice", "nope"}, {"mallory", "s3cret-Pass"}, {"bob", "y"}} {
		if _, ok := b.check(req(c[0], c[1])); ok {
			t.Fatalf("%v accepted", c)
		}
	}
	if _, ok := b.check(httptest.NewRequest("GET", "/", nil)); ok {
		t.Fatal("missing credentials accepted")
	}
	if b.challenge != `Basic realm="Restricted", charset="UTF-8"` {
		t.Fatalf("challenge = %q", b.challenge)
	}
}

func TestIPMatcher(t *testing.T) {
	m, err := compileIPRules([]IPRule{{Allow: false, CIDR: "10.0.0.5"}, {Allow: true, CIDR: "10.0.0.0/8"}, {Allow: true, CIDR: "2001:db8::/32"}, {Allow: false, CIDR: "all"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		addr           string
		allow, matched bool
	}{
		{"10.0.0.5", false, true},
		{"10.9.9.9", true, true},
		{"::ffff:10.1.1.1", true, true},
		{"2001:db8::1", true, true},
		{"192.168.1.1", false, true},
	}
	for _, tt := range tests {
		allow, matched := m.decide(netip.MustParseAddr(tt.addr))
		if allow != tt.allow || matched != tt.matched {
			t.Errorf("%s: allow=%v matched=%v", tt.addr, allow, matched)
		}
	}
	noAll, _ := compileIPRules([]IPRule{{Allow: false, CIDR: "10.0.0.0/8"}})
	if allow, matched := noAll.decide(netip.MustParseAddr("192.0.2.1")); !allow || matched {
		t.Error("an address matching no rule must be allowed without a match")
	}
	if _, err := compileIPRules([]IPRule{{CIDR: "10.0.0.0/33"}}); err == nil {
		t.Error("invalid prefix accepted")
	}
}

func TestPrefixMapLongestMatch(t *testing.T) {
	m := newPrefixMap[bool]()
	for cidr, v := range map[string]bool{"10.0.0.0/8": true, "10.1.0.0/16": false, "10.1.2.3": true, "2001:db8::/32": true} {
		p, err := parsePrefix(cidr)
		if err != nil {
			t.Fatal(err)
		}
		m.add(p, v)
	}
	tests := []struct {
		addr   string
		v, hit bool
	}{
		{"10.2.3.4", true, true},
		{"10.1.9.9", false, true},
		{"10.1.2.3", true, true},
		{"::ffff:10.1.9.9", false, true},
		{"2001:db8:1::1", true, true},
		{"192.168.0.1", false, false},
	}
	for _, tt := range tests {
		v, hit := m.lookup(netip.MustParseAddr(tt.addr))
		if v != tt.v || hit != tt.hit {
			t.Errorf("%s: got %v/%v, want %v/%v", tt.addr, v, hit, tt.v, tt.hit)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(2, 3) // 2 r/s, burst 3 → 4 immediately
	a := netip.MustParseAddr("192.0.2.1")
	now := time.Unix(1_000_000, 0)
	for i := range 4 {
		if !l.allow(a, now) {
			t.Fatalf("request %d within burst rejected", i+1)
		}
	}
	if l.allow(a, now) {
		t.Fatal("request beyond burst allowed")
	}
	if !l.allow(netip.MustParseAddr("192.0.2.2"), now) {
		t.Fatal("other client limited")
	}
	now = now.Add(500 * time.Millisecond) // one token back
	if !l.allow(a, now) || l.allow(a, now) {
		t.Fatal("refill should allow exactly one request after 0.5 s")
	}
	now = now.Add(time.Hour) // refill is capped at burst+1
	for i := range 4 {
		if !l.allow(a, now) {
			t.Fatalf("request %d after long idle rejected", i+1)
		}
	}
	if l.allow(a, now) {
		t.Fatal("tokens exceeded burst+1 after idle")
	}

	// Eviction: idle, fully refilled buckets disappear on sweep.
	for i := range 200 {
		l.allow(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), now)
	}
	if l.size() < 200 {
		t.Fatalf("size = %d", l.size())
	}
	later := now.Add(2 * time.Minute)
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.Lock()
		l.sweep(s, later)
		s.mu.Unlock()
	}
	if n := l.size(); n != 0 {
		t.Fatalf("size after sweep = %d, want 0", n)
	}

	// Shard cap: a flood of addresses never grows a shard past the cap.
	f := newRateLimiter(1, 0)
	for i := range maxKeysPerShard * 40 {
		f.allow(netip.AddrFrom4([4]byte{11, byte(i >> 16), byte(i >> 8), byte(i)}), now)
	}
	for i := range f.shards {
		if n := len(f.shards[i].buckets); n > maxKeysPerShard {
			t.Fatalf("shard %d has %d keys", i, n)
		}
	}
}

func TestHostLimiterExempt(t *testing.T) {
	cfg := &RateLimit{RequestsPerSecond: 1, Burst: 0, Exempt: []ExemptRule{{CIDR: "10.0.0.0/8", Exempt: true}, {CIDR: "10.1.0.0/16", Exempt: false}}}
	for _, def := range []bool{false, true} {
		cfg.ExemptDefault = def
		c := &compiler{cfg: &Config{}, rt: &runtime{limiters: map[string]*rateLimiter{}, transports: map[transportKey]*upstreamTransport{}}}
		hr := c.host(&Host{ID: "h", RateLimit: cfg}, "host h")
		if len(c.errs) > 0 {
			t.Fatal(c.errs)
		}
		now := time.Now()
		limited := func(addr string) bool {
			a := netip.MustParseAddr(addr)
			return !hr.limiter.allow(a, now) || !hr.limiter.allow(a, now)
		}
		if limited("10.2.0.1") {
			t.Error("exempt /8 limited")
		}
		if !limited("10.1.0.1") {
			t.Error("more specific non-exempt /16 not limited")
		}
		if got := limited("192.0.2.1"); got == def {
			t.Errorf("default exempt=%v: limited=%v", def, got)
		}
	}
}

func TestIsExploit(t *testing.T) {
	tests := []struct {
		uri, query, ua string
		want           bool
	}{
		{"/index.html", "", "Mozilla/5.0", false},
		{"/a/../etc", "", "", true},
		{"/a/..%2Fb", "", "", true},
		{"/%2E%2e/", "", "", true},
		{"/x%00", "", "", true},
		{"/.env", "", "", true},
		{"/.GIT/config", "", "", true},
		{"/cgi-bin/test.sh", "", "", true},
		{"/cgi-bin/test.php", "", "", false},
		{"/?q=%3Cscript%3Ealert(1)%3C/script%3E", "q=%3Cscript%3Ealert(1)%3C/script%3E", "", true},
		{"/?id=1 UNION ALL SELECT 1", "id=1 UNION ALL SELECT 1", "", true},
		{"/?q=reunionselection", "q=reunionselection", "", false},
		{"/?x=base64_decode(", "x=base64_decode(", "", true},
		{"/?GLOBALS[x]=1", "GLOBALS[x]=1", "", true},
		{"/", "", "sqlmap/1.7", true},
		{"/", "", "Nmap Scripting Engine", true},
	}
	for _, tt := range tests {
		if got := isExploit(tt.uri, tt.query, tt.ua); got != tt.want {
			t.Errorf("isExploit(%q, %q, %q) = %v, want %v", tt.uri, tt.query, tt.ua, got, tt.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"/", "/", true},
		{"/a/b", "/a/b", true},
		{"//a///b//", "/a/b/", true},
		{"/a/./b/.", "/a/b/", true},
		{"/a/b/../c", "/a/c", true},
		{"/a/..", "/", true},
		{"/%61pp/x%20y", "/app/x y", true},
		{"/a%2Fb", "/a/b", true},
		{"/..", "", false},
		{"/a/../../b", "", false},
		{"/%zz", "", false},
		{"/%2", "", false},
		{"/a%00", "", false},
		{"*", "", false},
	}
	for _, tt := range tests {
		got, ok := normalizePath(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("normalizePath(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRequestHostAndSplitURI(t *testing.T) {
	for in, want := range map[string]string{"Example.COM:8080": "example.com", "example.com": "example.com", "[::1]:443": "[::1]", "[::1]": "[::1]", "": ""} {
		if got := requestHost(in); got != want {
			t.Errorf("requestHost(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]string{"/a?b=1&c": {"/a", "b=1&c"}, "/a": {"/a", ""}, "http://h.test/x?y": {"/x", "y"}, "http://h.test": {"/", ""}, "http://h.test?q": {"/", "q"}} {
		p, q := splitRequestURI(in)
		if p != want[0] || q != want[1] {
			t.Errorf("splitRequestURI(%q) = %q, %q", in, p, q)
		}
	}
}

func TestServerSetLookup(t *testing.T) {
	s := newServerSet()
	a, b, c, d, def := &vserver{name: "a"}, &vserver{name: "b"}, &vserver{name: "c"}, &vserver{name: "d"}, &vserver{name: "_"}
	s.addAll([]string{"a.test", "*.wild.test", "[::1]"}, a)
	s.addAll([]string{"*.x.wild.test"}, b)
	s.addAll([]string{"A.test", "c.test"}, c) // a.test is taken: first wins
	s.addAll([]string{"exact.x.wild.test"}, d)
	s.def = def
	tests := map[string]*vserver{
		"a.test": a, "c.test": c, "y.wild.test": a, "deep.y.wild.test": a, "x.wild.test": a,
		"k.x.wild.test": b, "exact.x.wild.test": d, "wild.test": def, "unknown": def, "": def, "::1": a,
	}
	for name, want := range tests {
		if got := s.lookup(name); got != want {
			t.Errorf("lookup(%q) = %s, want %s", name, got.name, want.name)
		}
	}
}

func TestTemplates(t *testing.T) {
	for _, bad := range []string{"$nope", "x-$foo_bar", "${host", "$", "a$ b", "$ssl_client_cert"} {
		if _, err := compileTemplate(bad); err == nil {
			t.Errorf("compileTemplate(%q) accepted", bad)
		}
	}
	r := httptest.NewRequest("GET", "http://Example.test:8080/p/a%20b?x=1&Y=2", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	r.Header.Set("X-Foo-Bar", "fb")
	r.Header.Add("X-Forwarded-For", "1.1.1.1")
	r.AddCookie(&http.Cookie{Name: "Sess", Value: "abc"})
	rs := &reqState{r: r, role: &listenerRole{scheme: "https", port: 8443, portStr: "8443"}, requestID: "0123456789abcdef0123456789abcdef"}
	rs.splitRemote()
	rs.host = requestHost(r.Host)
	rs.rawPath, rs.rawQuery = splitRequestURI(r.RequestURI)
	rs.hasArgs = true
	rs.path, _ = normalizePath(rs.rawPath)
	tests := map[string]string{
		"static":                          "static",
		"$host":                           "example.test",
		"${http_host}!":                   "Example.test:8080!",
		"$remote_addr:$remote_port":       "192.0.2.7:5555",
		"$scheme://$host$request_uri":     "https://example.test/p/a%20b?x=1&Y=2",
		"$uri|$args|$is_args":             "/p/a b|x=1&Y=2|?",
		"$server_port $request_method":    "8443 GET",
		"$request_id":                     "0123456789abcdef0123456789abcdef",
		"$http_x_foo_bar/$HTTP_X_FOO_BAR": "fb/fb",
		"$proxy_add_x_forwarded_for":      "1.1.1.1, 192.0.2.7",
		"$cookie_Sess $arg_y $arg_x":      "abc 2 1",
		"$https$1":                        "on",
	}
	for in, want := range tests {
		tmpl, err := compileTemplate(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := tmpl.expand(rs); got != want {
			t.Errorf("expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocationMatchAndRewrite(t *testing.T) {
	h := &hostRT{locations: []*locationRT{
		{path: "/app/admin/", deny: true},
		{path: "/app/", prefix: "/app", strip: true},
		{path: "/exact/"},
		{path: "/exact"},
		{path: "/denied/", deny: true},
		{path: "/"},
	}}
	tests := []struct {
		path       string
		loc, slash string
	}{
		{"/app/x", "/app/", ""},
		{"/app/admin/y", "/app/admin/", ""},
		{"/app", "", "/app/"},
		{"/exact", "/exact", ""},
		{"/denied", "/", ""},
		{"/other", "/", ""},
	}
	for _, tt := range tests {
		loc, slash := h.match(tt.path)
		gotLoc, gotSlash := "", ""
		if loc != nil {
			gotLoc = loc.path
		}
		if slash != nil {
			gotSlash = slash.path
		}
		if gotLoc != tt.loc || gotSlash != tt.slash {
			t.Errorf("match(%q) = %q/%q, want %q/%q", tt.path, gotLoc, gotSlash, tt.loc, tt.slash)
		}
	}
	rw := []struct {
		loc  locationRT
		path string
		want string
		ok   bool
	}{
		{locationRT{path: "/app/", prefix: "/app", strip: true}, "/app/x/y", "/x/y", true},
		{locationRT{path: "/app/", prefix: "/app", strip: true, up: upstreamRT{base: "/base"}}, "/app/", "/base/", true},
		{locationRT{path: "/app", prefix: "/app", strip: true}, "/apple", "/le", true},
		{locationRT{path: "/", prefix: "", strip: true}, "/x", "", false},
		{locationRT{path: "/g/", up: upstreamRT{base: "/grafana"}}, "/g/x", "/grafana/g/x", true},
		{locationRT{path: "/"}, "/x", "", false},
	}
	for _, tt := range rw {
		got, ok := tt.loc.rewrite(tt.path)
		if got != tt.want || ok != tt.ok {
			t.Errorf("rewrite(%q on %q) = %q, %v", tt.path, tt.loc.path, got, ok)
		}
	}
}

func TestStreamTarget(t *testing.T) {
	one := &streamRT{forwardHost: "::1", listenLo: 5000, forwardLo: 6000, forwardHi: 6000}
	shift := &streamRT{forwardHost: "db.internal", listenLo: 5000, forwardLo: 7000, forwardHi: 7010}
	if got := one.target(5005); got != "[::1]:6000" {
		t.Errorf("single port target = %s", got)
	}
	if got := shift.target(5003); got != "db.internal:7003" {
		t.Errorf("offset target = %s", got)
	}
}

func TestCacheTTL(t *testing.T) {
	now := time.Now()
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			out.Add(kv[i], kv[i+1])
		}
		return out
	}
	tests := []struct {
		name string
		code int
		h    http.Header
		ttl  time.Duration
		ok   bool
	}{
		{"default", 200, h(), cacheValid, true},
		{"redirect", 302, h(), cacheValid, true},
		{"404", 404, h(), 0, false},
		{"private", 200, h("Cache-Control", "public, private"), 0, false},
		{"no-store", 200, h("Cache-Control", "no-store"), 0, false},
		{"set-cookie", 200, h("Set-Cookie", "a=b"), 0, false},
		{"vary star", 200, h("Vary", "*"), 0, false},
		{"max-age", 200, h("Cache-Control", "max-age=60"), time.Minute, true},
		{"s-maxage wins", 200, h("Cache-Control", "s-maxage=10, max-age=60"), 10 * time.Second, true},
		{"max-age 0", 200, h("Cache-Control", "max-age=0"), 0, false},
		{"expired", 200, h("Expires", now.Add(-time.Hour).UTC().Format(http.TimeFormat)), 0, false},
		{"too large", 200, h("Content-Length", "20000000"), 0, false},
	}
	for _, tt := range tests {
		ttl, ok := cacheTTL(tt.code, tt.h, now)
		if ok != tt.ok || (ok && ttl != tt.ttl) {
			t.Errorf("%s: ttl=%v ok=%v", tt.name, ttl, ok)
		}
	}
}

func TestAcceptsGzipAndStaticAsset(t *testing.T) {
	for v, want := range map[string]bool{"gzip": true, "br, gzip;q=0.5": true, "gzip;q=0": false, "deflate": false, "": false, "GZIP": true} {
		if got := acceptsGzip([]string{v}); got != want {
			t.Errorf("acceptsGzip(%q) = %v", v, got)
		}
	}
	for p, want := range map[string]bool{"/a.JS": true, "/x/y.woff2": true, "/a.json": false, "/css": false, "/a.": false, "/a.jpeg": true} {
		if got := isStaticAsset(p); got != want {
			t.Errorf("isStaticAsset(%q) = %v", p, got)
		}
	}
}

func TestJSONString(t *testing.T) {
	in := "a\"b\\c\n\x01\xff€"
	out := appendJSONString(nil, in)
	var back string
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("invalid JSON %s: %v", out, err)
	}
	if want := strings.ToValidUTF8(in, "�"); back != want {
		t.Fatalf("round trip = %q, want %q", back, want)
	}
	if got := string(appendSeconds(nil, 1234567*time.Microsecond)); got != "1.234" {
		t.Fatalf("appendSeconds = %s", got)
	}
	if got := string(appendMsec(nil, time.UnixMilli(1726400000123))); got != "1726400000.123" {
		t.Fatalf("appendMsec = %s", got)
	}
}
