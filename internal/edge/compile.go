package edge

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Load reads and decodes edge.json. Unknown fields are ignored so a newer
// renderer can add optional fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Schema != 1 {
		return nil, fmt.Errorf("%s: unsupported schema %d (expected 1)", path, cfg.Schema)
	}
	return &cfg, nil
}

// release is a config file resolved through the `current` symlink.
type release struct {
	path    string // resolved edge.json
	baseDir string // relative paths resolve here
	hash    string // release directory name
}

// resolveRelease resolves path (edge.json or its directory) through symlinks,
// so the config and its relative files come from one release directory even
// if `current` flips while loading.
func resolveRelease(path string) (release, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return release{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return release{}, err
	}
	if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
		resolved = filepath.Join(resolved, "edge.json")
	}
	dir := filepath.Dir(resolved)
	return release{path: resolved, baseDir: dir, hash: filepath.Base(dir)}, nil
}

// ---------------------------------------------------------------- runtime

// runtime is one compiled, immutable configuration. The server swaps it
// atomically; in-flight requests keep the runtime they started with.
type runtime struct {
	cfg        *Config
	hash       string
	http       serverSet
	https      serverSet
	blocklist  *prefixMap[struct{}]
	geo        *geoStore // nil without geoipDatabase
	acmeRoot   string
	pages      map[int][]byte // custom error pages
	httpsPort  int
	cacheBytes int64
	listeners  []listenSpec

	certs      map[string]*certEntry
	limiters   map[string]*rateLimiter
	basics     map[string]*basicAuth
	transports map[transportKey]*upstreamTransport
}

type vkind uint8

const (
	kindHost          vkind = iota // proxy host
	kindHTTPSRedirect              // ForceHTTPS on the HTTP port
	kindGroup                      // redirect group
	kindDefault                    // default server
)

// vserver is one nginx server block as seen from one listener.
type vserver struct {
	kind    vkind
	name    string // $server_name
	hostID  string // access log host_id
	host    *hostRT
	group   *groupRT
	acme    bool        // answers ACME challenges
	tlsConf *tls.Config // handshake config (HTTPS listener only)
	headers []headerKV  // add_header … always
	metrics *hostMetrics

	action     string // default server
	redirectTo *template
}

type headerKV struct {
	name  string
	value []string // len == cap: safe to share with response header maps
}

type hostRT struct {
	id            string
	blockExploits bool
	maxBody       int64
	countries     map[string]bool // geo-blocking: allowed countries (nil = off)
	limiter       *hostLimiter
	fa            *forwardAuthRT
	maint         *maintenanceRT
	locations     []*locationRT // longest path first
	pathRedirects []pathRedirectRT
}

type locationRT struct {
	path        string
	prefix      string // path without trailing slash (strip prefix)
	deny        bool
	up          upstreamRT
	strip       bool
	websockets  bool
	cache       bool
	headers     []headerRT
	access      *accessRT
	denyAll     bool
	forwardAuth bool
	skipMaint   bool
}

type upstreamRT struct {
	scheme    string
	port      int
	hostPort  string // net.JoinHostPort form
	proxyHost string // $proxy_host
	base      string // base path without trailing slash
	transport *upstreamTransport
}

type headerRT struct {
	name string // canonical
	tmpl *template
	host bool // the Host header
}

type accessRT struct {
	rules      *ipMatcher
	basic      *basicAuth
	satisfyAny bool
}

// Redirect targets may contain nginx variables, like `return 301 <url>`.
type pathRedirectRT struct {
	from, fromSlash string
	to              *template
	code            int
	keep            bool
}

type wholeRedirectRT struct {
	to   *template
	code int
	keep bool
}

type groupRT struct {
	paths []pathRedirectRT
	whole *wholeRedirectRT
}

// ---------------------------------------------------------------- compiler

type compileEnv struct {
	certs    *certStore
	geo      *geoStore
	prev     *runtime
	metrics  *metrics
	bindHost string // HTTP/HTTPS/QUIC interface ("" = all)
}

type compiler struct {
	cfg     *Config
	baseDir string
	env     compileEnv
	rt      *runtime
	lists   map[string]*accessRT
	errs    []error
}

func (c *compiler) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf(format, args...))
}

// compile validates cfg and builds a runtime. Everything `run` needs is
// loaded here (certificates, htpasswd files), so a successful compile only
// leaves binding the listeners.
func compile(cfg *Config, baseDir string, env compileEnv) (*runtime, error) {
	if env.metrics == nil {
		env.metrics = newMetrics()
	}
	if env.certs == nil {
		cs, err := newCertStore()
		if err != nil {
			return nil, err
		}
		env.certs = cs
	}
	if env.geo == nil {
		env.geo = newGeoStore()
	}
	c := &compiler{cfg: cfg, baseDir: baseDir, env: env, lists: map[string]*accessRT{}, rt: &runtime{
		cfg:        cfg,
		limiters:   map[string]*rateLimiter{},
		basics:     map[string]*basicAuth{},
		transports: map[transportKey]*upstreamTransport{},
		http:       newServerSet(),
		https:      newServerSet(),
	}}
	c.globals()
	c.prepareCerts()
	c.accessLists()
	c.servers()
	c.listeners()
	if len(c.errs) > 0 {
		return nil, errors.Join(c.errs...)
	}
	return c.rt, nil
}

func (c *compiler) path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.baseDir, p)
}

func validPort(p int) bool { return p >= 0 && p <= 65535 }

func (c *compiler) globals() {
	cfg := c.cfg
	if !validPort(cfg.HTTPPort) || !validPort(cfg.HTTPSPort) {
		c.fail("invalid HTTP/HTTPS port %d/%d", cfg.HTTPPort, cfg.HTTPSPort)
	}
	if cfg.HTTPPort > 0 && cfg.HTTPPort == cfg.HTTPSPort {
		c.fail("HTTP and HTTPS both use port %d", cfg.HTTPPort)
	}
	c.rt.httpsPort = cfg.HTTPSPort
	if _, err := profileFor(cfg.TLSProfile); err != nil {
		c.fail("tlsProfile: %v", err)
	}
	if cfg.AssetCacheMB < 0 {
		c.fail("assetCacheMb must not be negative")
	}
	c.rt.cacheBytes = int64(cfg.AssetCacheMB) << 20
	if cfg.AssetCacheMB == 0 {
		c.rt.cacheBytes = 256 << 20
	}
	c.rt.acmeRoot = c.path(cfg.ACMEWebroot)
	c.rt.blocklist = newPrefixMap[struct{}]()
	c.rt.pages = map[int][]byte{}
	for code, page := range cfg.ErrorPages {
		n, err := strconv.Atoi(code)
		if err != nil || n < 400 || n > 599 {
			c.fail("errorPages: %q is not an HTTP error status", code)
			continue
		}
		c.rt.pages[n] = []byte(page)
	}
	for _, e := range cfg.Blocklist {
		p, err := parsePrefix(e)
		if err != nil {
			c.fail("blocklist: %v", err)
			continue
		}
		c.rt.blocklist.add(p, struct{}{})
	}
	if cfg.GeoIPDatabase != "" {
		if err := c.env.geo.load(c.path(cfg.GeoIPDatabase)); err != nil {
			c.fail("%v", err)
		} else {
			c.rt.geo = c.env.geo
		}
	}
}

func (c *compiler) resolveCert(ref *CertRef) *CertRef {
	if ref == nil {
		return nil
	}
	r := *ref
	r.CertFile, r.KeyFile = c.path(r.CertFile), c.path(r.KeyFile)
	if r.CertFile == "" || r.KeyFile == "" {
		c.fail("certificate %s: certFile and keyFile are required", r.ID)
		return nil
	}
	return &r
}

func (c *compiler) prepareCerts() {
	var keys []string
	refs := map[string]CertRef{}
	add := func(ref *CertRef) {
		r := c.resolveCert(ref)
		if r == nil {
			return
		}
		k := certKey(*r)
		if old, dup := refs[k]; dup {
			old.OCSPStapling = old.OCSPStapling || r.OCSPStapling
			refs[k] = old
			return
		}
		keys = append(keys, k)
		refs[k] = *r
	}
	for i := range c.cfg.Hosts {
		add(c.cfg.Hosts[i].Cert)
	}
	for i := range c.cfg.Redirects {
		add(c.cfg.Redirects[i].Cert)
	}
	add(c.cfg.Default.Cert)
	// Load each pair on its own so `check` reports every broken certificate.
	entries := map[string]*certEntry{}
	for _, k := range keys {
		m, err := c.env.certs.prepare([]CertRef{refs[k]})
		if err != nil {
			c.fail("%v", err)
			continue
		}
		entries[k] = m[k]
	}
	c.rt.certs = entries
}

// certFunc returns the certificate getter for ref (placeholder when nil).
func (c *compiler) certFunc(ref *CertRef) func() *tls.Certificate {
	if ref != nil {
		r := *ref
		r.CertFile, r.KeyFile = c.path(r.CertFile), c.path(r.KeyFile)
		if e := c.rt.certs[certKey(r)]; e != nil {
			return e.cert
		}
	}
	placeholder := c.env.certs.placeholder
	return func() *tls.Certificate { return placeholder }
}

func (c *compiler) accessLists() {
	ids := make([]string, 0, len(c.cfg.AccessLists))
	for id := range c.cfg.AccessLists {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		al := c.cfg.AccessLists[id]
		a := &accessRT{satisfyAny: al.SatisfyAny}
		if len(al.Rules) > 0 {
			m, err := compileIPRules(al.Rules)
			if err != nil {
				c.fail("access list %s: %v", id, err)
				continue
			}
			a.rules = m
		}
		if ba := al.BasicAuth; ba != nil {
			a.basic = c.basicAuth(id, ba)
			if a.basic == nil {
				continue
			}
		}
		c.lists[id] = a
	}
}

func (c *compiler) basicAuth(id string, ba *BasicAuth) *basicAuth {
	if ba.UsersFile == "" {
		c.fail("access list %s: basicAuth.usersFile is required", id)
		return nil
	}
	file := c.path(ba.UsersFile)
	data, err := os.ReadFile(file)
	if err != nil {
		c.fail("access list %s: %v", id, err)
		return nil
	}
	sum := sha256.Sum256(data)
	key := ba.Realm + "\x00" + string(sum[:])
	if b := c.rt.basics[key]; b != nil {
		return b
	}
	if prev := c.env.prev; prev != nil && prev.basics[key] != nil {
		c.rt.basics[key] = prev.basics[key] // keep the verification cache across reloads
		return prev.basics[key]
	}
	users, err := loadHtpasswd(file)
	if err != nil {
		c.fail("access list %s: %v", id, err)
		return nil
	}
	b := newBasicAuth(ba.Realm, users)
	c.rt.basics[key] = b
	return b
}

func (c *compiler) transport(key transportKey) *upstreamTransport {
	if t := c.rt.transports[key]; t != nil {
		return t
	}
	t := (*upstreamTransport)(nil)
	if prev := c.env.prev; prev != nil {
		t = prev.transports[key]
	}
	if t == nil {
		t = newUpstreamTransport(key)
	}
	c.rt.transports[key] = t
	return t
}

func secondsOr(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (c *compiler) host(h *Host, where string) *hostRT {
	hr := &hostRT{id: h.ID, blockExploits: h.BlockExploits, maxBody: h.MaxBodyBytes}
	if len(h.AllowCountries) > 0 {
		if c.cfg.GeoIPDatabase == "" {
			c.fail("%s: allowCountries needs geoipDatabase", where)
		}
		hr.countries = map[string]bool{}
		for _, cc := range h.AllowCountries {
			if len(cc) != 2 || strings.ToUpper(cc) != cc {
				c.fail("%s: allowCountries: %q is not an upper-case two-letter country code", where, cc)
				continue
			}
			hr.countries[cc] = true
		}
	}
	if h.MaxBodyBytes < 0 {
		c.fail("%s: maxBodyBytes must not be negative", where)
	}
	key := transportKey{verify: h.UpstreamTLSVerify, read: secondsOr(h.ReadTimeoutSec, 60), send: secondsOr(h.SendTimeoutSec, 60)}
	if rl := h.RateLimit; rl != nil {
		if rl.RequestsPerSecond <= 0 || rl.Burst < 0 {
			c.fail("%s: rate limit needs requestsPerSecond > 0 and burst >= 0", where)
		} else {
			lkey := fmt.Sprintf("%s|%s|%d|%d", where, h.ID, rl.RequestsPerSecond, rl.Burst)
			l := c.rt.limiters[lkey]
			if l == nil && c.env.prev != nil {
				l = c.env.prev.limiters[lkey]
			}
			if l == nil {
				l = newRateLimiter(float64(rl.RequestsPerSecond), rl.Burst)
			}
			c.rt.limiters[lkey] = l
			hl := &hostLimiter{l: l, exempt: newPrefixMap[bool](), exemptDefault: rl.ExemptDefault}
			for _, e := range rl.Exempt {
				p, err := parsePrefix(e.CIDR)
				if err != nil {
					c.fail("%s: rate limit exempt: %v", where, err)
					continue
				}
				hl.exempt.add(p, e.Exempt)
			}
			hr.limiter = hl
		}
	}
	if fa := h.ForwardAuth; fa != nil {
		hr.fa = c.forwardAuth(fa, key, where)
	}
	if m := h.Maintenance; m != nil {
		mr := &maintenanceRT{page: []byte(m.Page), bypass: newPrefixMap[bool](), bypassDefault: m.BypassDefault}
		for _, e := range m.Bypass {
			p, err := parsePrefix(e.CIDR)
			if err != nil {
				c.fail("%s: maintenance bypass: %v", where, err)
				continue
			}
			mr.bypass.add(p, e.Exempt)
		}
		hr.maint = mr
	}
	for i := range h.Locations {
		if l := c.location(h, hr, &h.Locations[i], key, where); l != nil {
			hr.locations = append(hr.locations, l)
		}
	}
	sort.SliceStable(hr.locations, func(i, j int) bool { return len(hr.locations[i].path) > len(hr.locations[j].path) })
	hr.pathRedirects = c.pathRedirects(h.PathRedirects, where)
	return hr
}

func (c *compiler) forwardAuth(fa *ForwardAuth, key transportKey, where string) *forwardAuthRT {
	u, err := url.Parse(strings.TrimSpace(fa.VerifyURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.fail("%s: forward auth verifyUrl %q is not an absolute http(s) URL", where, fa.VerifyURL)
		return nil
	}
	rt := &forwardAuthRT{verify: u, passUser: fa.PassRemoteUser, passGroups: fa.PassRemoteGroups, transport: c.transport(key), timeout: connectTimeout + key.read}
	if s := strings.TrimSpace(fa.SignInURL); s != "" {
		if _, err := url.Parse(s); err != nil {
			c.fail("%s: forward auth signInUrl: %v", where, err)
		}
		rt.signIn = s + "?rd="
		if strings.Contains(s, "?") {
			rt.signIn = s + "&rd="
		}
	}
	return rt
}

func (c *compiler) location(h *Host, hr *hostRT, l *Location, key transportKey, where string) *locationRT {
	where = fmt.Sprintf("%s location %q", where, l.Path)
	if !strings.HasPrefix(l.Path, "/") {
		c.fail("%s: path must start with /", where)
		return nil
	}
	lr := &locationRT{path: l.Path, prefix: strings.TrimRight(l.Path, "/"), strip: l.StripPrefix, websockets: l.Websockets, cache: l.Cache, denyAll: l.DenyAll, skipMaint: l.SkipMaintenance}
	switch l.Kind {
	case "deny":
		lr.deny = true
	case "proxy", "":
		up := l.Upstream
		scheme := strings.ToLower(up.Scheme)
		if scheme == "" {
			scheme = "http"
		}
		if scheme != "http" && scheme != "https" {
			c.fail("%s: upstream scheme %q must be http or https", where, up.Scheme)
		}
		port := up.Port
		if port == 0 {
			port = 80
			if scheme == "https" {
				port = 443
			}
		}
		host := strings.Trim(strings.TrimSpace(up.Host), "[]")
		if host == "" || !validPort(port) || strings.ContainsAny(host, "/ ?#@") {
			c.fail("%s: invalid upstream %q port %d", where, up.Host, up.Port)
		}
		base := strings.TrimRight(strings.TrimSpace(up.Path), "/")
		if base != "" && !strings.HasPrefix(base, "/") {
			base = "/" + base
		}
		hp := net.JoinHostPort(host, strconv.Itoa(port))
		lr.up = upstreamRT{scheme: scheme, port: port, hostPort: hp, proxyHost: hp, base: base, transport: c.transport(key)}
	default:
		c.fail("%s: unknown kind %q", where, l.Kind)
		return nil
	}
	for _, hd := range l.Headers {
		if !headerNameRe.MatchString(hd.Name) {
			c.fail("%s: invalid header name %q", where, hd.Name)
			continue
		}
		if strings.ContainsAny(hd.Value, "\r\n") {
			c.fail("%s: header %s: value contains a line break", where, hd.Name)
			continue
		}
		t, err := compileTemplate(hd.Value)
		if err != nil {
			c.fail("%s: header %s: %v", where, hd.Name, err)
			continue
		}
		name := http.CanonicalHeaderKey(hd.Name)
		lr.headers = append(lr.headers, headerRT{name: name, tmpl: t, host: name == "Host"})
	}
	if l.AccessListID != "" && !l.DenyAll {
		a, ok := c.lists[l.AccessListID]
		if !ok {
			if _, exists := c.cfg.AccessLists[l.AccessListID]; !exists {
				c.fail("%s: access list %q does not exist", where, l.AccessListID)
			}
			return nil
		}
		lr.access = a
	}
	if l.ForwardAuth {
		if h.ForwardAuth == nil {
			c.fail("%s: forwardAuth is set but the host has no forward auth", where)
		}
		lr.forwardAuth = hr.fa != nil
	}
	return lr
}

func redirectCode(code int) (int, bool) {
	switch code {
	case 301, 302, 307, 308:
		return code, true
	case 0:
		return 301, true
	}
	return 0, false
}

func (c *compiler) pathRedirects(prs []PathRedirect, where string) []pathRedirectRT {
	out := make([]pathRedirectRT, 0, len(prs))
	for _, p := range prs {
		code, ok := redirectCode(p.Code)
		to := strings.TrimSpace(p.To)
		from := strings.TrimRight(p.From, "/")
		if !ok || to == "" || (p.From != "" && !strings.HasPrefix(p.From, "/")) {
			c.fail("%s: invalid path redirect %q → %q (%d)", where, p.From, p.To, p.Code)
			continue
		}
		t := c.redirectTemplate(to, where)
		if t == nil {
			continue
		}
		out = append(out, pathRedirectRT{from: from, fromSlash: from + "/", to: t, code: code, keep: p.KeepPath})
	}
	return out
}

func (c *compiler) redirectTemplate(to, where string) *template {
	if strings.ContainsAny(to, "\r\n") {
		c.fail("%s: redirect target contains a line break", where)
		return nil
	}
	t, err := compileTemplate(to)
	if err != nil {
		c.fail("%s: redirect target: %v", where, err)
		return nil
	}
	return t
}

// addHeader returns an add_header entry.
func addHeader(name, value string) headerKV {
	return headerKV{name: name, value: []string{value}}
}

func (c *compiler) servers() {
	cfg := c.cfg
	rt := c.rt
	altSvc := fmt.Sprintf(`h3=":%d"; ma=86400`, cfg.HTTPSPort)
	for i := range cfg.Hosts {
		h := &cfg.Hosts[i]
		where := fmt.Sprintf("host %s", h.ID)
		if len(h.Domains) == 0 {
			c.fail("%s: no domains", where)
			continue
		}
		hr := c.host(h, where)
		m := c.env.metrics.host(h.ID)
		name := strings.ToLower(h.Domains[0])
		var common []headerKV
		if h.NoIndex {
			common = append(common, addHeader("X-Robots-Tag", "noindex, nofollow"))
		}
		if h.Cert != nil && h.ForceHTTPS {
			rt.http.addAll(h.Domains, &vserver{kind: kindHTTPSRedirect, name: name, hostID: h.ID, acme: true, metrics: m})
		} else {
			rt.http.addAll(h.Domains, &vserver{kind: kindHost, name: name, hostID: h.ID, host: hr, acme: true, headers: common, metrics: m})
		}
		if h.Cert != nil {
			profile, err := profileFor(h.CipherProfile, cfg.TLSProfile)
			if err != nil {
				c.fail("%s: %v", where, err)
				continue
			}
			var headers []headerKV
			if h.HSTS != "" {
				headers = append(headers, addHeader("Strict-Transport-Security", h.HSTS))
			}
			if h.HTTP3 {
				headers = append(headers, addHeader("Alt-Svc", altSvc))
			}
			headers = append(headers, common...)
			rt.https.addAll(h.Domains, &vserver{kind: kindHost, name: name, hostID: h.ID, host: hr, acme: !h.ForceHTTPS, headers: headers, metrics: m,
				tlsConf: handshakeConfig(profile, h.HTTP2, c.certFunc(h.Cert))})
		}
	}
	defProfile, _ := profileFor(cfg.TLSProfile)
	for i := range cfg.Redirects {
		g := &cfg.Redirects[i]
		where := fmt.Sprintf("redirect group %d", i+1)
		if len(g.Domains) == 0 {
			c.fail("%s: no domains", where)
			continue
		}
		gr := &groupRT{paths: c.pathRedirects(g.Paths, where)}
		if w := g.Whole; w != nil {
			code, ok := redirectCode(w.Code)
			to := strings.TrimSpace(w.To)
			if !ok || to == "" {
				c.fail("%s: invalid redirect to %q (%d)", where, w.To, w.Code)
			} else if t := c.redirectTemplate(to, where); t != nil {
				gr.whole = &wholeRedirectRT{to: t, code: code, keep: w.KeepPath}
			}
		}
		m := c.env.metrics.host("")
		name := strings.ToLower(g.Domains[0])
		if g.Cert != nil && g.ForceHTTPS {
			rt.http.addAll(g.Domains, &vserver{kind: kindHTTPSRedirect, name: name, acme: true, metrics: m})
		} else {
			rt.http.addAll(g.Domains, &vserver{kind: kindGroup, name: name, group: gr, acme: true, metrics: m})
		}
		if g.Cert != nil {
			rt.https.addAll(g.Domains, &vserver{kind: kindGroup, name: name, group: gr, acme: !g.ForceHTTPS, metrics: m,
				tlsConf: handshakeConfig(defProfile, true, c.certFunc(g.Cert))})
		}
	}

	d := cfg.Default
	def := &vserver{kind: kindDefault, name: "_", acme: true, action: d.Action, metrics: c.env.metrics.host("")}
	switch d.Action {
	case "", "close":
		def.action = "close"
	case "404":
	case "redirect":
		if to := strings.TrimSpace(d.RedirectTo); to == "" {
			def.action = "close" // like the nginx renderer
		} else {
			def.redirectTo = c.redirectTemplate(to, "default server")
		}
	case "host":
		if d.Host == nil {
			def.action = "close"
			break
		}
		def.host = c.host(d.Host, "default host")
		if d.Host.NoIndex {
			def.headers = []headerKV{addHeader("X-Robots-Tag", "noindex, nofollow")}
		}
	default:
		c.fail("default server: unknown action %q", d.Action)
	}
	httpsDef := *def
	httpsDef.tlsConf = handshakeConfig(defProfile, true, c.certFunc(d.Cert))
	rt.http.def, rt.https.def = def, &httpsDef
}

// ---------------------------------------------------------------- listeners

// listenSpec is one socket the runtime needs. Reloads keep sockets whose key
// is unchanged.
type listenSpec struct {
	key     string
	kind    string // http | https | quic | status | stream
	network string // tcp | udp
	addr    string
	host    string // bind host, "" = all interfaces
	port    int
	stream  *streamRT
	label   string // for error messages
}

func isWildcardHost(h string) bool {
	return h == "" || h == "0.0.0.0" || h == "::"
}

func (c *compiler) listeners() {
	cfg := c.cfg
	var specs []listenSpec
	add := func(kind, network, host string, port int, label string, st *streamRT) {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		specs = append(specs, listenSpec{key: kind + "|" + network + "|" + addr, kind: kind, network: network, addr: addr, host: host, port: port, stream: st, label: label})
	}
	bind := c.env.bindHost
	if cfg.HTTPPort > 0 {
		add("http", "tcp", bind, cfg.HTTPPort, fmt.Sprintf("HTTP port %d", cfg.HTTPPort), nil)
	}
	if cfg.HTTPSPort > 0 {
		add("https", "tcp", bind, cfg.HTTPSPort, fmt.Sprintf("HTTPS port %d", cfg.HTTPSPort), nil)
		if cfg.HTTP3 || cfg.Default.HTTP3 {
			add("quic", "udp", bind, cfg.HTTPSPort, fmt.Sprintf("HTTP/3 port %d/udp", cfg.HTTPSPort), nil)
		}
	}
	if cfg.StatusAddr != "" {
		host, p, err := net.SplitHostPort(cfg.StatusAddr)
		port, perr := strconv.Atoi(p)
		if err != nil || perr != nil || port <= 0 || port > 65535 {
			c.fail("statusAddr %q must be host:port", cfg.StatusAddr)
		} else {
			add("status", "tcp", host, port, "status address "+cfg.StatusAddr, nil)
		}
	}
	for i := range cfg.Streams {
		st := &cfg.Streams[i]
		name := st.Name
		if name == "" {
			name = st.ID
		}
		where := fmt.Sprintf("stream %q", name)
		spec := c.stream(st, where)
		if spec == nil {
			continue
		}
		host := strings.Trim(strings.TrimSpace(st.ListenAddr), "[]")
		for p := st.ListenLo; p <= st.ListenHi; p++ {
			if st.TCP {
				add("stream", "tcp", host, p, fmt.Sprintf("%s port %d/tcp", where, p), spec)
			}
			if st.UDP {
				add("stream", "udp", host, p, fmt.Sprintf("%s port %d/udp", where, p), spec)
			}
		}
	}
	type bindKey struct {
		network string
		port    int
	}
	seen := map[bindKey][]int{}
	for i, s := range specs {
		bk := bindKey{s.network, s.port}
		for _, j := range seen[bk] {
			o := specs[j]
			if s.host == o.host || isWildcardHost(s.host) || isWildcardHost(o.host) {
				c.fail("%s conflicts with %s", s.label, o.label)
			}
		}
		seen[bk] = append(seen[bk], i)
	}
	c.rt.listeners = specs
}

func (c *compiler) stream(st *Stream, where string) *streamRT {
	bad := func(format string, args ...any) *streamRT {
		c.fail(where+": "+format, args...)
		return nil
	}
	if !st.TCP && !st.UDP {
		return bad("neither tcp nor udp is enabled")
	}
	if st.ListenLo < 1 || st.ListenHi > 65535 || st.ListenHi < st.ListenLo {
		return bad("invalid listen ports %d-%d", st.ListenLo, st.ListenHi)
	}
	if st.ForwardLo < 1 || st.ForwardHi > 65535 || st.ForwardHi < st.ForwardLo {
		return bad("invalid forward ports %d-%d", st.ForwardLo, st.ForwardHi)
	}
	if st.ForwardLo != st.ForwardHi && st.ForwardHi-st.ForwardLo != st.ListenHi-st.ListenLo {
		return bad("listen and forward port ranges differ in size")
	}
	host := strings.Trim(strings.TrimSpace(st.ForwardHost), "[]")
	if host == "" {
		return bad("forward host is empty")
	}
	if a := strings.Trim(strings.TrimSpace(st.ListenAddr), "[]"); a != "" {
		if _, err := netip.ParseAddr(a); err != nil {
			return bad("listen address %q is not an IP address", st.ListenAddr)
		}
	}
	if st.IdleTimeoutMs < 0 || st.ConnectTimeoutMs < 0 {
		return bad("timeouts must not be negative")
	}
	idle, connect := time.Duration(st.IdleTimeoutMs)*time.Millisecond, time.Duration(st.ConnectTimeoutMs)*time.Millisecond
	if idle == 0 {
		idle = 10 * time.Minute
	}
	if connect == 0 {
		connect = 10 * time.Second
	}
	return &streamRT{id: st.ID, forwardHost: host, listenLo: st.ListenLo, forwardLo: st.ForwardLo, forwardHi: st.ForwardHi,
		proxyProtocol: st.ProxyProtocol, idle: idle, connect: connect}
}
