package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const kvManaged = "ops.docker.managed"

// managedHost tracks a proxy host created from Docker.
type managedHost struct {
	Endpoint  string `json:"endpoint,omitempty"` // endpoint id ("" = legacy local)
	Container string `json:"container"`
	Auto      bool   `json:"auto"`           // created from labels (eligible for auto-remove)
	Sync      bool   `json:"sync"`           // keep upstream in sync (bulk dialog option)
	Labels    string `json:"labels"`         // label hash last applied
	Removed   bool   `json:"removed"`        // deleted by a user; only recreate when labels change
	Port      int    `json:"port,omitempty"` // chosen container port (0 = unknown)
	Link      bool   `json:"link,omitempty"` // disabled host waiting for the container to start
}

type managedServer struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Container string `json:"container"`
	BackendID string `json:"backendId"`
	ServerID  string `json:"serverId"`
	Auto      bool   `json:"auto"`
	Labels    string `json:"labels"`
	Removed   bool   `json:"removed"`
}

func endpointOf(id string) string {
	if id == "" {
		return LocalEndpointID
	}
	return id
}

type managedState struct {
	Hosts   map[string]*managedHost   `json:"hosts"`   // host id
	Servers map[string]*managedServer `json:"servers"` // backend id + "/" + server id
}

func (s *Service) loadManaged(ctx context.Context) *managedState {
	m := &managedState{Hosts: map[string]*managedHost{}, Servers: map[string]*managedServer{}}
	if b, err := s.app.Store.GetKV(ctx, kvManaged); err == nil {
		_ = json.Unmarshal(b, m)
	}
	if m.Hosts == nil {
		m.Hosts = map[string]*managedHost{}
	}
	if m.Servers == nil {
		m.Servers = map[string]*managedServer{}
	}
	return m
}

func (s *Service) saveManaged(ctx context.Context, m *managedState) {
	b, _ := json.Marshal(m)
	if err := s.app.Store.PutKV(context.WithoutCancel(ctx), kvManaged, b); err != nil {
		s.app.Log.Warn("docker managed state", "err", err)
	}
}

// dockerRequest builds a synthetic request so CRUD hooks of other slices run
// for background changes made by the docker actor.
func dockerRequest(ctx context.Context, method, path string) *http.Request {
	r, _ := http.NewRequestWithContext(core.WithActor(ctx, core.DockerActor), method, path, nil)
	return r
}

func validate(v any) error {
	if val, ok := v.(model.Validator); ok {
		return val.Validate()
	}
	return nil
}

// ---------------------------------------------------------------- helpers

// domainOwner returns the host or redirect that already serves one of domains.
func domainOwner(ctx context.Context, st *store.Store, domains []string, exceptHostID string) (string, error) {
	want := map[string]bool{}
	for _, d := range domains {
		want[strings.ToLower(d)] = true
	}
	hosts, err := st.Hosts().List(ctx)
	if err != nil {
		return "", err
	}
	for _, h := range hosts {
		if h.ID == exceptHostID {
			continue
		}
		for _, d := range h.Domains {
			if want[strings.ToLower(d)] {
				return fmt.Sprintf("%s is already used by host %s", d, h.Domains[0]), nil
			}
		}
	}
	redirects, err := st.Redirects().List(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range redirects {
		for _, d := range r.Domains {
			if want[strings.ToLower(d)] {
				return fmt.Sprintf("%s is already used by a redirect", d), nil
			}
		}
	}
	return "", nil
}

// certCovers reports whether a certificate's names cover domain.
func certCovers(names []string, domain string) bool {
	domain = strings.ToLower(domain)
	for _, n := range names {
		n = strings.ToLower(n)
		if n == domain {
			return true
		}
		if strings.HasPrefix(n, "*.") {
			suffix := n[1:]
			if strings.HasSuffix(domain, suffix) && !strings.Contains(strings.TrimSuffix(domain, suffix), ".") {
				return true
			}
		}
	}
	return false
}

func coversAll(c model.Certificate, domains []string) bool {
	for _, d := range domains {
		if !certCovers(c.Domains, d) {
			return false
		}
	}
	return len(domains) > 0
}

// pickCertificate chooses a certificate for new docker hosts:
// "off" → none; explicit default when it covers the domains; otherwise a
// valid (wildcard) certificate covering them; "letsencrypt" requests one.
func (s *Service) pickCertificate(ctx context.Context, tlsMode string, domains []string, set model.DockerSettings) (string, string) {
	if tlsMode == "off" {
		return "", ""
	}
	certs, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return "", ""
	}
	if set.DefaultCertID != "" {
		for _, c := range certs {
			if c.ID == set.DefaultCertID && c.Status != model.CertStatusFailed && coversAll(c, domains) {
				return c.ID, ""
			}
		}
	}
	for _, c := range certs {
		if c.Status == model.CertStatusValid && coversAll(c, domains) {
			return c.ID, ""
		}
	}
	if tlsMode == "letsencrypt" {
		for _, c := range certs {
			if c.Status == model.CertStatusPending && coversAll(c, domains) {
				return c.ID, ""
			}
		}
		if s.app.Certs == nil {
			return "", "certificate service unavailable"
		}
		tls, _ := store.LoadSettings[model.TLSSettings](ctx, s.app.Store, model.SettingsTLS)
		cert, err := s.app.Certs.Request(core.WithActor(ctx, core.DockerActor), core.CertRequest{
			Domains: domains, Challenge: tls.PreferredChallenge, AutoRenew: true, Staging: tls.ACMEProvider == model.CertLetsEncryptStaging,
		})
		if err != nil {
			return "", "certificate request failed: " + err.Error()
		}
		return cert.ID, ""
	}
	return "", ""
}

func (s *Service) accessListByName(ctx context.Context, name string) (string, bool) {
	if name == "" {
		return "", true
	}
	lists, err := s.app.Store.AccessLists().List(ctx)
	if err != nil {
		return "", false
	}
	for _, l := range lists {
		if strings.EqualFold(l.Name, name) {
			return l.ID, true
		}
	}
	return "", false
}

// newHost builds a host for a container from host + docker defaults.
func newHost(g model.GeneralSettings, set model.DockerSettings, c core.Container, domains []string, port int, scheme string) model.ProxyHost {
	if scheme == "" {
		scheme = containerScheme(c)
	}
	access := set.DefaultAccessListID
	if access == "" {
		access = g.Defaults.AccessListID
	}
	return model.ProxyHost{
		Domains:       domains,
		Enabled:       true,
		Upstream:      model.Upstream{Scheme: scheme, Host: c.UpstreamHost, Port: port},
		Websockets:    g.Defaults.Websockets,
		BlockExploits: g.Defaults.BlockExploits,
		AccessListID:  access,
		HTTP2:         g.Defaults.HTTP2,
		HSTS:          "inherit",
		Locations:     []model.Location{},
		GeoBlock:      model.GeoBlock{AllowCountries: []string{}},
		Source:        model.SourceDocker,
		SourceRef:     sourceRef(c),
	}
}

// hostOwnedBy reports whether a docker host belongs to container c.
func hostOwnedBy(h model.ProxyHost, c core.Container) bool {
	return h.Source == model.SourceDocker && (h.SourceRef == sourceRef(c) || (h.SourceRef == c.Name && c.EndpointID == LocalEndpointID))
}

// saveHost runs the generic CRUD pipeline (hooks → Validate → store → audit →
// Changed → AfterSave) for a host.
func (s *Service) saveHost(r *http.Request, prev, next *model.ProxyHost, detail string) error {
	ctx := r.Context()
	if httpx.HostHooks.BeforeSave != nil {
		if err := httpx.HostHooks.BeforeSave(r, prev, next); err != nil {
			return err
		}
	}
	if err := validate(next); err != nil {
		return err
	}
	action, verb := core.ActionCreated, "host.create"
	if prev == nil {
		if err := s.app.Store.Hosts().Create(ctx, next); err != nil {
			return err
		}
	} else {
		action, verb = core.ActionUpdated, "host.update"
		if err := s.app.Store.Hosts().Update(ctx, next); err != nil {
			return err
		}
	}
	name := next.Domains[0]
	s.app.Audit(ctx, core.AuditEntry{Action: verb, Target: name, Detail: detail, Result: "saved"})
	s.app.Changed(ctx, model.KindHost, next.ID, name, action)
	if httpx.HostHooks.AfterSave != nil {
		httpx.HostHooks.AfterSave(r, prev, next)
	}
	return nil
}

func (s *Service) saveBackend(r *http.Request, prev, next *model.Backend, detail string) error {
	ctx := r.Context()
	if httpx.BackendHooks.BeforeSave != nil {
		if err := httpx.BackendHooks.BeforeSave(r, prev, next); err != nil {
			return err
		}
	}
	if err := validate(next); err != nil {
		return err
	}
	if err := s.app.Store.Backends().Update(ctx, next); err != nil {
		return err
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "backend.update", Target: next.Name, Detail: detail, Result: "saved"})
	s.app.Changed(ctx, model.KindBackend, next.ID, next.Name, core.ActionUpdated)
	if httpx.BackendHooks.AfterSave != nil {
		httpx.BackendHooks.AfterSave(r, prev, next)
	}
	return nil
}

func cloneHost(h *model.ProxyHost) *model.ProxyHost {
	b, _ := json.Marshal(h)
	var c model.ProxyHost
	_ = json.Unmarshal(b, &c)
	return &c
}

func cloneBackend(v *model.Backend) *model.Backend {
	b, _ := json.Marshal(v)
	var c model.Backend
	_ = json.Unmarshal(b, &c)
	return &c
}

func onEndpoint(where string, c core.Container) string {
	if c.EndpointID == LocalEndpointID || c.EndpointName == "" {
		return where
	}
	return where + " on " + c.EndpointName
}

// ---------------------------------------------------------------- reconcile

func (s *Service) warnOnce(ctx context.Context, subject, msg string) {
	s.warnMu.Lock()
	seen := s.labelErrors[subject] == msg
	s.labelErrors[subject] = msg
	s.warnMu.Unlock()
	if seen {
		return
	}
	s.app.Log.Warn("docker labels", "container", subject, "problem", msg)
	s.app.Activity(context.WithoutCancel(ctx), "docker.labels", "warn", "Docker labels on "+subject+" not applied", subject, msg)
}

// reconcile applies relay.* labels of running containers (endpoint
// AutoCreate) and keeps upstreams of managed hosts/servers in sync.
func (s *Service) reconcile(ctx context.Context, conn *endpointConn, ep model.DockerEndpoint, list []core.Container, set model.DockerSettings, initial bool) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	m := s.loadManaged(ctx)
	changed := false
	g, _ := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral)
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		changed = s.linkPending(ctx, m, c) || changed
		spec, ok, err := ParseLabels(c.Labels)
		if err != nil {
			s.warnOnce(ctx, sourceRef(c), err.Error())
		}
		if ok && ep.AutoCreate {
			hash := labelHash(c.Labels)
			switch {
			case c.UpstreamHost == "" || c.SuggestedPort == 0:
				reason := c.Reason
				if reason == "" {
					reason = "no reachable address"
				}
				s.warnOnce(ctx, sourceRef(c), "relay labels are set but the container can't be proxied: "+reason)
			case spec.Backend != "":
				changed = s.ensureServer(ctx, m, c, spec, hash) || changed
			case len(spec.Domains) > 0:
				changed = s.ensureHost(ctx, m, g, set, c, spec, hash) || changed
			}
		}
		changed = s.syncUpstreams(ctx, m, set, c, spec) || changed
	}
	if initial && ep.AutoRemove && conn != nil {
		s.scheduleOrphans(ctx, conn, ep, m)
	}
	if changed {
		s.saveManaged(ctx, m)
	}
}

func (s *Service) ensureHost(ctx context.Context, m *managedState, g model.GeneralSettings, set model.DockerSettings, c core.Container, spec LabelSpec, hash string) bool {
	var entryID string
	var entry *managedHost
	for id, e := range m.Hosts {
		if endpointOf(e.Endpoint) == c.EndpointID && e.Container == c.Name && e.Auto {
			entryID, entry = id, e
		}
	}
	port := c.SuggestedPort
	scheme := spec.Scheme
	r := dockerRequest(ctx, http.MethodPost, "/api/hosts")
	detail := "from labels on container " + onEndpoint(c.Name, c)

	if entry != nil {
		if entry.Labels == hash {
			return false
		}
		host, err := s.app.Store.Hosts().Get(ctx, entryID)
		if err != nil {
			// Host was deleted by a user; labels changed since, so create again.
			delete(m.Hosts, entryID)
			entry = nil
		} else {
			if owner, _ := domainOwner(ctx, s.app.Store, spec.Domains, host.ID); owner != "" {
				s.warnOnce(ctx, sourceRef(c), owner)
				return false
			}
			next := cloneHost(host)
			next.Domains = spec.Domains
			next.Upstream.Port = port
			next.Upstream.Host = c.UpstreamHost
			next.Upstream.Scheme = scheme
			if next.Upstream.Scheme == "" {
				next.Upstream.Scheme = containerScheme(c)
			}
			next.SourceRef = sourceRef(c)
			if spec.Websockets != nil {
				next.Websockets = *spec.Websockets
			}
			if spec.Access != "" {
				if id, found := s.accessListByName(ctx, spec.Access); found {
					next.AccessListID = id
				} else {
					s.warnOnce(ctx, sourceRef(c), "access list "+spec.Access+" does not exist")
				}
			}
			if !coversAllIDs(ctx, s.app.Store, next.CertificateID, spec.Domains) || spec.TLS == "off" {
				certID, problem := s.pickCertificate(ctx, spec.TLS, spec.Domains, set)
				if problem != "" {
					s.warnOnce(ctx, sourceRef(c), problem)
				}
				next.CertificateID = certID
				next.ForceHTTPS = certID != "" && g.Defaults.ForceHTTPS
			}
			if err := s.saveHost(r, host, next, "labels changed on container "+onEndpoint(c.Name, c)); err != nil {
				s.warnOnce(ctx, sourceRef(c), err.Error())
				return false
			}
			entry.Labels = hash
			s.app.Activity(ctx, "host.updated", "info", "Host updated from Docker labels", next.Domains[0], "container "+onEndpoint(c.Name, c))
			return true
		}
	}

	if owner, _ := domainOwner(ctx, s.app.Store, spec.Domains, ""); owner != "" {
		// Already proxied (e.g. created via the suggestions dialog) — not an error.
		hosts, _ := s.app.Store.Hosts().List(ctx)
		for _, h := range hosts {
			if hostOwnedBy(h, c) {
				return false
			}
		}
		s.warnOnce(ctx, sourceRef(c), owner)
		return false
	}
	h := newHost(g, set, c, spec.Domains, port, scheme)
	if spec.Websockets != nil {
		h.Websockets = *spec.Websockets
	}
	if spec.Access != "" {
		if id, found := s.accessListByName(ctx, spec.Access); found {
			h.AccessListID = id
		} else {
			s.warnOnce(ctx, sourceRef(c), "access list "+spec.Access+" does not exist; host created without it")
		}
	}
	certID, problem := s.pickCertificate(ctx, spec.TLS, spec.Domains, set)
	if problem != "" {
		s.warnOnce(ctx, sourceRef(c), problem)
	}
	h.CertificateID = certID
	h.ForceHTTPS = certID != "" && g.Defaults.ForceHTTPS
	if err := s.saveHost(r, nil, &h, detail); err != nil {
		s.warnOnce(ctx, sourceRef(c), "could not create host: "+err.Error())
		return false
	}
	m.Hosts[h.ID] = &managedHost{Endpoint: c.EndpointID, Container: c.Name, Auto: true, Sync: set.KeepInSync, Labels: hash, Port: containerPortFor(c, port)}
	s.app.Activity(ctx, "host.created", "ok", "Host created from Docker labels", h.Domains[0], "container "+onEndpoint(c.Name, c))
	return true
}

func coversAllIDs(ctx context.Context, st *store.Store, certID string, domains []string) bool {
	if certID == "" {
		return false
	}
	c, err := st.Certificates().Get(ctx, certID)
	return err == nil && coversAll(*c, domains)
}

func (s *Service) ensureServer(ctx context.Context, m *managedState, c core.Container, spec LabelSpec, hash string) bool {
	backends, err := s.app.Store.Backends().List(ctx)
	if err != nil {
		return false
	}
	var backend *model.Backend
	for i := range backends {
		if strings.EqualFold(backends[i].Name, spec.Backend) {
			backend = &backends[i]
		}
	}
	if backend == nil {
		s.warnOnce(ctx, sourceRef(c), "backend "+spec.Backend+" does not exist")
		return false
	}
	port := c.SuggestedPort
	for _, e := range m.Servers {
		if endpointOf(e.Endpoint) == c.EndpointID && e.Container == c.Name && e.Auto && e.BackendID == backend.ID && e.Labels == hash {
			return false
		}
	}
	for _, srv := range backend.Servers {
		if srv.Address == c.UpstreamHost && srv.Port == port {
			m.Servers[backend.ID+"/"+srv.ID] = &managedServer{Endpoint: c.EndpointID, Container: c.Name, BackendID: backend.ID, ServerID: srv.ID, Auto: true, Labels: hash}
			return true
		}
	}
	next := cloneBackend(backend)
	srv := newServer(next, c, port)
	next.Servers = append(next.Servers, srv)
	r := dockerRequest(ctx, http.MethodPut, "/api/backends/"+backend.ID)
	if err := s.saveBackend(r, backend, next, "added server "+srv.Name+" from labels on container "+onEndpoint(c.Name, c)); err != nil {
		s.warnOnce(ctx, sourceRef(c), "could not add to backend "+backend.Name+": "+err.Error())
		return false
	}
	m.Servers[backend.ID+"/"+srv.ID] = &managedServer{Endpoint: c.EndpointID, Container: c.Name, BackendID: backend.ID, ServerID: srv.ID, Auto: true, Labels: hash}
	s.app.Activity(ctx, "backend.server_added", "ok", "Container joined backend "+backend.Name, sourceRef(c), fmt.Sprintf("%s:%d from Docker labels", c.UpstreamHost, port))
	return true
}

func newServer(b *model.Backend, c core.Container, port int) model.Server {
	name := serverName(c.Name)
	if c.EndpointID != LocalEndpointID && c.EndpointName != "" {
		name = serverName(c.EndpointName + "-" + c.Name)
	}
	for _, s := range b.Servers {
		if s.Name == name {
			name = name + "-" + store.NewID()[:4]
		}
	}
	return model.Server{
		ID: store.NewID(), Name: name, Address: c.UpstreamHost, Port: port, Weight: 100, Role: model.ServerActive,
		Check: b.HealthCheck.Type != "" && b.HealthCheck.Type != "none", State: model.ServerStateReady,
	}
}

// syncUpstreams updates hosts/servers of a recreated container (new address).
func (s *Service) syncUpstreams(ctx context.Context, m *managedState, set model.DockerSettings, c core.Container, spec LabelSpec) bool {
	if c.UpstreamHost == "" {
		return false
	}
	changed := false
	ports := upstreamPorts(c)
	for id, e := range m.Hosts {
		if endpointOf(e.Endpoint) != c.EndpointID || e.Container != c.Name || e.Removed || e.Link {
			continue
		}
		if !(e.Sync || (e.Auto && set.KeepInSync)) {
			continue
		}
		host, err := s.app.Store.Hosts().Get(ctx, id)
		if err != nil {
			continue
		}
		port := host.Upstream.Port
		switch {
		case spec.Port > 0:
			port = c.SuggestedPort
		case e.Port > 0 && upstreamPortFor(c, e.Port) > 0:
			port = upstreamPortFor(c, e.Port) // the port the user picked, re-mapped
		case !ports[port]:
			port = c.SuggestedPort
		}
		if port == 0 {
			continue
		}
		if host.Upstream.Host == c.UpstreamHost && host.Upstream.Port == port {
			continue
		}
		next := cloneHost(host)
		next.Upstream.Host, next.Upstream.Port = c.UpstreamHost, port
		r := dockerRequest(ctx, http.MethodPut, "/api/hosts/"+id)
		if err := s.saveHost(r, host, next, fmt.Sprintf("container %s recreated · upstream %s:%d", onEndpoint(c.Name, c), c.UpstreamHost, port)); err != nil {
			s.warnOnce(ctx, sourceRef(c), "could not sync upstream: "+err.Error())
			continue
		}
		changed = true
	}
	if !set.KeepInSync {
		return changed
	}
	for key, e := range m.Servers {
		if endpointOf(e.Endpoint) != c.EndpointID || e.Container != c.Name || e.Removed {
			continue
		}
		backend, err := s.app.Store.Backends().Get(ctx, e.BackendID)
		if err != nil {
			delete(m.Servers, key)
			changed = true
			continue
		}
		next := cloneBackend(backend)
		updated := false
		for i := range next.Servers {
			srv := &next.Servers[i]
			if srv.ID != e.ServerID {
				continue
			}
			port := srv.Port
			if (spec.Port > 0 || !ports[port]) && c.SuggestedPort > 0 {
				port = c.SuggestedPort
			}
			if srv.Address != c.UpstreamHost || srv.Port != port {
				srv.Address, srv.Port = c.UpstreamHost, port
				updated = true
			}
		}
		if !updated {
			continue
		}
		r := dockerRequest(ctx, http.MethodPut, "/api/backends/"+backend.ID)
		if err := s.saveBackend(r, backend, next, fmt.Sprintf("container %s recreated · server address %s", onEndpoint(c.Name, c), c.UpstreamHost)); err != nil {
			s.warnOnce(ctx, sourceRef(c), "could not sync backend server: "+err.Error())
			continue
		}
		changed = true
	}
	return changed
}

// ---------------------------------------------------------------- removal

func removalKey(endpointID, name string) string { return endpointID + "\x00" + name }

func (s *Service) scheduleRemoval(endpointID, name string, at time.Time) {
	s.syncMu.Lock()
	s.removals[removalKey(endpointID, name)] = at
	s.syncMu.Unlock()
}

func (s *Service) cancelRemoval(endpointID, name string) {
	s.syncMu.Lock()
	delete(s.removals, removalKey(endpointID, name))
	s.syncMu.Unlock()
}

// scheduleOrphans queues removal for auto-created entries whose container no
// longer exists (destroyed while Relay was not watching). Caller holds syncMu.
func (s *Service) scheduleOrphans(ctx context.Context, conn *endpointConn, ep model.DockerEndpoint, m *managedState) {
	names := map[string]bool{}
	for _, e := range m.Hosts {
		if e.Auto && !e.Removed && endpointOf(e.Endpoint) == ep.ID {
			names[e.Container] = true
		}
	}
	for _, e := range m.Servers {
		if e.Auto && !e.Removed && endpointOf(e.Endpoint) == ep.ID {
			names[e.Container] = true
		}
	}
	for name := range names {
		if exists, err := conn.containerExists(ctx, name); err == nil && !exists {
			s.removals[removalKey(ep.ID, name)] = time.Time{}
		}
	}
}

// processRemovals deletes auto-created hosts/servers of destroyed containers
// (after a grace period, so `docker compose up` recreation does not churn).
func (s *Service) processRemovals(ctx context.Context) {
	set := s.settings(ctx)
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if len(s.removals) == 0 {
		return
	}
	m := s.loadManaged(ctx)
	changed := false
	for key, at := range s.removals {
		if time.Since(at) < s.removeDelay {
			continue
		}
		epID, name, _ := strings.Cut(key, "\x00")
		var ep *model.DockerEndpoint
		for i := range set.Endpoints {
			if set.Endpoints[i].ID == epID {
				ep = &set.Endpoints[i]
			}
		}
		if ep == nil || !set.Enabled || !ep.Enabled || !ep.AutoRemove {
			delete(s.removals, key)
			continue
		}
		conn := s.conn(epID)
		if conn == nil {
			continue
		}
		exists, err := conn.containerExists(ctx, name)
		if err != nil {
			continue
		}
		delete(s.removals, key)
		if exists {
			continue
		}
		where := name
		if epID != LocalEndpointID {
			where = name + " on " + ep.Name
		}
		for id, e := range m.Hosts {
			if endpointOf(e.Endpoint) != epID || e.Container != name || !e.Auto {
				continue
			}
			delete(m.Hosts, id)
			changed = true
			host, err := s.app.Store.Hosts().Get(ctx, id)
			if err != nil {
				continue
			}
			r := dockerRequest(ctx, http.MethodDelete, "/api/hosts/"+id)
			if httpx.HostHooks.BeforeDelete != nil {
				if err := httpx.HostHooks.BeforeDelete(r, host); err != nil {
					s.app.Log.Warn("docker auto-remove blocked", "host", host.Domains[0], "err", err)
					continue
				}
			}
			if err := s.app.Store.Hosts().Delete(ctx, id); err != nil {
				continue
			}
			rctx := r.Context()
			s.app.Audit(rctx, core.AuditEntry{Action: "host.delete", Target: host.Domains[0], Detail: "container " + where + " was removed", Result: "saved"})
			s.app.Changed(rctx, model.KindHost, id, host.Domains[0], core.ActionDeleted)
			if httpx.HostHooks.AfterDelete != nil {
				httpx.HostHooks.AfterDelete(r, host)
			}
			s.app.Activity(rctx, "host.deleted", "info", "Host removed with its container", host.Domains[0], "container "+where)
		}
		for skey, e := range m.Servers {
			if endpointOf(e.Endpoint) != epID || e.Container != name || !e.Auto {
				continue
			}
			delete(m.Servers, skey)
			changed = true
			backend, err := s.app.Store.Backends().Get(ctx, e.BackendID)
			if err != nil {
				continue
			}
			next := cloneBackend(backend)
			next.Servers = slices.DeleteFunc(next.Servers, func(srv model.Server) bool { return srv.ID == e.ServerID })
			if len(next.Servers) == len(backend.Servers) {
				continue
			}
			r := dockerRequest(ctx, http.MethodPut, "/api/backends/"+backend.ID)
			if err := s.saveBackend(r, backend, next, "removed server of container "+where); err != nil {
				s.app.Log.Warn("docker auto-remove server", "backend", backend.Name, "err", err)
			}
		}
	}
	if changed {
		s.saveManaged(ctx, m)
	}
}

func (s *Service) handleRename(ctx context.Context, ep model.DockerEndpoint, oldName, newName string) {
	if oldName == "" || newName == "" || oldName == newName {
		return
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	m := s.loadManaged(ctx)
	changed := false
	for id, e := range m.Hosts {
		if endpointOf(e.Endpoint) != ep.ID || e.Container != oldName {
			continue
		}
		e.Container = newName
		changed = true
		if host, err := s.app.Store.Hosts().Get(ctx, id); err == nil && (host.SourceRef == ep.Name+"/"+oldName || host.SourceRef == oldName) {
			host.SourceRef = ep.Name + "/" + newName
			_ = s.app.Store.Hosts().Update(ctx, host)
		}
	}
	for _, e := range m.Servers {
		if endpointOf(e.Endpoint) == ep.ID && e.Container == oldName {
			e.Container = newName
			changed = true
		}
	}
	if changed {
		s.saveManaged(ctx, m)
	}
}

// ---------------------------------------------------------------- bulk create (29c)

type CreateItem struct {
	EndpointID  string `json:"endpointId"`
	ContainerID string `json:"containerId"`
	Domain      string `json:"domain"`
	Port        int    `json:"port"`
	Scheme      string `json:"scheme"`
}

type CreateRequest struct {
	Items         []CreateItem `json:"items"`
	CertificateID string       `json:"certificateId,omitempty"`
	AccessListID  string       `json:"accessListId,omitempty"`
	KeepInSync    bool         `json:"keepInSync"`
}

type CreatedHost struct {
	EndpointID  string `json:"endpointId"`
	ContainerID string `json:"containerId"`
	HostID      string `json:"hostId"`
	Domain      string `json:"domain"`
	Enabled     bool   `json:"enabled"`
	LinkOnStart bool   `json:"linkOnStart,omitempty"` // created disabled; enabled when the container starts
}

type ItemError struct {
	EndpointID  string            `json:"endpointId"`
	ContainerID string            `json:"containerId"`
	Domain      string            `json:"domain"`
	Error       string            `json:"error"`
	Fields      map[string]string `json:"fields,omitempty"`
}

type CreateResult struct {
	Created []CreatedHost `json:"created"`
	Errors  []ItemError   `json:"errors"`
}

func findContainer(list []core.Container, endpointID, id string) (core.Container, bool) {
	for _, c := range list {
		if endpointID != "" && c.EndpointID != endpointID {
			continue
		}
		if c.ID == id || (len(id) >= 12 && strings.HasPrefix(c.ID, id)) || c.Name == id {
			return c, true
		}
	}
	return core.Container{}, false
}

// CreateHosts creates proxy hosts for selected containers (bulk dialog).
func (s *Service) CreateHosts(r *http.Request, req CreateRequest) (*CreateResult, error) {
	ctx := r.Context()
	if !s.Status(ctx).Connected {
		return nil, httpx.Errorf(http.StatusServiceUnavailable, "docker_unavailable", "Docker is not connected")
	}
	if len(req.Items) == 0 {
		return nil, httpx.Errorf(http.StatusBadRequest, "bad_request", "select at least one container")
	}
	list, err := s.Containers(ctx)
	if err != nil {
		return nil, httpx.Errorf(http.StatusBadGateway, "docker_error", err.Error())
	}
	set := s.settings(ctx)
	g, _ := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral)
	res := &CreateResult{Created: []CreatedHost{}, Errors: []ItemError{}}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	m := s.loadManaged(ctx)
	batch := map[string]bool{}
	for _, it := range req.Items {
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(it.Domain)), ".")
		fail := func(msg string, fields map[string]string) {
			res.Errors = append(res.Errors, ItemError{EndpointID: it.EndpointID, ContainerID: it.ContainerID, Domain: domain, Error: msg, Fields: fields})
		}
		c, ok := findContainer(list, it.EndpointID, it.ContainerID)
		if !ok {
			fail("container no longer exists", nil)
			continue
		}
		if c.UpstreamHost == "" && !c.LinkOnStart {
			reason := c.Reason
			if reason == "" {
				reason = "no reachable address"
			}
			fail("can't proxy "+c.Name+": "+reason, nil)
			continue
		}
		if domain == "" || !hostnameRe.MatchString(domain) {
			fail("enter a valid domain", map[string]string{"domains.0": "invalid domain"})
			continue
		}
		if batch[domain] {
			fail(domain+" is used twice in this selection", nil)
			continue
		}
		if owner, err := domainOwner(ctx, s.app.Store, []string{domain}, ""); err != nil {
			return nil, err
		} else if owner != "" {
			fail(owner, map[string]string{"domains.0": owner})
			continue
		}
		port := it.Port
		if port == 0 {
			port = c.SuggestedPort
		}
		if port < 1 || port > 65535 {
			fail("choose the port to proxy to", map[string]string{"upstream.port": "required"})
			continue
		}
		scheme := strings.ToLower(it.Scheme)
		if scheme != "http" && scheme != "https" {
			scheme = portScheme(c, port)
		}
		h := newHost(g, set, c, []string{domain}, port, scheme)
		// Stopped local container without an address: create the host disabled
		// with a placeholder upstream; linkPending enables it on start.
		link := c.UpstreamHost == ""
		detail := "from Docker container " + onEndpoint(c.Name, c)
		if link {
			h.Enabled = false
			h.Upstream.Host = placeholderHost(c.Name)
			detail = "from stopped Docker container " + onEndpoint(c.Name, c) + " · enabled when it starts"
		}
		if req.AccessListID != "" {
			h.AccessListID = req.AccessListID
		}
		certID := req.CertificateID
		if certID == "" {
			certID, _ = s.pickCertificate(ctx, "auto", h.Domains, set)
		}
		h.CertificateID = certID
		h.ForceHTTPS = certID != "" && g.Defaults.ForceHTTPS
		if err := s.saveHost(r, nil, &h, detail); err != nil {
			var fields map[string]string
			if ve, ok := err.(*model.ValidationError); ok {
				fields = ve.Fields
			}
			fail(err.Error(), fields)
			continue
		}
		batch[domain] = true
		m.Hosts[h.ID] = &managedHost{Endpoint: c.EndpointID, Container: c.Name, Auto: false, Sync: req.KeepInSync, Port: containerPortFor(c, port), Link: link}
		res.Created = append(res.Created, CreatedHost{EndpointID: c.EndpointID, ContainerID: c.ID, HostID: h.ID, Domain: domain, Enabled: h.Enabled, LinkOnStart: link})
	}
	if len(res.Created) > 0 {
		s.saveManaged(ctx, m)
		names := make([]string, len(res.Created))
		for i, c := range res.Created {
			names[i] = c.Domain
		}
		s.app.Activity(ctx, "host.created", "ok", fmt.Sprintf("%d hosts created from Docker", len(res.Created)), strings.Join(names, ", "), "")
		s.publish("hosts_created", "")
	}
	return res, nil
}

// AddToBackend adds a container as a server of a backend.
func (s *Service) AddToBackend(r *http.Request, backendID, endpointID, containerID string, port int) (*model.Backend, error) {
	ctx := r.Context()
	list, err := s.Containers(ctx)
	if err != nil {
		return nil, httpx.Errorf(http.StatusServiceUnavailable, "docker_unavailable", err.Error())
	}
	c, ok := findContainer(list, endpointID, containerID)
	if !ok {
		return nil, httpx.Errorf(http.StatusNotFound, "not_found", "container not found")
	}
	if c.UpstreamHost == "" {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "unreachable", "can't reach "+c.Name+": "+c.Reason)
	}
	backend, err := s.app.Store.Backends().Get(ctx, backendID)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		port = c.SuggestedPort
	}
	if port < 1 || port > 65535 {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "invalid", "choose the container port")
	}
	for _, srv := range backend.Servers {
		if srv.Address == c.UpstreamHost && srv.Port == port {
			return nil, httpx.Errorf(http.StatusConflict, "conflict", fmt.Sprintf("%s:%d is already a server of %s", c.UpstreamHost, port, backend.Name))
		}
	}
	next := cloneBackend(backend)
	srv := newServer(next, c, port)
	srv.Port = port
	next.Servers = append(next.Servers, srv)
	if err := s.saveBackend(r, backend, next, "added server "+srv.Name+" from Docker container "+onEndpoint(c.Name, c)); err != nil {
		return nil, err
	}
	s.syncMu.Lock()
	m := s.loadManaged(ctx)
	m.Servers[backend.ID+"/"+srv.ID] = &managedServer{Endpoint: c.EndpointID, Container: c.Name, BackendID: backend.ID, ServerID: srv.ID}
	s.saveManaged(ctx, m)
	s.syncMu.Unlock()
	s.publish("server_added", c.Name)
	return next, nil
}
