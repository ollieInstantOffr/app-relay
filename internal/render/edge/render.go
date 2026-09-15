// Package edge renders a configuration snapshot into Relay Edge's resolved
// JSON configuration (edge.json).
//
// Relay Edge must behave exactly like the nginx configuration rendered by
// internal/render/nginx for the same snapshot (docs/EDGE.md §4). This package
// therefore applies the same modelling rules as the nginx renderer — enabled
// host ordering, usable certificates, synthesised locations, effective access
// lists, redirect grouping and injection, default host fallbacks, stream port
// mapping — and emits the result so the data plane only has to execute it.
//
// Rendered file set (paths relative to the release root):
//
//	edge.json                 the resolved configuration (internal/edge.Config)
//	htpasswd/<accessListId>   basic-auth users (same format as the nginx renderer)
//
// Output is deterministic for a given snapshot and env so config hashes are stable.
package edge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/instantoffr/relay/internal/agent"
	edgecfg "github.com/instantoffr/relay/internal/edge"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
)

// ConfigFile is the main file of the rendered release.
const ConfigFile = "edge.json"

// StatusAddr is where Relay Edge's local status listener (/healthz,
// /stub_status, /metrics) binds. The apply service overrides it from
// $RELAY_EDGE_STATUS_PORT, like nginx.StubStatusAddr.
var StatusAddr = "127.0.0.1:18081"

const (
	defaultACMEWebroot = "/data/acme"
	defaultLogDir      = "/var/log/relay"
)

// Render produces edge.json plus one htpasswd file per access list with basic
// auth enabled. Like the nginx renderer it returns the files it could render
// together with an error describing everything that failed.
func Render(snap *model.Snapshot, env render.Env) (agent.Files, error) {
	r := newRenderer(snap, env)
	cfg := r.config()
	if set := snap.ErrorPages.WithDefaults(); set.Enabled {
		cfg.ErrorPages = map[string]string{}
		for _, code := range render.ErrorPageCodes {
			key := fmt.Sprint(code)
			cfg.ErrorPages[key] = render.ErrorPageHTML(set, key, nil)
		}
	}
	data, err := encodeJSON(cfg)
	if err != nil {
		return nil, fmt.Errorf("edge render: %w", err)
	}
	files := agent.Files{ConfigFile: data}
	for i := range snap.AccessLists {
		al := &snap.AccessLists[i]
		if al.BasicAuth.Enabled {
			files["htpasswd/"+safeID(al.ID)] = htpasswd(al)
		}
	}
	return files, r.err()
}

// ---------------------------------------------------------------- renderer

type renderer struct {
	snap      *model.Snapshot
	env       render.Env
	certs     map[string]*model.Certificate
	lists     map[string]*model.AccessList
	hosts     map[string]*model.ProxyHost
	backends  map[string]*model.Backend
	httpPort  int
	httpsPort int
	errs      []string
	notes     []string
	// usedLists collects the access lists referenced by rendered locations.
	usedLists map[string]bool
}

func newRenderer(snap *model.Snapshot, env render.Env) *renderer {
	r := &renderer{
		snap: snap, env: env,
		certs: map[string]*model.Certificate{}, lists: map[string]*model.AccessList{},
		hosts: map[string]*model.ProxyHost{}, backends: map[string]*model.Backend{},
		httpPort: snap.General.HTTPPort, httpsPort: snap.General.HTTPSPort,
		usedLists: map[string]bool{},
	}
	if r.httpPort <= 0 {
		r.httpPort = 80
	}
	if r.httpsPort <= 0 {
		r.httpsPort = 443
	}
	if r.env.Modules == nil {
		r.env.Modules = map[string]bool{}
	}
	for i := range snap.Certificates {
		r.certs[snap.Certificates[i].ID] = &snap.Certificates[i]
	}
	for i := range snap.AccessLists {
		r.lists[snap.AccessLists[i].ID] = &snap.AccessLists[i]
	}
	for i := range snap.Hosts {
		r.hosts[snap.Hosts[i].ID] = &snap.Hosts[i]
	}
	for i := range snap.Backends {
		r.backends[snap.Backends[i].ID] = &snap.Backends[i]
	}
	return r
}

func (r *renderer) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *renderer) note(format string, args ...any) {
	r.notes = append(r.notes, oneLine(fmt.Sprintf(format, args...)))
}

func (r *renderer) err() error {
	if len(r.errs) == 0 {
		return nil
	}
	return fmt.Errorf("edge render: %s", strings.Join(r.errs, "; "))
}

func (r *renderer) mod(name string) bool { return r.env.Modules[name] }

// config builds the complete resolved configuration.
func (r *renderer) config() *edgecfg.Config {
	hosts := r.enabledHosts()
	r.checkUnsupported(hosts)

	cfg := &edgecfg.Config{
		Schema:      1,
		HTTPPort:    r.httpPort,
		HTTPSPort:   r.httpsPort,
		HTTP3:       r.quicEnabled(),
		StatusAddr:  StatusAddr,
		LogDir:      orDefault(r.env.LogDir, defaultLogDir),
		ACMEWebroot: orDefault(r.env.ACMEWebroot, defaultACMEWebroot),
		TLSProfile:  r.cipherProfile(""),
		Blocklist:   r.blocklist(),
		Hosts:       []edgecfg.Host{},
	}
	for _, h := range hosts {
		cfg.Hosts = append(cfg.Hosts, r.host(h, false))
	}
	cfg.Default = r.defaultServer()
	cfg.Redirects = r.redirects()
	cfg.Streams = r.streams()
	cfg.AccessLists = r.accessLists()
	if len(r.notes) > 0 {
		cfg.Notes = r.notes
	}
	return cfg
}

// enabledHosts returns enabled hosts sorted by first domain, then id.
func (r *renderer) enabledHosts() []*model.ProxyHost {
	var out []*model.ProxyHost
	for i := range r.snap.Hosts {
		if h := &r.snap.Hosts[i]; h.Enabled && len(h.Domains) > 0 {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domains[0] != out[j].Domains[0] {
			return out[i].Domains[0] < out[j].Domains[0]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// checkUnsupported records notes for configuration Relay Edge skips (every
// enabled host plus the host served by the default server). Skipped settings
// stay stored, so switching back to nginx applies them again.
func (r *renderer) checkUnsupported(hosts []*model.ProxyHost) {
	seen := map[*model.ProxyHost]bool{}
	for _, h := range hosts {
		seen[h] = true
		r.checkHost(h)
	}
	if dh := r.snap.DefaultHost; dh.Action == "host" {
		if h := r.hosts[dh.HostID]; h != nil && h.Enabled && !seen[h] {
			r.checkHost(h)
		}
	}
}

func (r *renderer) checkHost(h *model.ProxyHost) {
	if strings.TrimSpace(h.CustomNginx) != "" {
		r.note("host %s: custom nginx snippet kept but not run by Relay Edge (it applies again with nginx)", hostLabel(h))
	}
	if h.GeoBlock.Enabled && len(h.GeoBlock.AllowCountries) > 0 {
		r.note("host %s: geo-blocking by country is not supported by Relay Edge and is skipped", hostLabel(h))
	}
}

func hostLabel(h *model.ProxyHost) string {
	for _, d := range h.Domains {
		if d = strings.TrimSpace(d); d != "" {
			return d
		}
	}
	return h.ID
}

// blocklist returns the valid global deny entries, sorted and deduplicated.
func (r *renderer) blocklist() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, e := range r.snap.Blocklist.Entries {
		cidr := strings.TrimSpace(e.CIDR)
		if !validCIDR(cidr) || seen[cidr] {
			continue
		}
		seen[cidr] = true
		out = append(out, cidr)
	}
	// nginx's geo block is a set; sorting after trimming keeps the output stable.
	sort.Strings(out)
	return out
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

var idSafeRe = regexp.MustCompile(`[^a-z0-9_]+`)

// safeID turns an entity id into a file-name-safe token (same as the nginx renderer).
func safeID(id string) string {
	s := idSafeRe.ReplaceAllString(strings.ToLower(id), "_")
	if s == "" {
		return "x"
	}
	return s
}

func validCIDR(s string) bool {
	if _, _, err := net.ParseCIDR(s); err == nil {
		return true
	}
	return net.ParseIP(s) != nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// oneLine replaces line breaks like the nginx renderer does for comments.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
