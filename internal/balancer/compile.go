package balancer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/lbcheck"
)

// Built-in defaults (the values Relay renders into haproxy.cfg defaults).
const (
	defaultConnect  = 5 * time.Second
	defaultClient   = 50 * time.Second
	defaultServer   = 50 * time.Second
	defaultInterval = 2 * time.Second
	defaultRise     = 2
	defaultFall     = 3
	defaultInspect  = 5 * time.Second
	defaultExpire   = 30 * time.Minute
	defaultTable    = 200000
)

// firstDuration returns the first positive value in milliseconds, else def.
func firstDuration(def time.Duration, ms ...int64) time.Duration {
	for _, v := range ms {
		if v > 0 {
			return time.Duration(v) * time.Millisecond
		}
	}
	return def
}

func firstInt(def int, vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return def
}

// runtime is one compiled configuration. It is immutable once active except
// for load-balancing scratch state guarded by each backend's lbMu.
type runtime struct {
	cfg      *spec.Config
	hash     string
	loaded   time.Time
	warnings []string

	frontends []*frontend
	backends  []*backend
	beByID    map[string]*backend
	beByName  map[string]*backend
	stats     *statsConf
	listeners []listenSpec
	maxConn   int

	fes  map[string]*feState
	bes  map[string]*beState
	srvs map[string]*srvState

	checkCancel context.CancelFunc
}

type listenSpec struct {
	key   string // kind|host:port
	kind  string // http | tcp | stats
	addr  string
	fe    *frontend
	stats *statsConf
}

type frontend struct {
	id, name, mode  string
	iid             int
	bind            string
	acceptProxy     bool
	forwardFor      bool
	ffExcept        []netip.Prefix
	compression     bool
	inspectDelay    time.Duration
	needSNI         bool
	rules           []rule
	def             *backend
	clientTimeout   time.Duration
	maxConn         int
	st              *feState
	listenKey       string
	referencesAnyBe bool
}

type rule struct {
	conds []cond
	be    *backend
}

type cond struct {
	typ      string
	negate   bool
	never    bool // not available in this mode
	suffix   bool
	found    bool
	name     string // canonical header name
	values   []string
	res      []*regexp.Regexp
	prefixes []netip.Prefix
}

type backend struct {
	id, name, mode, algo string
	iid                  int
	servers              []*server
	actives, backups     []*server
	srvByName            map[string]*server
	srvByCookie          map[string]*server

	check      *spec.HealthCheck
	interval   time.Duration
	sticky     *spec.Sticky
	forwardFor bool
	sendProxy  bool
	tlsConf    *tls.Config
	tlsVerify  bool
	roots      *x509.CertPool
	retries    int
	redispatch bool

	connectTimeout time.Duration
	serverTimeout  time.Duration
	queueTimeout   time.Duration
	fullConn       int

	st *beState

	lbMu sync.Mutex
	rr   atomic.Uint64
	hmap atomic.Pointer[hashMap]
}

type server struct {
	be         *backend
	name       string
	cookie     string
	idx        int
	backup     bool
	host       string
	port       int
	dialAddr   string // resolved ip:port (or host:port when unresolved)
	unresolved bool
	cfgState   string
	cfgWeight  int
	checkOn    bool
	rise, fall int
	st         *srvState
	pool       *connPool
	dkey       dialKey
	cw         int64 // smooth weighted round robin, guarded by be.lbMu
}

type dialKey struct {
	addr                   string
	tls, verify, sendProxy bool
	connect, serverTimeout time.Duration
	caFile                 string
}

type statsConf struct {
	bind       string
	prometheus bool
	rules      []accessRule
	st         *feState
	iid        int
}

type accessRule struct {
	allow, all bool
	p          netip.Prefix
}

type compileEnv struct {
	prev *runtime
	dry  bool // Check: no DNS, no state, no pools
	log  *logger
	// resolve looks up server host names (nil = net.DefaultResolver).
	resolve func(ctx context.Context, host string) (netip.Addr, error)
}

// compile validates cfg and builds a runtime. States and connection pools of
// env.prev are reused for proxies and servers that still exist; nothing is
// mutated until commit.
func compile(cfg *spec.Config, env compileEnv) (*runtime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	rt := &runtime{
		cfg: cfg, loaded: time.Now(), maxConn: cfg.MaxConn,
		beByID: map[string]*backend{}, beByName: map[string]*backend{},
		fes: map[string]*feState{}, bes: map[string]*beState{}, srvs: map[string]*srvState{},
	}
	prev := env.prev
	if prev == nil {
		prev = &runtime{fes: map[string]*feState{}, bes: map[string]*beState{}, srvs: map[string]*srvState{}}
	}

	var roots *x509.CertPool
	needCA := false
	for _, b := range cfg.Backends {
		needCA = needCA || b.TLSVerify
	}
	if needCA && cfg.CAFile != "" {
		data, err := os.ReadFile(cfg.CAFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			rt.warnings = append(rt.warnings, fmt.Sprintf("caFile %s does not exist; verifying servers against the system roots", cfg.CAFile))
		case err != nil:
			return nil, fmt.Errorf("caFile: %w", err)
		default:
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(data) {
				return nil, fmt.Errorf("caFile %s: no PEM certificates found", cfg.CAFile)
			}
		}
	}

	addrs := map[string]netip.Addr{}
	if !env.dry {
		addrs = resolveHosts(cfg, env.resolve)
	}

	iid := 2
	if cfg.Stats != nil {
		sc := &statsConf{bind: cfg.Stats.Bind, prometheus: cfg.Stats.Prometheus, iid: iid}
		iid++
		for _, r := range cfg.Stats.Access {
			p, all, _ := spec.ParseIPRule(r.CIDR)
			sc.rules = append(sc.rules, accessRule{allow: r.Allow, all: all, p: p})
		}
		sc.st = prev.fes["\x00stats"]
		if sc.st == nil {
			sc.st = &feState{name: "stats"}
		}
		rt.fes["\x00stats"] = sc.st
		rt.stats = sc
		rt.listeners = append(rt.listeners, listenSpec{key: "stats|" + bindKey(sc.bind), kind: "stats", addr: sc.bind, stats: sc})
	}
	feIID := iid
	iid += len(cfg.Frontends)

	for i := range cfg.Backends {
		sb := &cfg.Backends[i]
		be := &backend{
			id: sb.ID, name: sb.Name, mode: sb.Mode, algo: sb.Algorithm, iid: iid,
			srvByName: map[string]*server{}, srvByCookie: map[string]*server{},
			check: sb.Check, forwardFor: sb.ForwardFor && sb.Mode == spec.ModeHTTP, sendProxy: sb.SendProxy,
			retries: sb.Retries, redispatch: sb.Redispatch, sticky: sb.Sticky,
			connectTimeout: firstDuration(defaultConnect, sb.Timeouts.ConnectMs, cfg.Timeouts.ConnectMs),
			serverTimeout:  firstDuration(defaultServer, sb.Timeouts.ServerMs, cfg.Timeouts.ServerMs),
		}
		iid++
		be.queueTimeout = firstDuration(be.connectTimeout, sb.Timeouts.QueueMs, cfg.Timeouts.QueueMs)
		if sb.TLS {
			be.tlsConf = lbcheck.TLSConfig(sb.TLSVerify, roots, "")
			be.tlsVerify, be.roots = sb.TLSVerify, roots
		}
		rise, fall := defaultRise, defaultFall
		if sb.Check != nil {
			be.interval = firstDuration(defaultInterval, sb.Check.IntervalMs, cfg.Check.IntervalMs)
			rise = firstInt(defaultRise, sb.Check.Rise, cfg.Check.Rise)
			fall = firstInt(defaultFall, sb.Check.Fall, cfg.Check.Fall)
		}
		be.st = prev.bes[sb.Name]
		if be.st == nil {
			be.st = newBeState(sb.Name, env.log)
		}
		rt.bes[sb.Name] = be.st
		for j, ss := range sb.Servers {
			host := strings.Trim(ss.Address, "[]")
			srv := &server{
				be: be, name: ss.Name, cookie: ss.Cookie, idx: j, backup: ss.Backup, host: host, port: ss.Port,
				cfgState: ss.State, cfgWeight: ss.Weight, checkOn: sb.Check != nil && ss.Check, rise: rise, fall: fall,
			}
			if srv.cfgState == "" {
				srv.cfgState = spec.StateReady
			}
			if a, err := netip.ParseAddr(host); err == nil {
				srv.dialAddr = netip.AddrPortFrom(a.Unmap(), uint16(ss.Port)).String()
			} else if a, ok := addrs[host]; ok {
				srv.dialAddr = netip.AddrPortFrom(a, uint16(ss.Port)).String()
			} else {
				srv.dialAddr = net.JoinHostPort(host, strconv.Itoa(ss.Port))
				srv.unresolved = !env.dry
			}
			key := sb.Name + "/" + ss.Name + "/" + net.JoinHostPort(host, strconv.Itoa(ss.Port))
			srv.st = prev.srvs[key]
			if srv.st == nil {
				srv.st = newSrvState(key, ss.Name, be.st)
			}
			rt.srvs[key] = srv.st
			srv.dkey = dialKey{addr: srv.dialAddr, tls: sb.TLS, verify: sb.TLSVerify, sendProxy: sb.SendProxy,
				connect: be.connectTimeout, serverTimeout: be.serverTimeout, caFile: cfg.CAFile}
			be.servers = append(be.servers, srv)
			be.srvByName[srv.name] = srv
			if srv.cookie != "" {
				if _, dup := be.srvByCookie[srv.cookie]; !dup {
					be.srvByCookie[srv.cookie] = srv
				}
			}
			if srv.backup {
				be.backups = append(be.backups, srv)
			} else {
				be.actives = append(be.actives, srv)
			}
		}
		rt.backends = append(rt.backends, be)
		rt.beByID[be.id] = be
		rt.beByName[be.name] = be
	}
	if !env.dry {
		old := map[*srvState]*server{}
		for _, pb := range prev.backends {
			for _, ps := range pb.servers {
				old[ps.st] = ps
			}
		}
		for _, be := range rt.backends {
			for _, srv := range be.servers {
				if ps := old[srv.st]; ps != nil && ps.pool != nil && ps.dkey == srv.dkey {
					srv.pool = ps.pool
				} else {
					srv.pool = newConnPool()
				}
			}
		}
	}

	for i := range cfg.Frontends {
		sf := &cfg.Frontends[i]
		fe := &frontend{
			id: sf.ID, name: sf.Name, mode: sf.Mode, iid: feIID + i, bind: sf.Bind,
			acceptProxy: sf.AcceptProxy, forwardFor: sf.ForwardFor && sf.Mode == spec.ModeHTTP,
			compression:   sf.Compression && sf.Mode == spec.ModeHTTP,
			clientTimeout: firstDuration(defaultClient, sf.Timeouts.ClientMs, cfg.Timeouts.ClientMs),
			maxConn:       cfg.MaxConn,
		}
		if fe.forwardFor {
			for _, e := range sf.ForwardForExcept {
				p, _ := spec.ParsePrefix(e)
				fe.ffExcept = append(fe.ffExcept, p)
			}
		}
		for _, r := range sf.Rules {
			cr := rule{be: rt.beByID[r.Backend]}
			for _, c := range r.Conditions {
				cc := compileCond(c, sf.Mode)
				if cc.typ == spec.CondSNI && !cc.never {
					fe.needSNI = true
				}
				cr.conds = append(cr.conds, cc)
			}
			fe.rules = append(fe.rules, cr)
		}
		fe.inspectDelay = firstDuration(defaultInspect, sf.InspectDelayMs)
		if sf.DefaultBackend != "" {
			fe.def = rt.beByID[sf.DefaultBackend]
		}
		fe.st = prev.fes[sf.Name]
		if fe.st == nil {
			fe.st = &feState{name: sf.Name}
		}
		rt.fes[sf.Name] = fe.st
		fe.listenKey = sf.Mode + "|" + bindKey(sf.Bind)
		rt.frontends = append(rt.frontends, fe)
		rt.listeners = append(rt.listeners, listenSpec{key: fe.listenKey, kind: sf.Mode, addr: sf.Bind, fe: fe})
		// HAProxy fullconn defaults to 10% of the maxconn of the frontends
		// referencing the backend.
		seen := map[*backend]bool{}
		for _, r := range fe.rules {
			seen[r.be] = true
		}
		if fe.def != nil {
			seen[fe.def] = true
		}
		for be := range seen {
			if be != nil {
				be.fullConn += cfg.MaxConn / 10
			}
		}
	}
	return rt, nil
}

// bindKey normalises a bind for listener identity.
func bindKey(bind string) string {
	host, port, err := spec.ParseBind(bind)
	if err != nil {
		return bind
	}
	if a, err := netip.ParseAddr(host); err == nil {
		host = a.Unmap().String()
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port))
}

func compileCond(c spec.Condition, mode string) cond {
	cc := cond{typ: c.Type, negate: c.Negate}
	switch c.Type {
	case spec.CondHost, spec.CondSNI:
		cc.suffix = c.Match == spec.MatchSuffix
		for _, v := range c.Values {
			cc.values = append(cc.values, strings.ToLower(v))
		}
		cc.never = (c.Type == spec.CondHost) != (mode == spec.ModeHTTP)
	case spec.CondPath, spec.CondPathBeg:
		cc.values = c.Values
		cc.never = mode != spec.ModeHTTP
	case spec.CondPathReg:
		for _, v := range c.Values {
			cc.res = append(cc.res, regexp.MustCompile(v))
		}
		cc.never = mode != spec.ModeHTTP
	case spec.CondHeader:
		cc.name = textproto.CanonicalMIMEHeaderKey(c.Name)
		cc.found = c.Match == spec.MatchFound
		cc.values = c.Values
		cc.never = mode != spec.ModeHTTP
	case spec.CondSrc:
		for _, v := range c.Values {
			p, _ := spec.ParsePrefix(v)
			cc.prefixes = append(cc.prefixes, p)
		}
	}
	return cc
}

// resolveHosts looks up every server host name concurrently (HAProxy
// init-addr libc,none: unresolvable servers start DOWN).
func resolveHosts(cfg *spec.Config, resolve func(context.Context, string) (netip.Addr, error)) map[string]netip.Addr {
	if resolve == nil {
		resolve = func(ctx context.Context, host string) (netip.Addr, error) {
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return netip.Addr{}, err
			}
			if len(ips) == 0 {
				return netip.Addr{}, errors.New("no addresses")
			}
			return ips[0].Unmap(), nil
		}
	}
	hosts := map[string]bool{}
	for _, b := range cfg.Backends {
		for _, s := range b.Servers {
			h := strings.Trim(s.Address, "[]")
			if _, err := netip.ParseAddr(h); err != nil {
				hosts[h] = true
			}
		}
	}
	out := map[string]netip.Addr{}
	if len(hosts) == 0 {
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for h := range hosts {
		wg.Go(func() {
			if a, err := resolve(ctx, h); err == nil {
				mu.Lock()
				out[h] = a
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

// commit makes rt's configuration effective on the shared states.
func (rt *runtime) commit(_ *logger) {
	for _, be := range rt.backends {
		be.st.cur.Store(be)
		if be.sticky != nil && be.sticky.Mode == spec.StickySource {
			size := firstInt(defaultTable, be.sticky.TableSize)
			expire := firstDuration(defaultExpire, be.sticky.ExpireMs)
			t := be.st.stick.Load()
			if t == nil {
				t = newStickTable(size, expire)
				be.st.stick.Store(t)
			} else {
				t.configure(size, expire)
			}
		} else {
			be.st.stick.Store(nil)
		}
		for _, srv := range be.servers {
			srv.st.applyConfig(srv)
		}
		be.st.changed()
	}
}

// closePools closes the idle connections of pools rt no longer uses.
func (rt *runtime) closePools(next *runtime) {
	keep := map[*connPool]bool{}
	if next != nil {
		for _, be := range next.backends {
			for _, s := range be.servers {
				keep[s.pool] = true
			}
		}
	}
	for _, be := range rt.backends {
		for _, s := range be.servers {
			if s.pool != nil && !keep[s.pool] {
				s.pool.close()
			}
		}
	}
}

// ---------------------------------------------------------------- rules

// routeHTTP returns the backend for a request (nil = none).
func (fe *frontend) routeHTTP(r *http.Request, client netip.Addr) *backend {
	var path, host string
	var havePath, haveHost bool
	for i := range fe.rules {
		ru := &fe.rules[i]
		ok := true
		for j := range ru.conds {
			c := &ru.conds[j]
			var m bool
			switch {
			case c.never:
			case c.typ == spec.CondSrc:
				m = c.matchSrc(client)
			case c.typ == spec.CondHost:
				if !haveHost {
					host, haveHost = hostOnly(r.Host), true
				}
				m = c.matchName(host)
			case c.typ == spec.CondHeader:
				m = c.matchHeader(r)
			default:
				if !havePath {
					path, havePath = rawPath(r), true
				}
				m = c.matchPath(path)
			}
			if m == c.negate {
				ok = false
				break
			}
		}
		if ok {
			return ru.be
		}
	}
	return fe.def
}

// routeTCP returns the backend for a TCP session.
func (fe *frontend) routeTCP(client netip.Addr, sni string) *backend {
	for i := range fe.rules {
		ru := &fe.rules[i]
		ok := true
		for j := range ru.conds {
			c := &ru.conds[j]
			var m bool
			switch {
			case c.never:
			case c.typ == spec.CondSrc:
				m = c.matchSrc(client)
			case c.typ == spec.CondSNI:
				m = sni != "" && c.matchName(sni)
			}
			if m == c.negate {
				ok = false
				break
			}
		}
		if ok {
			return ru.be
		}
	}
	return fe.def
}

func (c *cond) matchSrc(a netip.Addr) bool {
	for _, p := range c.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (c *cond) matchName(name string) bool {
	for _, v := range c.values {
		if c.suffix {
			if strings.HasSuffix(name, v) {
				return true
			}
		} else if name == v {
			return true
		}
	}
	return false
}

func (c *cond) matchPath(path string) bool {
	switch c.typ {
	case spec.CondPath:
		for _, v := range c.values {
			if path == v {
				return true
			}
		}
	case spec.CondPathBeg:
		for _, v := range c.values {
			if strings.HasPrefix(path, v) {
				return true
			}
		}
	case spec.CondPathReg:
		for _, re := range c.res {
			if re.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// matchHeader implements req.hdr(name) -m found / -m str: every
// comma-separated value of every occurrence is tested.
func (c *cond) matchHeader(r *http.Request) bool {
	var lines []string
	if c.name == "Host" {
		if r.Host != "" {
			lines = []string{r.Host}
		}
	} else {
		lines = r.Header[c.name]
	}
	if c.found {
		return len(lines) > 0
	}
	for _, l := range lines {
		for part := range strings.SplitSeq(l, ",") {
			part = strings.TrimSpace(part)
			for _, v := range c.values {
				if part == v {
					return true
				}
			}
		}
	}
	return false
}

// hostOnly lowercases a Host header value and strips the port
// (HAProxy host_only keeps IPv6 brackets).
func hostOnly(h string) string {
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i > 0 {
			h = h[:i+1]
		}
	} else if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return toLower(h)
}

func toLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

// rawPath is HAProxy's path fetch: the request target's path, not decoded,
// without the query string (absolute-form targets lose scheme and authority).
func rawPath(r *http.Request) string {
	u := r.RequestURI
	if i := strings.Index(u, "://"); i >= 0 && !strings.HasPrefix(u, "/") {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			u = rest[j:]
		} else {
			u = "/"
		}
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u
}
