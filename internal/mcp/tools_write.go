package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// ---------------------------------------------------------------- args

type createHostArgs struct {
	Domains       []string `json:"domains" jsonschema:"Domains the host answers to, e.g. [grafana.home.lan]. Leading wildcards (*.example.com) are allowed"`
	Upstream      string   `json:"upstream" jsonschema:"Where to proxy to, e.g. http://10.0.0.21:3000 or https://nas.lan:5001"`
	Websockets    *bool    `json:"websockets,omitempty" jsonschema:"Allow websocket upgrades (default from host defaults)"`
	AccessList    string   `json:"accessList,omitempty" jsonschema:"Access list name or id, or none (default from host defaults)"`
	Certificate   string   `json:"certificate,omitempty" jsonschema:"Certificate id or name, auto to pick a valid certificate covering all domains (e.g. a *.home.lan wildcard), or none for HTTP only. Default: the default certificate, else auto"`
	ForceHTTPS    *bool    `json:"forceHttps,omitempty" jsonschema:"Redirect HTTP to HTTPS (only with a certificate; default from host defaults)"`
	BlockExploits *bool    `json:"blockExploits,omitempty" jsonschema:"Block common exploit paths (default from host defaults)"`
	HTTP2         *bool    `json:"http2,omitempty" jsonschema:"Enable HTTP/2 (default from host defaults)"`
	CacheAssets   *bool    `json:"cacheAssets,omitempty" jsonschema:"Cache static assets"`
	Enabled       *bool    `json:"enabled,omitempty" jsonschema:"Whether the host is enabled (default true)"`
	Reason        string   `json:"reason,omitempty" jsonschema:"Why this change is needed; shown to the person approving it"`
}

type updateHostArgs struct {
	Host          string   `json:"host" jsonschema:"Host id or any of its current domains"`
	Domains       []string `json:"domains,omitempty" jsonschema:"Replace the host's domains"`
	Upstream      string   `json:"upstream,omitempty" jsonschema:"New upstream, e.g. http://10.0.0.21:3000"`
	Websockets    *bool    `json:"websockets,omitempty" jsonschema:"Allow websocket upgrades"`
	AccessList    string   `json:"accessList,omitempty" jsonschema:"Access list name or id, or none to remove it"`
	Certificate   string   `json:"certificate,omitempty" jsonschema:"Certificate id or name, auto, or none for HTTP only"`
	ForceHTTPS    *bool    `json:"forceHttps,omitempty" jsonschema:"Redirect HTTP to HTTPS (needs a certificate)"`
	BlockExploits *bool    `json:"blockExploits,omitempty" jsonschema:"Block common exploit paths"`
	HTTP2         *bool    `json:"http2,omitempty" jsonschema:"Enable HTTP/2"`
	CacheAssets   *bool    `json:"cacheAssets,omitempty" jsonschema:"Cache static assets"`
	Enabled       *bool    `json:"enabled,omitempty" jsonschema:"Enable or disable the host"`
	Reason        string   `json:"reason,omitempty" jsonschema:"Why this change is needed; shown to the person approving it"`
}

type deleteHostArgs struct {
	Host   string `json:"host" jsonschema:"Host id or any of its domains"`
	Reason string `json:"reason,omitempty" jsonschema:"Why the host should be deleted; shown to the person approving it"`
}

type drainServerArgs struct {
	Backend string `json:"backend" jsonschema:"Backend name or id, e.g. minio"`
	Server  string `json:"server" jsonschema:"Server address (10.0.0.92), address:port (10.0.0.92:9000), load balancer server name or id"`
	State   string `json:"state,omitempty" jsonschema:"drain (default) stops new sessions and lets existing ones finish; maint takes the server out entirely; ready puts it back into rotation"`
	Reason  string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type requestCertArgs struct {
	Domains     []string `json:"domains" jsonschema:"Domains for the certificate, e.g. [vault.home.lan] or [*.home.lan]"`
	Challenge   string   `json:"challenge,omitempty" jsonschema:"ACME challenge. Default: dns-01 for wildcards, otherwise the preferred challenge from TLS settings"`
	DNSProvider string   `json:"dnsProvider,omitempty" jsonschema:"DNS provider name or id for dns-01 (optional when only one is configured)"`
	Staging     bool     `json:"staging,omitempty" jsonschema:"Use Let's Encrypt staging (untrusted test certificates)"`
	AutoRenew   *bool    `json:"autoRenew,omitempty" jsonschema:"Renew automatically before expiry (default true)"`
	Reason      string   `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type applyArgs struct {
	Summary string `json:"summary,omitempty" jsonschema:"Short description recorded on the new config version"`
	Reason  string `json:"reason,omitempty" jsonschema:"Why now; shown to the person approving it"`
}

func (s *Service) registerWriteTools() {
	addWrite(s, toolInfo{Name: "create_host", Title: "Create a proxy host",
		Description: "Create a reverse proxy (nginx or Relay Edge) host for one or more domains pointing at an upstream. Unspecified options use Relay's host defaults; certificate \"auto\" picks a valid certificate covering the domains (for example a wildcard). The host is saved to pending changes and is not live until apply_changes runs. May wait for human approval."},
		nil, false, s.planCreateHost)
	addWrite(s, toolInfo{Name: "update_host", Title: "Update a proxy host",
		Description: "Change an existing proxy host (found by id or domain): domains, upstream, websockets, access list, certificate, force HTTPS, exploit blocking, HTTP/2, asset caching or enabled. Only the fields you pass change. Saved to pending changes; not live until apply_changes. May wait for human approval."},
		nil, false, s.planUpdateHost)
	addWrite(s, toolInfo{Name: "delete_host", Title: "Delete a proxy host",
		Description: "Delete a proxy host by id or domain. Saved to pending changes; the host keeps serving until apply_changes runs. Relay's own admin host cannot be deleted. May wait for human approval."},
		nil, true, s.planDeleteHost)
	addWrite(s, toolInfo{Name: "drain_server", Title: "Drain a backend server",
		Description: "Change a load balancer server's admin state at runtime: drain (finish existing sessions, accept no new ones), maint (remove) or ready (back in rotation). Takes effect immediately through the load balancer's runtime API (HAProxy or Relay Balancer) and is remembered on the backend — no apply needed. May wait for human approval."},
		map[string][]any{"state": {model.ServerStateDrain, model.ServerStateMaint, model.ServerStateReady}}, true, s.planDrainServer)
	addWrite(s, toolInfo{Name: "request_certificate", Title: "Request a TLS certificate",
		Description: "Request a Let's Encrypt certificate for one or more domains (wildcards need dns-01 and a configured DNS provider). Issuance runs in the background; check list_certificates for the result, then attach it with update_host. May wait for human approval."},
		map[string][]any{"challenge": {model.ChallengeHTTP01, model.ChallengeDNS01}}, false, s.planRequestCertificate)
	addWrite(s, toolInfo{Name: "apply_changes", Title: "Apply pending changes",
		Description: "Make all pending configuration changes live: render the reverse proxy (nginx or Relay Edge) and load balancer (HAProxy or Relay Balancer) config, validate, reload, health-check for 10 s and roll back automatically on failure. Returns the new config version. Affects every pending change, not only yours — check get_pending_changes first. May wait for human approval."},
		nil, true, s.planApply)
	s.registerEntityTools()
	s.registerExtWriteTools()
}

// ---------------------------------------------------------------- hosts

// hostSettings is the reviewable subset of a host used for previews.
type hostSettings struct {
	Domains       []string `json:"domains"`
	Enabled       bool     `json:"enabled"`
	Upstream      string   `json:"upstream"`
	Websockets    bool     `json:"websockets"`
	BlockExploits bool     `json:"blockExploits"`
	CacheAssets   bool     `json:"cacheAssets"`
	HTTP2         bool     `json:"http2"`
	Certificate   string   `json:"certificate"`
	ForceHTTPS    bool     `json:"forceHttps"`
	AccessList    string   `json:"accessList"`
}

func settingsOf(h *model.ProxyHost, l *lookups) hostSettings {
	hs := hostSettings{Domains: h.Domains, Enabled: h.Enabled, Upstream: upstreamString(h.Upstream), Websockets: h.Websockets,
		BlockExploits: h.BlockExploits, CacheAssets: h.CacheAssets, HTTP2: h.HTTP2, ForceHTTPS: h.ForceHTTPS, Certificate: "none", AccessList: "none"}
	if h.CertificateID != "" {
		hs.Certificate = h.CertificateID
		if c := l.certs[h.CertificateID]; c != nil {
			hs.Certificate = c.Name
		}
	}
	if h.AccessListID != "" {
		hs.AccessList = h.AccessListID
		if al := l.lists[h.AccessListID]; al != nil {
			hs.AccessList = al.Name
		}
	}
	return hs
}

func (s *Service) syntheticRequest(ctx context.Context, method, path string) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, method, "http://relay.internal"+path, nil)
	a := core.ActorFrom(ctx)
	if a.IP != "" {
		r.RemoteAddr = net.JoinHostPort(a.IP, "0")
	}
	return r
}

// resolveCertificate turns a certificate argument into a certificate id.
func (s *Service) resolveCertificate(ctx context.Context, ref string, domains []string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case "none", "":
		return "", nil
	case "auto":
		certs, err := s.app.Store.Certificates().List(ctx)
		if err != nil {
			return "", err
		}
		if c := matchCertificate(certs, domains, s.now()); c != nil {
			return c.ID, nil
		}
		return "", nil
	}
	c, err := s.findCertificate(ctx, strings.TrimSpace(ref))
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func (s *Service) resolveAccessList(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "none") {
		return "", nil
	}
	al, err := s.findAccessList(ctx, ref)
	if err != nil {
		return "", err
	}
	return al.ID, nil
}

// checkHost normalizes, validates and checks domain uniqueness and scope.
func (s *Service) checkHost(ctx context.Context, c *call, prev, next *model.ProxyHost) error {
	if n, ok := any(next).(interface{ Normalize() }); ok {
		n.Normalize()
	}
	if !c.scope.allowsAll(next.Domains) {
		return fmt.Errorf("this token may only manage hosts matching %s", c.scope)
	}
	if hk := httpx.HostHooks.BeforeSave; hk != nil {
		method := http.MethodPost
		if prev != nil {
			method = http.MethodPut
		}
		if err := hk(s.syntheticRequest(ctx, method, "/api/hosts"), prev, next); err != nil {
			return err
		}
	}
	if v, ok := any(next).(model.Validator); ok {
		if err := v.Validate(); err != nil {
			return err
		}
	}
	return s.domainConflicts(ctx, next.Domains, next.ID)
}

// domainConflicts enforces that a domain belongs to at most one host or redirect.
func (s *Service) domainConflicts(ctx context.Context, domains []string, selfID string) error {
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if h.ID == selfID {
			continue
		}
		for _, d := range domains {
			for _, od := range h.Domains {
				if strings.EqualFold(d, od) {
					return fmt.Errorf("%s is already used by proxy host %s", d, first(h.Domains))
				}
			}
		}
	}
	redirects, err := s.app.Store.Redirects().List(ctx)
	if err != nil {
		return err
	}
	for _, rd := range redirects {
		for _, d := range domains {
			for _, od := range rd.Domains {
				if strings.EqualFold(d, od) {
					return fmt.Errorf("%s is already used by a redirect to %s", d, rd.To)
				}
			}
		}
	}
	return nil
}

func (s *Service) planCreateHost(ctx context.Context, c *call, in createHostArgs) (*plan, error) {
	if len(in.Domains) == 0 {
		return nil, errors.New("domains is required, e.g. [\"grafana.home.lan\"]")
	}
	up, err := parseUpstream(in.Upstream)
	if err != nil {
		return nil, err
	}
	general, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral)
	if err != nil {
		return nil, err
	}
	def := general.Defaults
	h := &model.ProxyHost{
		Domains: in.Domains, Enabled: true, Upstream: up,
		Websockets: def.Websockets, BlockExploits: def.BlockExploits, HTTP2: def.HTTP2, ForceHTTPS: def.ForceHTTPS,
		AccessListID: def.AccessListID, HSTS: "inherit", Locations: []model.Location{},
		Source: model.SourceMCP, SourceRef: c.actor.ClientName,
	}
	if n, ok := any(h).(interface{ Normalize() }); ok {
		n.Normalize() // lowercases domains before certificate matching
	}
	h.ForceHTTPS = def.ForceHTTPS
	if in.Websockets != nil {
		h.Websockets = *in.Websockets
	}
	if in.BlockExploits != nil {
		h.BlockExploits = *in.BlockExploits
	}
	if in.HTTP2 != nil {
		h.HTTP2 = *in.HTTP2
	}
	if in.CacheAssets != nil {
		h.CacheAssets = *in.CacheAssets
	}
	if in.Enabled != nil {
		h.Enabled = *in.Enabled
	}
	if in.AccessList != "" {
		if h.AccessListID, err = s.resolveAccessList(ctx, in.AccessList); err != nil {
			return nil, err
		}
	}
	certRef := in.Certificate
	if strings.TrimSpace(certRef) == "" {
		certRef = "auto"
		if def.CertificateID != "" {
			if _, err := s.app.Store.Certificates().Get(ctx, def.CertificateID); err == nil {
				certRef = def.CertificateID
			}
		}
	}
	if h.CertificateID, err = s.resolveCertificate(ctx, certRef, h.Domains); err != nil {
		return nil, err
	}
	if in.ForceHTTPS != nil {
		h.ForceHTTPS = *in.ForceHTTPS
		if *in.ForceHTTPS && h.CertificateID == "" {
			return nil, errors.New("forceHttps needs a certificate: pass certificate (id, name or auto) or request one with request_certificate")
		}
	}
	if err := s.checkHost(ctx, c, nil, h); err != nil {
		return nil, err
	}
	l, err := s.lookups(ctx)
	if err != nil {
		return nil, err
	}
	view := settingsOf(h, l)
	name := first(h.Domains)
	summary := fmt.Sprintf("Create proxy host %s → %s", bold(strings.Join(h.Domains, ", ")), bold(view.Upstream))
	if view.Certificate != "none" {
		summary += " with certificate " + bold(view.Certificate)
	} else {
		summary += " (HTTP only)"
	}
	return &plan{
		Summary: summary,
		Target:  name,
		Preview: diffJSON(nil, view),
		Detail:  joinDetail(name+" → "+view.Upstream, "added to pending"),
		Exec: func(ctx context.Context) (*outcome, error) {
			next := *h
			if err := s.app.Store.Hosts().Create(ctx, &next); err != nil {
				if errors.Is(err, store.ErrConflict) {
					return nil, errors.New("a host with this id already exists")
				}
				return nil, err
			}
			s.app.Changed(ctx, model.KindHost, next.ID, name, core.ActionCreated)
			if hk := httpx.HostHooks.AfterSave; hk != nil {
				hk(s.syntheticRequest(ctx, http.MethodPost, "/api/hosts"), nil, &next)
			}
			tls := "HTTP only"
			if view.Certificate != "none" {
				tls = "TLS with " + view.Certificate
			}
			text := fmt.Sprintf("Created proxy host %s → %s (%s, id %s). %s", strings.Join(next.Domains, ", "), view.Upstream, tls, next.ID, pendingNote)
			return &outcome{Text: text, Structured: map[string]any{"host": s.hostView(&next, l), "pending": true}}, nil
		},
	}, nil
}

func (s *Service) planUpdateHost(ctx context.Context, c *call, in updateHostArgs) (*plan, error) {
	prev, err := s.findHost(ctx, in.Host)
	if err != nil {
		return nil, err
	}
	if !c.scope.allowsAll(prev.Domains) {
		return nil, fmt.Errorf("no proxy host matches %q — use list_hosts to find it", in.Host)
	}
	if prev.System {
		return nil, errors.New("Relay's own admin host can't be changed over MCP — use Settings → General")
	}
	var next model.ProxyHost
	data, _ := json.Marshal(prev)
	if err := json.Unmarshal(data, &next); err != nil {
		return nil, err
	}
	if len(in.Domains) > 0 {
		next.Domains = in.Domains
	}
	if strings.TrimSpace(in.Upstream) != "" {
		up, err := parseUpstream(in.Upstream)
		if err != nil {
			return nil, err
		}
		next.Upstream = up
	}
	if in.Websockets != nil {
		next.Websockets = *in.Websockets
	}
	if in.BlockExploits != nil {
		next.BlockExploits = *in.BlockExploits
	}
	if in.HTTP2 != nil {
		next.HTTP2 = *in.HTTP2
	}
	if in.CacheAssets != nil {
		next.CacheAssets = *in.CacheAssets
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.AccessList != "" {
		if next.AccessListID, err = s.resolveAccessList(ctx, in.AccessList); err != nil {
			return nil, err
		}
	}
	if in.Certificate != "" {
		domains := next.Domains
		for i := range domains {
			domains[i] = model.HostNormalizeDomain(domains[i])
		}
		if next.CertificateID, err = s.resolveCertificate(ctx, in.Certificate, domains); err != nil {
			return nil, err
		}
		if strings.EqualFold(in.Certificate, "auto") && next.CertificateID == "" {
			return nil, errors.New("no valid certificate covers these domains — request one with request_certificate")
		}
	}
	if in.ForceHTTPS != nil {
		next.ForceHTTPS = *in.ForceHTTPS
		if *in.ForceHTTPS && next.CertificateID == "" {
			return nil, errors.New("forceHttps needs a certificate")
		}
	}
	if err := s.checkHost(ctx, c, prev, &next); err != nil {
		return nil, err
	}
	l, err := s.lookups(ctx)
	if err != nil {
		return nil, err
	}
	before, after := settingsOf(prev, l), settingsOf(&next, l)
	changes := hostChanges(before, after)
	if len(changes) == 0 {
		return nil, fmt.Errorf("nothing to change: %s already has these settings", first(prev.Domains))
	}
	name := first(next.Domains)
	return &plan{
		Summary: fmt.Sprintf("Update %s: %s", bold(first(prev.Domains)), strings.Join(changes, ", ")),
		Target:  name,
		Preview: diffJSON(before, after),
		Detail:  joinDetail(name, strings.Join(changes, ", ")),
		Exec: func(ctx context.Context) (*outcome, error) {
			n := next
			if err := s.app.Store.Hosts().Update(ctx, &n); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("host %s no longer exists", first(prev.Domains))
				}
				return nil, err
			}
			s.app.Changed(ctx, model.KindHost, n.ID, name, core.ActionUpdated)
			if hk := httpx.HostHooks.AfterSave; hk != nil {
				hk(s.syntheticRequest(ctx, http.MethodPut, "/api/hosts/"+n.ID), prev, &n)
			}
			text := fmt.Sprintf("Updated %s: %s. %s", name, strings.Join(changes, ", "), pendingNote)
			return &outcome{Text: text, Structured: map[string]any{"host": s.hostView(&n, l), "changes": changes, "pending": true}}, nil
		},
	}, nil
}

func hostChanges(a, b hostSettings) []string {
	var out []string
	if strings.Join(a.Domains, ",") != strings.Join(b.Domains, ",") {
		out = append(out, "domains → "+strings.Join(b.Domains, ", "))
	}
	if a.Upstream != b.Upstream {
		out = append(out, "upstream → "+b.Upstream)
	}
	flag := func(name string, x, y bool) {
		if x != y {
			out = append(out, name+" "+onOff(y))
		}
	}
	flag("enabled", a.Enabled, b.Enabled)
	flag("websockets", a.Websockets, b.Websockets)
	flag("block exploits", a.BlockExploits, b.BlockExploits)
	flag("HTTP/2", a.HTTP2, b.HTTP2)
	flag("cache assets", a.CacheAssets, b.CacheAssets)
	if a.Certificate != b.Certificate {
		out = append(out, "certificate → "+b.Certificate)
	}
	flag("force HTTPS", a.ForceHTTPS, b.ForceHTTPS)
	if a.AccessList != b.AccessList {
		out = append(out, "access list → "+b.AccessList)
	}
	return out
}

func (s *Service) planDeleteHost(ctx context.Context, c *call, in deleteHostArgs) (*plan, error) {
	h, err := s.findHost(ctx, in.Host)
	if err != nil {
		return nil, err
	}
	if !c.scope.allowsAll(h.Domains) {
		return nil, fmt.Errorf("no proxy host matches %q — use list_hosts to find it", in.Host)
	}
	if h.System {
		return nil, errors.New("Relay's own admin host can't be deleted")
	}
	if hk := httpx.HostHooks.BeforeDelete; hk != nil {
		if err := hk(s.syntheticRequest(ctx, http.MethodDelete, "/api/hosts/"+h.ID), h); err != nil {
			return nil, err
		}
	}
	l, err := s.lookups(ctx)
	if err != nil {
		return nil, err
	}
	name := first(h.Domains)
	cur := *h
	return &plan{
		Summary: fmt.Sprintf("Delete proxy host %s (→ %s)", bold(name), upstreamString(h.Upstream)),
		Target:  name,
		Preview: diffJSON(settingsOf(h, l), nil),
		Detail:  joinDetail(name, "removed in pending"),
		Exec: func(ctx context.Context) (*outcome, error) {
			if err := s.app.Store.Hosts().Delete(ctx, cur.ID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("host %s no longer exists", name)
				}
				return nil, err
			}
			s.app.Changed(ctx, model.KindHost, cur.ID, name, core.ActionDeleted)
			if hk := httpx.HostHooks.AfterDelete; hk != nil {
				hk(s.syntheticRequest(ctx, http.MethodDelete, "/api/hosts/"+cur.ID), &cur)
			}
			return &outcome{Text: fmt.Sprintf("Deleted proxy host %s. It keeps serving until the change is applied. %s", name, pendingNote),
				Structured: map[string]any{"deleted": cur.ID, "domains": cur.Domains, "pending": true}}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- load balancer

func (s *Service) planDrainServer(ctx context.Context, c *call, in drainServerArgs) (*plan, error) {
	state := strings.ToLower(strings.TrimSpace(in.State))
	if state == "" {
		state = model.ServerStateDrain
	}
	switch state {
	case model.ServerStateDrain, model.ServerStateMaint, model.ServerStateReady:
	default:
		return nil, fmt.Errorf("state must be drain, maint or ready, not %q", in.State)
	}
	b, err := s.findBackend(ctx, in.Backend)
	if err != nil {
		return nil, err
	}
	if !c.scope.allows(b.Name) {
		return nil, fmt.Errorf("no backend named %q — use list_backends to see them", in.Backend)
	}
	sv, err := findServer(b, in.Server)
	if err != nil {
		return nil, err
	}
	cur := sv.State
	if cur == "" {
		cur = model.ServerStateReady
	}
	addr := net.JoinHostPort(sv.Address, strconv.Itoa(sv.Port))
	if cur == state {
		return nil, fmt.Errorf("server %s in backend %s is already in state %s", addr, b.Name, state)
	}
	verb := map[string]string{
		model.ServerStateDrain: "Drain %s in backend %s",
		model.ServerStateMaint: "Put %s in backend %s into maintenance",
		model.ServerStateReady: "Put %s in backend %s back into rotation",
	}[state]
	backendID, serverID, backendName := b.ID, sv.ID, b.Name
	return &plan{
		Summary: fmt.Sprintf(verb, bold(addr), bold(b.Name)),
		Target:  b.Name + " / " + sv.Address,
		Preview: fmt.Sprintf("backend %s\n- server %s %s state %s\n+ server %s %s state %s", b.Name, sv.Name, addr, cur, sv.Name, addr, state),
		Detail:  fmt.Sprintf("%s → %s", addr, state),
		Exec: func(ctx context.Context) (*outcome, error) {
			if s.app.LB == nil {
				return nil, unavailable(core.ErrNotImplemented, "the load balancer")
			}
			if err := s.app.LB.SetServerState(ctx, backendID, serverID, state); err != nil {
				return nil, unavailable(err, "the load balancer")
			}
			text := map[string]string{
				model.ServerStateDrain: "Server %s in backend %s is draining: existing sessions finish, no new ones are sent.",
				model.ServerStateMaint: "Server %s in backend %s is in maintenance and receives no traffic.",
				model.ServerStateReady: "Server %s in backend %s is back in rotation.",
			}[state]
			return &outcome{Text: fmt.Sprintf(text, addr, backendName) + " Applied at runtime and saved on the backend — no apply needed.",
				Structured: map[string]any{"backend": backendName, "server": addr, "previousState": cur, "state": state}}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- certificates

func (s *Service) planRequestCertificate(ctx context.Context, c *call, in requestCertArgs) (*plan, error) {
	domains := []string{}
	seen := map[string]bool{}
	for _, d := range in.Domains {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if !model.ValidCertDomain(d) || strings.Count(d, "*") > 1 || (strings.Contains(d, "*") && !strings.HasPrefix(d, "*.")) {
			return nil, fmt.Errorf("%q is not a valid certificate domain", d)
		}
		domains = append(domains, d)
	}
	if len(domains) == 0 {
		return nil, errors.New("domains is required, e.g. [\"vault.home.lan\"]")
	}
	if !c.scope.allowsAll(domains) {
		return nil, fmt.Errorf("this token may only request certificates for %s", c.scope)
	}
	wildcard := false
	for _, d := range domains {
		wildcard = wildcard || strings.HasPrefix(d, "*.")
	}
	challenge := strings.ToLower(strings.TrimSpace(in.Challenge))
	if challenge == "" {
		if wildcard {
			challenge = model.ChallengeDNS01
		} else {
			tls, err := store.LoadSettings[model.TLSSettings](ctx, s.app.Store, model.SettingsTLS)
			if err != nil {
				return nil, err
			}
			challenge = tls.PreferredChallenge
			if challenge != model.ChallengeDNS01 {
				challenge = model.ChallengeHTTP01
			}
		}
	}
	switch challenge {
	case model.ChallengeHTTP01:
		if wildcard {
			return nil, errors.New("wildcard domains need the dns-01 challenge")
		}
	case model.ChallengeDNS01:
	default:
		return nil, fmt.Errorf("challenge must be http-01 or dns-01, not %q", in.Challenge)
	}
	var provider *model.DNSProvider
	if challenge == model.ChallengeDNS01 {
		if strings.TrimSpace(in.DNSProvider) != "" {
			p, err := s.findDNSProvider(ctx, strings.TrimSpace(in.DNSProvider))
			if err != nil {
				return nil, err
			}
			provider = p
		} else {
			providers, err := s.app.Store.DNSProviders().List(ctx)
			if err != nil {
				return nil, err
			}
			if len(providers) != 1 {
				return nil, fmt.Errorf("dns-01 needs a DNS provider: pass dnsProvider (%d configured)", len(providers))
			}
			provider = &providers[0]
		}
	}
	autoRenew := true
	if in.AutoRenew != nil {
		autoRenew = *in.AutoRenew
	}
	req := core.CertRequest{Domains: domains, Challenge: challenge, Staging: in.Staging, AutoRenew: autoRenew}
	how := "HTTP-01"
	if provider != nil {
		req.DNSProviderID = provider.ID
		how = "DNS-01 via " + bold(provider.Name)
	}
	env := "Let's Encrypt"
	if in.Staging {
		env = "Let's Encrypt staging"
	}
	bolded := make([]string, len(domains))
	for i, d := range domains {
		bolded[i] = bold(d)
	}
	preview := []string{"+ certificate " + strings.Join(domains, ", "), "+   provider   " + env, "+   challenge  " + plain(how), fmt.Sprintf("+   autoRenew  %v", autoRenew)}
	return &plan{
		Summary: fmt.Sprintf("Request a %s certificate for %s (%s)", env, strings.Join(bolded, ", "), how),
		Target:  first(domains),
		Preview: strings.Join(preview, "\n"),
		Detail:  joinDetail(strings.Join(domains, ", "), plain(how)),
		Exec: func(ctx context.Context) (*outcome, error) {
			if s.app.Certs == nil {
				return nil, unavailable(core.ErrNotImplemented, "certificate issuance")
			}
			cert, err := s.app.Certs.Request(ctx, req)
			if err != nil {
				return nil, unavailable(err, "certificate issuance")
			}
			return &outcome{
				Text: fmt.Sprintf("Certificate requested for %s (status %s, id %s). Issuance runs in the background — check list_certificates, then attach it with update_host.",
					strings.Join(domains, ", "), cert.Status, cert.ID),
				Structured: map[string]any{"certificate": cert},
			}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- apply

func (s *Service) planApply(ctx context.Context, c *call, in applyArgs) (*plan, error) {
	p, err := s.pending(ctx)
	if err != nil {
		return nil, err
	}
	if p.Count == 0 && len(p.Items) == 0 {
		return nil, fmt.Errorf("there are no pending changes to apply (live config is v%d)", p.LiveVersion)
	}
	lines := []string{}
	for _, it := range p.Items {
		if !c.scope.allows(it.Name) {
			return nil, fmt.Errorf("pending changes include %s, which is outside this token's scope — apply them in Relay", it.Name)
		}
		sign := "~"
		switch it.Action {
		case core.ActionCreated:
			sign = "+"
		case core.ActionDeleted:
			sign = "-"
		}
		lines = append(lines, sign+" "+pendingLine(it))
	}
	count := p.Count
	if count == 0 {
		count = len(p.Items)
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = "Applied via MCP by " + c.actor.ClientName
	}
	return &plan{
		Summary: fmt.Sprintf("Apply %s on top of live v%d", bold(fmt.Sprintf("%d pending change(s)", count)), p.LiveVersion),
		Target:  fmt.Sprintf("%d changes", count),
		Preview: strings.Join(lines, "\n"),
		Detail:  summary,
		Exec: func(ctx context.Context) (*outcome, error) {
			if s.app.Engine == nil {
				return nil, unavailable(core.ErrNotImplemented, "applying changes")
			}
			v, err := s.app.Engine.Apply(ctx, core.ApplyOptions{Summary: summary})
			if err != nil {
				return nil, unavailable(err, "applying changes")
			}
			id := v.ID
			out := &outcome{Structured: map[string]any{"version": v}, Version: &id}
			switch v.Status {
			case "failed", "rolled_back":
				out.IsError = true
				out.Text = fmt.Sprintf("Apply failed as v%d (%s): %s. The previous configuration stays live.", v.ID, v.Status, v.Error)
			default:
				out.Text = fmt.Sprintf("Applied %d change(s) as v%d (%s · validate %d ms · reload %d ms).", len(v.Changes), v.ID, v.Status, v.ValidateMs, v.ReloadMs)
			}
			return out, nil
		},
	}, nil
}
