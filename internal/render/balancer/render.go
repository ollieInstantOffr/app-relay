// Package balancer renders a configuration snapshot into Relay Balancer's
// resolved JSON configuration (balancer.json, schema internal/balancer/spec).
//
// Relay Balancer must behave exactly like the haproxy.cfg rendered by
// internal/render/haproxy for the same snapshot. This package therefore applies
// the same rules as the HAProxy renderer — settings defaults, frontend filters
// and rule ordering, ACL semantics, algorithm fallbacks, server names, health
// check and stickiness resolution — and emits the result so the data plane only
// has to execute it.
//
// Output is deterministic for a given snapshot and env so config hashes are stable.
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/render/haproxy"
)

// ConfigFile is the only file in a Relay Balancer release.
const ConfigFile = "balancer.json"

// RuntimeSocketName is the runtime API socket in the run directory. It differs
// from HAProxy's so both engines can share a run dir during a switch.
const RuntimeSocketName = "balancer-runtime.sock"

// RuntimeSocketPath returns the runtime API socket path for a run dir.
func RuntimeSocketPath(runDir string) string {
	if runDir == "" {
		runDir = "/run/relay"
	}
	return filepath.Join(runDir, RuntimeSocketName)
}

// HasBackends reports whether the load balancer should run.
func HasBackends(snap *model.Snapshot) bool { return haproxy.HasBackends(snap) }

// Render returns {"balancer.json": …}. Like the edge renderer it returns the
// configuration it could render together with an error describing everything
// that failed (the same snapshots make haproxy.Render fail).
func Render(snap *model.Snapshot, env render.Env) (agent.Files, error) {
	r := newRenderer(snap)
	cfg := r.config(env)
	data, err := encodeJSON(cfg)
	if err != nil {
		return nil, fmt.Errorf("balancer render: %w", err)
	}
	return agent.Files{ConfigFile: data}, r.err()
}

// RenderBackend returns the JSON of one resolved backend (previews). Errors
// become a leading "// error: …" line followed by best-effort JSON.
func RenderBackend(snap *model.Snapshot, bk *model.Backend) string {
	r := newRenderer(snap)
	return r.preview(r.backend(bk))
}

// RenderFrontend returns the JSON of one resolved frontend, regardless of
// Enabled (previews). Errors become a leading "// error: …" line followed by
// best-effort JSON.
func RenderFrontend(snap *model.Snapshot, f *model.Frontend) string {
	r := newRenderer(snap)
	return r.preview(r.frontend(f))
}

// ---------------------------------------------------------------- renderer

type renderer struct {
	snap     *model.Snapshot
	s        model.HAProxySettings
	backends map[string]*model.Backend
	errs     []string
	notes    []string
}

func newRenderer(snap *model.Snapshot) *renderer {
	r := &renderer{snap: snap, s: effectiveSettings(snap.HAProxy), backends: map[string]*model.Backend{}}
	for i := range snap.Backends {
		if _, dup := r.backends[snap.Backends[i].ID]; !dup {
			r.backends[snap.Backends[i].ID] = &snap.Backends[i]
		}
	}
	return r
}

func (r *renderer) fail(format string, args ...any) {
	r.errs = append(r.errs, oneLine(fmt.Sprintf(format, args...)))
}

func (r *renderer) note(format string, args ...any) {
	r.notes = append(r.notes, oneLine(fmt.Sprintf(format, args...)))
}

func (r *renderer) err() error {
	if len(r.errs) == 0 {
		return nil
	}
	return fmt.Errorf("balancer render: %s", strings.Join(r.errs, "; "))
}

func (r *renderer) preview(v any) string {
	out, err := encodeJSON(v)
	if err != nil {
		r.fail("%v", err)
	}
	if len(r.errs) > 0 {
		return "// error: " + strings.Join(r.errs, "; ") + "\n" + out
	}
	return out
}

// config builds the complete resolved configuration (haproxy.Render).
func (r *renderer) config(env render.Env) *spec.Config {
	s := r.s
	cfg := &spec.Config{
		Schema:        spec.SchemaVersion,
		RuntimeSocket: RuntimeSocketPath(env.RunDir),
		MaxConn:       s.MaxConn,
		Timeouts: spec.Timeouts{
			ConnectMs: r.timeout("timeout connect", s.TimeoutConnect),
			ClientMs:  r.timeout("timeout client", s.TimeoutClient),
			ServerMs:  r.timeout("timeout server", s.TimeoutServer),
		},
		Check: spec.CheckDefaults{
			IntervalMs: r.interval("check interval", s.CheckInterval),
			Rise:       s.Rise,
			Fall:       s.Fall,
		},
		Frontends: []spec.Frontend{},
		Backends:  []spec.Backend{},
	}
	if !s.SeamlessReload {
		r.note("seamless reload is off in the settings, but Relay Balancer always reloads without dropping connections")
	}
	if s.StatsEnabled && s.StatsBind != "" {
		cfg.Stats = r.stats()
	}
	for i := range r.snap.Frontends {
		if f := &r.snap.Frontends[i]; f.Enabled {
			cfg.Frontends = append(cfg.Frontends, r.frontend(f))
		}
	}
	for i := range r.snap.Backends {
		b := r.backend(&r.snap.Backends[i])
		if b.TLSVerify {
			// haproxy.cfg only references the CA bundle on verified servers.
			cfg.CAFile = haproxy.CAFile
		}
		cfg.Backends = append(cfg.Backends, b)
	}
	if len(r.notes) > 0 {
		cfg.Notes = r.notes
	}
	return cfg
}

// effectiveSettings mirrors the HAProxy renderer's zero-value defaults.
func effectiveSettings(s model.HAProxySettings) model.HAProxySettings {
	def := func(v *string, d string) {
		if strings.TrimSpace(*v) == "" {
			*v = d
		}
	}
	def(&s.TimeoutConnect, "5s")
	def(&s.TimeoutClient, "50s")
	def(&s.TimeoutServer, "50s")
	def(&s.CheckInterval, "2s")
	if s.MaxConn <= 0 {
		s.MaxConn = 20000
	}
	if s.Rise <= 0 {
		s.Rise = 2
	}
	if s.Fall <= 0 {
		s.Fall = 3
	}
	return s
}

// ---------------------------------------------------------------- stats

func (r *renderer) stats() *spec.Stats {
	st := &spec.Stats{Bind: r.bind("stats", r.s.StatsBind), Prometheus: r.s.Prometheus}
	if id := r.s.StatsAccessList; id != "" {
		// Like haproxy.cfg, an unknown access list leaves the listener open.
		for i := range r.snap.AccessLists {
			if al := &r.snap.AccessLists[i]; al.ID == id {
				rules, err := accessRules(al.Rules)
				if err != nil {
					r.fail("stats access list %s: %v", al.Name, err)
				}
				st.Access = rules
				break
			}
		}
	}
	return st
}

// accessRules converts an access list into first-match-wins rules whose
// outcome for every client equals the `http-request deny` lines produced by
// haproxy.AccessRules: rules are taken in order, "all" ends the list, rules
// with an empty CIDR or unknown action are ignored, and when the list allows
// anything (without "allow all") unmatched clients are denied.
func accessRules(rules []model.IPRule) ([]spec.IPRule, error) {
	var out []spec.IPRule
	hasAllow := false
	for i, rule := range rules {
		cidr := strings.TrimSpace(rule.CIDR)
		if cidr == "" {
			continue
		}
		all := strings.EqualFold(cidr, "all")
		if all {
			cidr = "all"
		} else {
			c, err := canonicalCIDR(cidr)
			if err != nil {
				return out, fmt.Errorf("rule %d: %w", i+1, err)
			}
			cidr = c
		}
		switch rule.Action {
		case "allow":
			out = append(out, spec.IPRule{Allow: true, CIDR: cidr})
			if all {
				return out, nil
			}
			hasAllow = true
		case "deny":
			out = append(out, spec.IPRule{Allow: false, CIDR: cidr})
			if all {
				return out, nil
			}
		}
	}
	if hasAllow {
		out = append(out, spec.IPRule{Allow: false, CIDR: "all"})
	}
	return out, nil
}

// canonicalCIDR validates an IP or CIDR (HAProxy masks host bits).
func canonicalCIDR(v string) (string, error) {
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return "", fmt.Errorf("%q is not an IP address or CIDR", v)
		}
		return p.Masked().String(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || a.Zone() != "" {
		return "", fmt.Errorf("%q is not an IP address or CIDR", v)
	}
	return a.String(), nil
}

// bind normalises "addr:port" ("*:80" and ":80" listen on all interfaces).
func (r *renderer) bind(what, v string) string {
	addr, port, err := model.SplitBind(v)
	if err != nil {
		r.fail("%s: bind %q: %v", what, v, err)
		return strings.TrimSpace(v)
	}
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

// ---------------------------------------------------------------- frontends

// frontend resolves one frontend (haproxy renderFrontend).
func (r *renderer) frontend(f *model.Frontend) spec.Frontend {
	mode := spec.ModeHTTP
	if f.Mode == spec.ModeTCP {
		mode = spec.ModeTCP
	}
	out := spec.Frontend{
		ID:          f.ID,
		Name:        f.Name,
		Mode:        mode,
		Bind:        r.bind("frontend "+f.Name, f.Bind),
		AcceptProxy: f.AcceptProxy,
	}
	if mode == spec.ModeHTTP {
		if f.HostID != "" {
			// option forwardfor except 127.0.0.0/8: fed by the reverse proxy,
			// which already sets X-Forwarded-For.
			out.ForwardFor = true
			out.ForwardForExcept = []string{"127.0.0.0/8"}
		}
		out.Compression = f.Compression
	}

	usesSNI := false
	for _, rule := range f.Rules {
		for _, c := range rule.Conditions {
			if c.Type == model.CondSNI {
				usesSNI = true
			}
		}
	}
	if mode == spec.ModeTCP && usesSNI {
		out.InspectDelayMs = 5000 // tcp-request inspect-delay 5s
	}

	for i, rule := range f.Rules {
		if len(rule.Conditions) == 0 {
			continue
		}
		if _, ok := r.backends[rule.BackendID]; !ok {
			r.fail("frontend %s rule %d: backend %q does not exist", f.Name, i+1, rule.BackendID)
			continue
		}
		conds := []spec.Condition{}
		never, bad := false, false
		for j, c := range rule.Conditions {
			sc, known, err := condition(c)
			if err != nil {
				r.fail("frontend %s rule %d condition %d: %v", f.Name, i+1, j+1, err)
				bad = true
				continue
			}
			if !known {
				// haproxy.cfg renders "always_false": the rule never matches,
				// unless the condition is negated (then it always holds).
				if !c.Negate {
					never = true
				}
				continue
			}
			conds = append(conds, sc)
		}
		if bad {
			continue
		}
		if never {
			r.note("frontend %s rule %d: unknown condition type never matches (as in HAProxy); rule skipped", f.Name, i+1)
			continue
		}
		if len(conds) == 0 {
			// Only negated unknown conditions: always true.
			conds = append(conds, spec.Condition{Type: spec.CondSrc, Values: []string{"0.0.0.0/0", "::/0"}})
		}
		out.Rules = append(out.Rules, spec.Rule{Conditions: conds, Backend: rule.BackendID})
	}
	if f.DefaultBackendID != "" {
		if _, ok := r.backends[f.DefaultBackendID]; ok {
			out.DefaultBackend = f.DefaultBackendID
		} else {
			r.fail("frontend %s: default backend %q does not exist", f.Name, f.DefaultBackendID)
		}
	}
	return out
}

// condition maps one rule condition exactly like haproxy conditionExpr.
// known is false for unknown types (HAProxy "always_false").
func condition(c model.Condition) (out spec.Condition, known bool, err error) {
	v := strings.TrimSpace(c.Value)
	// haproxy quoteArg drops single quotes from quoted arguments.
	q := strings.ReplaceAll(v, "'", "")
	out.Negate = c.Negate
	switch c.Type {
	case model.CondHost, model.CondSNI:
		out.Type = spec.CondHost
		if c.Type == model.CondSNI {
			out.Type = spec.CondSNI
		}
		h := strings.ToLower(v)
		if strings.HasPrefix(h, "*.") {
			// -m end -i ".example.com": the leading dot stays part of the suffix.
			out.Match = spec.MatchSuffix
			out.Values = []string{strings.ReplaceAll(h[1:], "'", "")}
		} else {
			out.Match = spec.MatchExact
			out.Values = []string{strings.ReplaceAll(h, "'", "")}
		}
	case model.CondPathBeg:
		out.Type = spec.CondPathBeg
		out.Values = []string{q}
	case model.CondPath:
		out.Type = spec.CondPath
		out.Values = []string{q}
	case model.CondPathRegex:
		if _, cerr := regexp.Compile(q); cerr != nil {
			return out, true, fmt.Errorf("path regex %q is not supported by Relay Balancer, which uses Go (RE2) syntax: %v", q, cerr)
		}
		out.Type = spec.CondPathReg
		out.Values = []string{q}
	case model.CondHeader:
		out.Type = spec.CondHeader
		out.Name = strings.TrimSpace(c.Name)
		if v == "" {
			out.Match = spec.MatchFound
		} else {
			out.Match = spec.MatchExact
			out.Values = []string{q}
		}
	case model.CondSrc:
		out.Type = spec.CondSrc
		parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
		out.Values = []string{}
		for _, p := range parts {
			cidr, cerr := canonicalCIDR(p)
			if cerr != nil {
				return out, true, cerr
			}
			out.Values = append(out.Values, cidr)
		}
	default:
		return out, false, nil
	}
	return out, true, nil
}

// ---------------------------------------------------------------- backends

var balanceAlgos = map[string]string{
	"roundrobin": spec.AlgoRoundRobin,
	"leastconn":  spec.AlgoLeastConn,
	"source":     spec.AlgoSource,
	"uri":        spec.AlgoURI,
	"random":     spec.AlgoRandom,
	"first":      spec.AlgoFirst,
	"static-rr":  spec.AlgoStaticRR,
}

var (
	statusCodeRe  = regexp.MustCompile(`^[1-5][0-9]{2}$`)
	statusClassRe = regexp.MustCompile(`^([1-5])xx$`)
	statusListRe  = regexp.MustCompile(`^[1-5][0-9]{2}(-[1-5][0-9]{2})?(,[1-5][0-9]{2}(-[1-5][0-9]{2})?)*$`)
)

// haproxyDefaultRetries is HAProxy's built-in "retries" when none is set.
const haproxyDefaultRetries = 3

// backend resolves one backend (haproxy RenderBackend).
func (r *renderer) backend(bk *model.Backend) spec.Backend {
	label := "backend " + bk.Name
	mode := spec.ModeHTTP
	if bk.Mode == spec.ModeTCP {
		mode = spec.ModeTCP
	}
	algo := balanceAlgos[bk.Algorithm]
	if algo == "" || (algo == spec.AlgoURI && mode != spec.ModeHTTP) {
		algo = spec.AlgoRoundRobin
	}
	out := spec.Backend{
		ID:         bk.ID,
		Name:       bk.Name,
		Mode:       mode,
		Algorithm:  algo,
		Servers:    []spec.Server{},
		ForwardFor: mode == spec.ModeHTTP && bk.ForwardClientIP,
		SendProxy:  bk.SendProxy,
		TLS:        bk.TLSReencrypt,
		TLSVerify:  bk.TLSReencrypt && bk.TLSVerify,
		Retries:    haproxyDefaultRetries,
		Redispatch: len(bk.Servers) > 1,
	}
	if bk.Retries > 0 {
		out.Retries = bk.Retries
	}

	hc := bk.HealthCheck
	checks := hc.Type != "" && hc.Type != "none"
	if checks {
		out.Check = r.healthCheck(label, hc)
	}

	sticky := bk.Sticky.Enabled
	stickyMode := bk.Sticky.Mode
	if sticky {
		if mode != spec.ModeHTTP {
			stickyMode = spec.StickySource
		}
		switch stickyMode {
		case spec.StickySource:
			// stick-table type ip size 200k expire 30m; stick on src
			out.Sticky = &spec.Sticky{Mode: spec.StickySource, ExpireMs: 30 * 60 * 1000, TableSize: 200000}
		case spec.StickyPrefix:
			out.Sticky = &spec.Sticky{Mode: spec.StickyPrefix, Cookie: cookieName(bk.Sticky.CookieName)}
		default:
			stickyMode = spec.StickyInsert
			out.Sticky = &spec.Sticky{Mode: spec.StickyInsert, Cookie: cookieName(bk.Sticky.CookieName)}
		}
	}

	if v := strings.TrimSpace(bk.Timeouts.Connect); v != "" {
		out.Timeouts.ConnectMs = r.timeout(label+" timeout connect", v)
	}
	if v := strings.TrimSpace(bk.Timeouts.Server); v != "" {
		out.Timeouts.ServerMs = r.timeout(label+" timeout server", v)
	}
	if v := strings.TrimSpace(bk.Timeouts.Queue); v != "" {
		out.Timeouts.QueueMs = r.timeout(label+" timeout queue", v)
	}

	for i, srv := range bk.Servers {
		name := haproxy.ServerName(bk, i)
		w := srv.Weight
		if w <= 0 {
			w = 100
		}
		if w > 256 {
			r.fail("%s server %s: weight %d is out of range (1–256)", label, name, w)
			w = 256
		}
		state := spec.StateReady
		switch srv.State {
		case model.ServerStateDrain:
			state = spec.StateDrain
		case model.ServerStateMaint:
			state = spec.StateMaint
		}
		s := spec.Server{
			ID:      srv.ID,
			Name:    name,
			Address: serverAddress(srv.Address),
			Port:    srv.Port,
			Weight:  w,
			Backup:  srv.Role == model.ServerBackup,
			Check:   checks && srv.Check,
			State:   state,
		}
		if sticky && (stickyMode == spec.StickyInsert || stickyMode == spec.StickyPrefix) {
			s.Cookie = name
		}
		out.Servers = append(out.Servers, s)
	}
	return out
}

// healthCheck resolves the check options and the per-backend default-server
// line (inter/rise/fall are always explicit, like haproxy.cfg).
func (r *renderer) healthCheck(label string, hc model.HealthCheck) *spec.HealthCheck {
	out := &spec.HealthCheck{Type: spec.CheckTCP}
	switch hc.Type {
	case "http":
		out.Type = spec.CheckHTTP
		out.Method = strings.ToUpper(strings.TrimSpace(hc.Method))
		if out.Method == "" {
			out.Method = "GET"
		}
		out.Path = strings.ReplaceAll(strings.TrimSpace(hc.Path), "'", "")
		if out.Path == "" {
			out.Path = "/"
		}
		out.Host = strings.ReplaceAll(strings.TrimSpace(hc.Host), "'", "")
		out.Expect = expectStatus(hc.ExpectStatus)
	case "pgsql":
		out.Type = spec.CheckPgSQL
		out.User = "relay"
	case "mysql":
		out.Type = spec.CheckMySQL
	case "redis":
		out.Type = spec.CheckRedis
	}
	inter := strings.TrimSpace(hc.Interval)
	if inter == "" {
		inter = r.s.CheckInterval
	}
	out.IntervalMs = r.interval(label+" check interval", inter)
	out.Rise, out.Fall = hc.Rise, hc.Fall
	if out.Rise <= 0 {
		out.Rise = r.s.Rise
	}
	if out.Fall <= 0 {
		out.Fall = r.s.Fall
	}
	return out
}

// expectStatus mirrors haproxy's http-check expect: "status <code|list>" is
// kept verbatim, "rstatus ^N" becomes "Nxx", anything else is "2xx".
func expectStatus(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case statusCodeRe.MatchString(v), statusClassRe.MatchString(v), statusListRe.MatchString(v):
		return v
	}
	return "2xx"
}

func cookieName(n string) string {
	if strings.TrimSpace(n) == "" {
		return "SRVID"
	}
	return n
}

// serverAddress returns the bare host or IP (IPv6 without brackets).
func serverAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = addr[1 : len(addr)-1]
	}
	return addr
}

// ---------------------------------------------------------------- durations

var durationRe = regexp.MustCompile(`^([0-9]+)(us|ms|s|m|h|d)?$`)

// maxDurationMs is HAProxy's timer limit (2147483647 ms, ~24.8 days).
const maxDurationMs = math.MaxInt32

// ParseDurationMs converts an HAProxy time value (units us, ms, s, m, h, d;
// a bare number is milliseconds) to milliseconds. Microseconds round up, as
// in HAProxy.
func ParseDurationMs(v string) (int64, error) {
	v = strings.TrimSpace(v)
	m := durationRe.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q (use a number with us, ms, s, m, h or d, e.g. 5s)", v)
	}
	tooLarge := fmt.Errorf("duration %q is too large (maximum %d ms, ~24.8 days)", v, maxDurationMs)
	if len(strings.TrimLeft(m[1], "0")) > 13 {
		return 0, tooLarge
	}
	n, _ := strconv.ParseInt(m[1], 10, 64) // ≤ 13 digits: no overflow
	if m[2] != "us" && n > maxDurationMs {
		return 0, tooLarge // before multiplying
	}
	var ms int64
	switch m[2] {
	case "us":
		ms = (n + 999) / 1000
	case "", "ms":
		ms = n
	case "s":
		ms = n * 1000
	case "m":
		ms = n * 60 * 1000
	case "h":
		ms = n * 3600 * 1000
	case "d":
		ms = n * 86400 * 1000
	}
	if ms > maxDurationMs {
		return 0, tooLarge
	}
	return ms, nil
}

// timeout parses a timeout; 0 means "no timeout" in HAProxy, which the schema
// can't express (0 inherits), so it gets a note.
func (r *renderer) timeout(what, v string) int64 {
	ms, err := ParseDurationMs(v)
	if err != nil {
		r.fail("%s: %v", what, err)
		return 0
	}
	if ms == 0 {
		r.note("%s is 0 (no timeout in HAProxy); Relay Balancer has no unlimited timeout, so the inherited or built-in default applies", what)
	}
	return ms
}

// interval parses a health check interval; HAProxy rejects 0.
func (r *renderer) interval(what, v string) int64 {
	ms, err := ParseDurationMs(v)
	if err != nil {
		r.fail("%s: %v", what, err)
		return 0
	}
	if ms == 0 {
		r.fail("%s: must be greater than 0", what)
	}
	return ms
}

// ---------------------------------------------------------------- helpers

// encodeJSON pretty-prints v (2-space indent, no HTML escaping, trailing newline).
func encodeJSON(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}
