package spec

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/lbcheck"
)

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	tokenRe  = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)
	methodRe = regexp.MustCompile(`^[A-Z]{1,32}$`)
)

// ParseBind splits "host:port". host "" or "*" means all interfaces.
func ParseBind(bind string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(bind))
	if err != nil {
		return "", 0, fmt.Errorf("bind %q: use host:port", bind)
	}
	port, err = strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bind %q: port must be 1–65535", bind)
	}
	if h == "*" {
		h = ""
	}
	if strings.ContainsAny(h, " \t/") {
		return "", 0, fmt.Errorf("bind %q: invalid address", bind)
	}
	return h, port, nil
}

// ParsePrefix parses an IP address (a single-address prefix) or a CIDR.
// Host bits are masked.
func ParsePrefix(v string) (netip.Prefix, error) {
	v = strings.TrimSpace(v)
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not an IP address or CIDR", v)
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("%q is not an IP address or CIDR", v)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ParseIPRule parses an access rule CIDR: an IP, a CIDR or "all".
func ParseIPRule(cidr string) (p netip.Prefix, all bool, err error) {
	if strings.EqualFold(strings.TrimSpace(cidr), "all") {
		return netip.Prefix{}, true, nil
	}
	p, err = ParsePrefix(cidr)
	return p, false, err
}

// Validate checks c and returns every problem found (errors.Join, one per
// line). Conditions that don't fit the frontend's mode (host in tcp, sni in
// http) are valid: they never match, like HAProxy ACLs on unavailable fetches.
// wildcardHost reports whether a bind host listens on all addresses.
func wildcardHost(host string) bool { return host == "" || host == "0.0.0.0" || host == "::" }

// maxUnixSocketPath is the longest portable unix socket path.
const maxUnixSocketPath = 103

func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Schema != SchemaVersion {
		add("unsupported schema %d (this data plane supports schema %d)", c.Schema, SchemaVersion)
	}
	if c.RuntimeSocket != "" && !filepath.IsAbs(c.RuntimeSocket) {
		add("runtimeSocket %q must be an absolute path", c.RuntimeSocket)
	}
	// Unix socket paths are limited to 104 bytes on macOS/BSD and 108 on Linux
	// (including the terminating NUL); longer ones fail to bind with EINVAL.
	if len(c.RuntimeSocket) > maxUnixSocketPath {
		add("runtimeSocket %q is too long for a unix socket (%d bytes, max %d)", c.RuntimeSocket, len(c.RuntimeSocket), maxUnixSocketPath)
	}
	if c.MaxConn < 0 {
		add("maxConn must not be negative")
	}
	validTimeouts("global timeouts", c.Timeouts, add)
	if c.Check.IntervalMs < 0 || c.Check.Rise < 0 || c.Check.Fall < 0 {
		add("check defaults: intervalMs, rise and fall must not be negative")
	}

	type bindOwner struct{ owner, bind, host string }
	binds := map[string]bindOwner{}
	byPort := map[int][]bindOwner{}
	claim := func(owner, bind string) {
		host, port, err := ParseBind(bind)
		if err != nil {
			add("%s: %v", owner, err)
			return
		}
		key := strings.ToLower(host) + "|" + strconv.Itoa(port)
		if a, err := netip.ParseAddr(host); err == nil {
			key = a.Unmap().String() + "|" + strconv.Itoa(port)
		}
		if prev, dup := binds[key]; dup {
			add("%s: bind %s is already used by %s", owner, bind, prev.owner)
			return
		}
		cur := bindOwner{owner, bind, host}
		// A wildcard listener takes the port on every address (Go listens
		// dual-stack), so on Linux a second listener on the same port fails
		// to bind. Report it here instead of at bind time.
		for _, other := range byPort[port] {
			if wildcardHost(host) || wildcardHost(other.host) {
				add("%s: bind %s conflicts with %s (%s): a wildcard address already takes port %d on every address", owner, bind, other.bind, other.owner, port)
				break
			}
		}
		binds[key] = cur
		byPort[port] = append(byPort[port], cur)
	}
	if c.Stats != nil {
		claim("stats", c.Stats.Bind)
		for i, r := range c.Stats.Access {
			if _, _, err := ParseIPRule(r.CIDR); err != nil {
				add("stats access rule %d: %v", i+1, err)
			}
		}
	}

	backends := map[string]*Backend{}
	beNames := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		label := fmt.Sprintf("backend %q", b.Name)
		if b.Name == "" {
			label = fmt.Sprintf("backend %d", i+1)
		}
		switch {
		case b.ID == "":
			add("%s: id is required", label)
		case backends[b.ID] != nil:
			add("%s: duplicate backend id %q", label, b.ID)
		default:
			backends[b.ID] = b
		}
		if !nameRe.MatchString(b.Name) {
			add("%s: name must be 1–128 letters, digits, '_', '.', ':' or '-'", label)
		} else if beNames[b.Name] {
			add("%s: duplicate backend name", label)
		}
		beNames[b.Name] = true
		validateBackend(label, b, add)
	}

	feNames := map[string]bool{}
	for i := range c.Frontends {
		f := &c.Frontends[i]
		label := fmt.Sprintf("frontend %q", f.Name)
		if f.Name == "" {
			label = fmt.Sprintf("frontend %d", i+1)
		}
		if !nameRe.MatchString(f.Name) {
			add("%s: name must be 1–128 letters, digits, '_', '.', ':' or '-'", label)
		} else if feNames[f.Name] {
			add("%s: duplicate frontend name", label)
		}
		feNames[f.Name] = true
		if f.Mode != ModeHTTP && f.Mode != ModeTCP {
			add("%s: mode must be http or tcp", label)
		}
		claim(label, f.Bind)
		for _, e := range f.ForwardForExcept {
			if _, err := ParsePrefix(e); err != nil {
				add("%s: forwardForExcept: %v", label, err)
			}
		}
		if f.InspectDelayMs < 0 {
			add("%s: inspectDelayMs must not be negative", label)
		}
		validTimeouts(label+" timeouts", f.Timeouts, add)
		useBackend := func(what, id string) {
			b := backends[id]
			switch {
			case id == "":
				add("%s %s: backend is required", label, what)
			case b == nil:
				add("%s %s: backend %q does not exist", label, what, id)
			case (f.Mode == ModeHTTP || f.Mode == ModeTCP) && b.Mode != f.Mode:
				add("%s %s: backend %q is in %s mode, the frontend in %s mode", label, what, b.Name, b.Mode, f.Mode)
			}
		}
		for ri, r := range f.Rules {
			what := fmt.Sprintf("rule %d", ri+1)
			useBackend(what, r.Backend)
			for ci, cond := range r.Conditions {
				validateCondition(fmt.Sprintf("%s %s condition %d", label, what, ci+1), cond, add)
			}
		}
		if f.DefaultBackend != "" {
			useBackend("default backend", f.DefaultBackend)
		}
	}
	return errors.Join(errs...)
}

func validTimeouts(label string, t Timeouts, add func(string, ...any)) {
	if t.ConnectMs < 0 || t.ClientMs < 0 || t.ServerMs < 0 || t.QueueMs < 0 {
		add("%s must not be negative", label)
	}
}

func validateCondition(label string, c Condition, add func(string, ...any)) {
	needValues := func() {
		if len(c.Values) == 0 {
			add("%s: %s needs at least one value", label, c.Type)
		}
	}
	switch c.Type {
	case CondHost, CondSNI:
		needValues()
		if c.Match != "" && c.Match != MatchExact && c.Match != MatchSuffix {
			add("%s: %s match must be exact or suffix", label, c.Type)
		}
	case CondPath, CondPathBeg:
		needValues()
	case CondPathReg:
		needValues()
		for _, v := range c.Values {
			if _, err := regexp.Compile(v); err != nil {
				add("%s: path_reg %q: %v", label, v, err)
			}
		}
	case CondHeader:
		if !tokenRe.MatchString(c.Name) {
			add("%s: header name %q is invalid", label, c.Name)
		}
		switch c.Match {
		case MatchFound:
		case "", MatchExact:
			needValues()
		default:
			add("%s: header match must be found or exact", label)
		}
	case CondSrc:
		needValues()
		for _, v := range c.Values {
			if _, err := ParsePrefix(v); err != nil {
				add("%s: src: %v", label, err)
			}
		}
	default:
		add("%s: unknown condition type %q", label, c.Type)
	}
}

func validateBackend(label string, b *Backend, add func(string, ...any)) {
	if b.Mode != ModeHTTP && b.Mode != ModeTCP {
		add("%s: mode must be http or tcp", label)
	}
	switch b.Algorithm {
	case AlgoRoundRobin, AlgoStaticRR, AlgoLeastConn, AlgoSource, AlgoRandom, AlgoFirst:
	case AlgoURI:
		if b.Mode != ModeHTTP {
			add("%s: algorithm uri needs http mode", label)
		}
	default:
		add("%s: unknown algorithm %q", label, b.Algorithm)
	}
	names := map[string]bool{}
	for i, s := range b.Servers {
		sl := fmt.Sprintf("%s server %q", label, s.Name)
		if s.Name == "" {
			sl = fmt.Sprintf("%s server %d", label, i+1)
		}
		if !nameRe.MatchString(s.Name) {
			add("%s: name must be 1–128 letters, digits, '_', '.', ':' or '-'", sl)
		} else if names[s.Name] {
			add("%s: duplicate server name", sl)
		}
		names[s.Name] = true
		if addr := strings.Trim(s.Address, "[]"); addr == "" || strings.ContainsAny(addr, " \t/") {
			add("%s: address %q is invalid", sl, s.Address)
		}
		if s.Port < 1 || s.Port > 65535 {
			add("%s: port must be 1–65535", sl)
		}
		if s.Weight < 0 || s.Weight > 256 {
			add("%s: weight must be 0–256", sl)
		}
		switch s.State {
		case "", StateReady, StateDrain, StateMaint:
		default:
			add("%s: state must be ready, drain or maint", sl)
		}
		if s.Cookie != "" && !tokenRe.MatchString(s.Cookie) {
			add("%s: cookie value %q is invalid", sl, s.Cookie)
		}
	}
	if hc := b.Check; hc != nil {
		switch hc.Type {
		case CheckTCP, CheckPgSQL, CheckMySQL, CheckRedis:
		case CheckHTTP:
			if hc.Method != "" && !methodRe.MatchString(hc.Method) {
				add("%s: check method %q is invalid", label, hc.Method)
			}
			if hc.Path != "" && (!strings.HasPrefix(hc.Path, "/") || strings.ContainsAny(hc.Path, " \t\r\n")) {
				add("%s: check path must start with / and contain no spaces", label)
			}
			if strings.ContainsAny(hc.Host, " \t\r\n") {
				add("%s: check host %q is invalid", label, hc.Host)
			}
			if !lbcheck.ValidExpect(hc.Expect) {
				add("%s: check expect %q: use a status like 200, 2xx, 200-399 or 200,204", label, hc.Expect)
			}
		default:
			add("%s: unknown check type %q", label, hc.Type)
		}
		if hc.IntervalMs < 0 || hc.Rise < 0 || hc.Fall < 0 {
			add("%s: check intervalMs, rise and fall must not be negative", label)
		}
	}
	if st := b.Sticky; st != nil {
		switch st.Mode {
		case StickyInsert, StickyPrefix:
			if b.Mode != ModeHTTP {
				add("%s: sticky %s needs http mode", label, st.Mode)
			}
			if !tokenRe.MatchString(st.Cookie) || strings.ContainsAny(st.Cookie, "=;,") {
				add("%s: sticky cookie name %q is invalid", label, st.Cookie)
			}
		case StickySource:
		default:
			add("%s: unknown sticky mode %q", label, st.Mode)
		}
		if st.ExpireMs < 0 || st.TableSize < 0 {
			add("%s: sticky expireMs and tableSize must not be negative", label)
		}
	}
	if b.TLSVerify && !b.TLS {
		add("%s: tlsVerify needs tls", label)
	}
	if b.Retries < 0 {
		add("%s: retries must not be negative", label)
	}
	validTimeouts(label+" timeouts", b.Timeouts, add)
}
