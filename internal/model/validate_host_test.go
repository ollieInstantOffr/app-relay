package model

import (
	"errors"
	"strings"
	"testing"
)

func fieldErrs(t *testing.T, err error) map[string]string {
	t.Helper()
	if err == nil {
		return map[string]string{}
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T %v", err, err)
	}
	return ve.Fields
}

func validHost() ProxyHost {
	return ProxyHost{
		Domains:  []string{"grafana.home.lan"},
		Enabled:  true,
		Upstream: Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000},
		HSTS:     "inherit",
		Source:   SourceManual,
		Locations: []Location{
			{ID: "a", Path: "/office/", Kind: LocationProxy, Upstream: Upstream{Scheme: "http", Host: "10.0.0.36", Port: 9980},
				Headers: []Header{{Name: "X-Forwarded-Prefix", Value: "/office"}}},
			{ID: "b", Path: "/.well-known/", Kind: LocationSame, NoAuth: true},
			{ID: "c", Path: "/metrics", Kind: LocationDeny},
		},
		MaxBodySize: "10g",
		CustomNginx: `add_header X-Robots-Tag "noindex" always;
proxy_hide_header X-Powered-By;`,
	}
}

func TestHostDomainError(t *testing.T) {
	ok := []string{"grafana.home.lan", "*.home.lan", "home.lan", "nas", "xn--bcher-kva.example", "10.0.0.1", "a-b.c-d.lan", "*.lan"}
	for _, d := range ok {
		if msg := HostDomainError(d); msg != "" {
			t.Errorf("%q: unexpected error %q", d, msg)
		}
	}
	bad := map[string]string{
		"":                               "required",
		"Grafana.home.lan":               "lowercase",
		"foo..bar":                       "empty label",
		"-a.lan":                         "not a valid",
		"a-.lan":                         "not a valid",
		"*.*.lan":                        "first label",
		"foo.*.lan":                      "first label",
		"http://x.lan":                   "no scheme",
		"x.lan:80":                       "no scheme",
		"x.lan/path":                     "no scheme",
		"under_score.lan":                "not a valid",
		"*.":                             "after *.",
		"a b.lan":                        "spaces",
		strings.Repeat("a", 64) + ".lan": "63",
	}
	for d, want := range bad {
		msg := HostDomainError(d)
		if msg == "" || !strings.Contains(msg, want) {
			t.Errorf("%q: got %q, want message containing %q", d, msg, want)
		}
	}
}

func TestHostUpstreamHostError(t *testing.T) {
	for _, h := range []string{"10.0.0.21", "::1", "[::1]", "grafana", "my_container", "host.docker.internal", "fe80::1"} {
		if msg := HostUpstreamHostError(h); msg != "" {
			t.Errorf("%q: unexpected %q", h, msg)
		}
	}
	bad := map[string]string{
		"10.0.0.300": "Not a valid IPv4 address",
		"1.2.3":      "Not a valid IPv4 address",
		"fe80:::1":   "Not a valid IPv6 address",
		"[10.0.0.1]": "Not a valid IPv6 address",
		"http://x":   "host only",
		"a b":        "Not a valid host",
		"":           "required",
		"-bad":       "Not a valid host name",
	}
	for h, want := range bad {
		if msg := HostUpstreamHostError(h); !strings.Contains(msg, want) {
			t.Errorf("%q: got %q, want %q", h, msg, want)
		}
	}
}

func TestProxyHostValid(t *testing.T) {
	h := validHost()
	h.Normalize()
	if err := h.Validate(); err != nil {
		t.Fatalf("valid host rejected: %v", err)
	}
}

func TestProxyHostFieldPaths(t *testing.T) {
	h := validHost()
	h.Domains = []string{"ok.home.lan", "bad..lan"}
	h.Upstream.Host = "10.0.0.300"
	h.Upstream.Port = 70000
	h.Locations = append(h.Locations,
		Location{ID: "d", Path: "/office/", Kind: LocationProxy, Upstream: Upstream{Scheme: "http", Host: "x", Port: 1}},
		Location{ID: "e", Path: "/", Kind: LocationSame},
		Location{ID: "f", Path: "nope", Kind: "weird"},
	)
	h.Locations[0].Headers = append(h.Locations[0].Headers, Header{Name: "Bad Header", Value: "a\nb"})
	h.RateLimit = RateLimit{Enabled: true, RequestsPerSecond: 0, Burst: -1}
	h.ForwardAuth = ForwardAuth{Enabled: true, Provider: "authelia", VerifyURL: "10.0.0.5:9091"}
	h.GeoBlock = GeoBlock{Enabled: true, AllowCountries: []string{"DE", "Germany"}}
	h.MaxBodySize = "10 bananas"
	h.ProxyReadTimeout = -5
	h.CustomNginx = "server { listen 80; }"
	h.HSTS = "sometimes"
	h.CipherProfile = "legacy"

	f := fieldErrs(t, h.Validate())
	want := []string{
		"domains.1", "upstream.host", "upstream.port",
		"locations.3.path", "locations.4.path", "locations.5.path", "locations.5.kind",
		"locations.0.headers.1.name", "locations.0.headers.1.value",
		"rateLimit.requestsPerSecond", "rateLimit.burst",
		"forwardAuth.verifyUrl", "geoBlock.allowCountries.1",
		"maxBodySize", "proxyReadTimeout", "customNginx", "hsts", "cipherProfile",
	}
	for _, k := range want {
		if _, ok := f[k]; !ok {
			t.Errorf("missing field error %s (got %v)", k, f)
		}
	}
	if _, ok := f["domains.0"]; ok {
		t.Errorf("domains.0 should be valid: %v", f["domains.0"])
	}
	if !strings.Contains(f["upstream.host"], "IPv4") {
		t.Errorf("upstream.host message: %q", f["upstream.host"])
	}
}

func TestProxyHostEmpty(t *testing.T) {
	h := ProxyHost{}
	h.Normalize()
	f := fieldErrs(t, h.Validate())
	for _, k := range []string{"domains", "upstream.host", "upstream.port"} {
		if _, ok := f[k]; !ok {
			t.Errorf("missing %s: %v", k, f)
		}
	}
	if len(f) != 3 {
		t.Errorf("unexpected extra errors: %v", f)
	}
}

func TestProxyHostNormalize(t *testing.T) {
	h := ProxyHost{
		Domains:     []string{" Grafana.Home.LAN. ", "grafana.home.lan", ""},
		Upstream:    Upstream{Scheme: "HTTP", Host: " 10.0.0.1 ", Port: 80},
		ForceHTTPS:  true,
		Locations:   []Location{{Path: " /api/ ", Headers: []Header{{Name: " ", Value: ""}, {Name: " X-A ", Value: "1"}}}},
		GeoBlock:    GeoBlock{AllowCountries: []string{"de", "DE", " at "}},
		MaxBodySize: "10G",
		CustomNginx: "  \n ",
	}
	h.Normalize()
	if len(h.Domains) != 1 || h.Domains[0] != "grafana.home.lan" {
		t.Errorf("domains: %#v", h.Domains)
	}
	if h.Upstream.Scheme != "http" || h.Upstream.Host != "10.0.0.1" {
		t.Errorf("upstream: %#v", h.Upstream)
	}
	if h.HSTS != "inherit" || h.Source != SourceManual {
		t.Errorf("defaults: hsts=%q source=%q", h.HSTS, h.Source)
	}
	if h.ForceHTTPS {
		t.Error("forceHttps must be cleared without certificate")
	}
	l := h.Locations[0]
	if l.ID == "" || l.Path != "/api/" || l.Kind != LocationProxy || len(l.Headers) != 1 || l.Headers[0].Name != "X-A" {
		t.Errorf("location: %#v", l)
	}
	if strings.Join(h.GeoBlock.AllowCountries, ",") != "DE,AT" {
		t.Errorf("countries: %v", h.GeoBlock.AllowCountries)
	}
	if h.MaxBodySize != "10g" || h.CustomNginx != "" {
		t.Errorf("maxBodySize=%q customNginx=%q", h.MaxBodySize, h.CustomNginx)
	}
	var nilHost ProxyHost
	nilHost.Normalize()
	if nilHost.Locations == nil || nilHost.Domains == nil || nilHost.GeoBlock.AllowCountries == nil {
		t.Error("slices must be non-nil after Normalize")
	}
}

func TestHostSnippetError(t *testing.T) {
	ok := []string{
		"",
		`add_header X "{";`,
		"location /server { return 404; }",
		"# } stray brace in comment\nproxy_hide_header X-Powered-By;",
		"if ($http_host = x) { return 403; }",
		`add_header Content-Security-Policy "default-src 'self'";`,
	}
	for _, s := range ok {
		if msg := HostSnippetError(s); msg != "" {
			t.Errorf("%q: unexpected %q", s, msg)
		}
	}
	bad := map[string]string{
		"server { listen 81; }":      "server",
		"location / {\n http{}\n}":   "http",
		"location / {":               "missing }",
		"}":                          "unexpected }",
		`add_header X "open;`:        "Unterminated",
		"upstream foo { server a; }": "upstream",
	}
	for s, want := range bad {
		if msg := HostSnippetError(s); !strings.Contains(msg, want) {
			t.Errorf("%q: got %q want %q", s, msg, want)
		}
	}
}

func TestRedirectValidate(t *testing.T) {
	v := Redirect{Domains: []string{"WWW.example.com"}, FromPath: "/", To: "https://example.com", KeepPath: true, Enabled: true}
	v.Normalize()
	if v.Code != 301 || v.FromPath != "" || v.Domains[0] != "www.example.com" {
		t.Fatalf("normalize: %#v", v)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("valid redirect rejected: %v", err)
	}

	bad := Redirect{Domains: []string{"wiki.home.lan"}, FromPath: "old", To: "docs.home.lan/wiki", Code: 303, ForceHTTPS: true}
	f := fieldErrs(t, bad.Validate())
	for _, k := range []string{"fromPath", "to", "code", "forceHttps"} {
		if _, ok := f[k]; !ok {
			t.Errorf("missing %s: %v", k, f)
		}
	}

	loop := Redirect{Domains: []string{"home.lan"}, To: "https://home.lan/", Code: 308}
	if f := fieldErrs(t, loop.Validate()); !strings.Contains(f["to"], "itself") {
		t.Errorf("loop not detected: %v", f)
	}
	pathLoop := Redirect{Domains: []string{"home.lan"}, FromPath: "/old-blog", To: "https://home.lan/blog", Code: 308}
	if err := pathLoop.Validate(); err != nil {
		t.Errorf("path redirect on same domain rejected: %v", err)
	}
}

func TestDefaultHostSettingsValidate(t *testing.T) {
	d := DefaultHostSettings{}
	d.Normalize()
	if d.Action != DefaultHostClose || d.Validate() != nil {
		t.Fatalf("empty settings should default to close: %#v", d)
	}
	cases := map[string]DefaultHostSettings{
		"redirectTo": {Action: "redirect", RedirectTo: "home.lan"},
		"hostId":     {Action: "host"},
		"action":     {Action: "teapot"},
	}
	for field, s := range cases {
		if _, ok := fieldErrs(t, s.Validate())[field]; !ok {
			t.Errorf("%s: expected error for %#v", field, s)
		}
	}
	if err := (&DefaultHostSettings{Action: "redirect", RedirectTo: "https://home.lan"}).Validate(); err != nil {
		t.Errorf("valid redirect default rejected: %v", err)
	}
}
