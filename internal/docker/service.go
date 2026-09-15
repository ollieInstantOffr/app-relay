// Package docker discovers containers on one or more Docker hosts and keeps
// label-driven proxy hosts in sync (slice: ops).
package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"

	"github.com/instantoffr/relay/internal/core"
	relayevents "github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const cacheTTL = 5 * time.Second

// dockerAPI is the subset of the Docker client Relay uses.
type dockerAPI interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	ServerVersion(ctx context.Context) (types.Version, error)
	Close() error
}

// Status is GET /api/docker/status. Top-level fields aggregate all endpoints.
type Status struct {
	Enabled       bool             `json:"enabled"`
	Connected     bool             `json:"connected"`
	Endpoint      string           `json:"endpoint"`
	Version       string           `json:"version"`
	APIVersion    string           `json:"apiVersion,omitempty"`
	Containers    int              `json:"containers"`
	Running       int              `json:"running"`
	Error         string           `json:"error,omitempty"`
	SocketMissing bool             `json:"socketMissing,omitempty"`
	CheckedAt     *time.Time       `json:"checkedAt,omitempty"`
	Endpoints     []EndpointStatus `json:"endpoints"`
}

type Service struct {
	app  *core.App
	dial func(ctx context.Context, ep model.DockerEndpoint) (*dialed, error)

	mu     sync.Mutex
	conns  map[string]*endpointConn
	reconf chan struct{}

	syncMu      sync.Mutex
	warnMu      sync.Mutex
	removals    map[string]time.Time // endpoint id \x00 container name → destroyed at
	labelErrors map[string]string    // endpoint/container → last reported label problem
	removeDelay time.Duration
}

func New(app *core.App) *Service {
	return &Service{
		app:         app,
		dial:        dialEndpoint,
		conns:       map[string]*endpointConn{},
		reconf:      make(chan struct{}, 1),
		removals:    map[string]time.Time{},
		labelErrors: map[string]string{},
		removeDelay: 20 * time.Second,
	}
}

func (s *Service) Start(ctx context.Context) error {
	s.migrate(ctx)
	go s.supervise(ctx)
	go s.watchBus(ctx)
	return nil
}

// migrate persists the endpoint list converted from legacy settings.
func (s *Service) migrate(ctx context.Context) {
	set, err := store.LoadSettings[model.DockerSettings](ctx, s.app.Store, model.SettingsDocker)
	if err != nil {
		return
	}
	if NormalizeSettings(&set) {
		set.Endpoint = legacyEndpoint(set)
		if err := s.app.Store.PutSettings(ctx, model.SettingsDocker, set); err != nil {
			s.app.Log.Warn("docker settings migration", "err", err)
			return
		}
		s.app.Log.Info("docker settings migrated to endpoints", "endpoints", len(set.Endpoints))
	}
}

func (s *Service) settings(ctx context.Context) model.DockerSettings {
	v, err := store.LoadSettings[model.DockerSettings](ctx, s.app.Store, model.SettingsDocker)
	if err != nil {
		v = store.DefaultDocker()
	}
	NormalizeSettings(&v)
	return v
}

// Reconfigure reloads settings and (re)connects changed endpoints.
func (s *Service) Reconfigure() {
	select {
	case s.reconf <- struct{}{}:
	default:
	}
}

func (s *Service) supervise(ctx context.Context) {
	s.apply(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.reconf:
			s.apply(ctx)
		}
	}
}

func (s *Service) apply(ctx context.Context) {
	set := s.settings(ctx)
	want := map[string]model.DockerEndpoint{}
	if set.Enabled {
		for _, ep := range set.Endpoints {
			if ep.Enabled {
				want[ep.ID] = ep
			}
		}
	}
	s.mu.Lock()
	for id, c := range s.conns {
		ep, ok := want[id]
		if !ok || endpointKey(ep) != c.getKey() {
			c.stop()
			delete(s.conns, id)
			continue
		}
		c.updateMeta(ep)
	}
	for id, ep := range want {
		if s.conns[id] == nil {
			c := newConn(s, ep)
			s.conns[id] = c
			c.start(ctx)
		}
	}
	s.mu.Unlock()
	s.publish("reconfigured", "")
}

func (s *Service) conn(id string) *endpointConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id]
}

// connList returns the active connections in settings order.
func (s *Service) connList(set model.DockerSettings) []*endpointConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*endpointConn{}
	for _, ep := range set.Endpoints {
		if c := s.conns[ep.ID]; c != nil {
			out = append(out, c)
		}
	}
	return out
}

func (s *Service) publish(action, name string) {
	s.app.Bus.Publish(relayevents.DockerChanged, map[string]string{"action": action, "container": name})
}

// Status reports every endpoint plus aggregate top-level fields.
func (s *Service) Status(ctx context.Context) Status {
	set := s.settings(ctx)
	st := Status{Enabled: set.Enabled, Endpoint: legacyEndpoint(set), Endpoints: []EndpointStatus{}}
	var firstErr string
	missing, enabledCount := false, 0
	for _, ep := range set.Endpoints {
		es := EndpointStatus{ID: ep.ID, Name: ep.Name, Type: ep.Type, URL: ep.URL, UpstreamAddress: effectiveUpstream(ep),
			Enabled: set.Enabled && ep.Enabled, SSHFingerprint: ep.SSHKnownHost}
		if c := s.conn(ep.ID); c != nil && es.Enabled {
			cs := c.getStatus()
			es.Connected, es.Version, es.APIVersion = cs.Connected, cs.Version, cs.APIVersion
			es.Containers, es.Running, es.Error, es.SocketMissing, es.CheckedAt = cs.Containers, cs.Running, cs.Error, cs.SocketMissing, cs.CheckedAt
			if cs.SSHFingerprint != "" {
				es.SSHFingerprint = cs.SSHFingerprint
			}
		}
		if es.Enabled {
			enabledCount++
			if es.Connected {
				if !st.Connected {
					st.Endpoint, st.Version, st.APIVersion = ep.URL, es.Version, es.APIVersion
				}
				st.Connected = true
				st.Containers += es.Containers
				st.Running += es.Running
			} else if es.Error != "" && firstErr == "" {
				firstErr = fmt.Sprintf("%s: %s", ep.Name, es.Error)
			}
			missing = missing || es.SocketMissing
			if es.CheckedAt != nil && (st.CheckedAt == nil || es.CheckedAt.After(*st.CheckedAt)) {
				st.CheckedAt = es.CheckedAt
			}
		}
		st.Endpoints = append(st.Endpoints, es)
	}
	if !st.Connected {
		st.Error = firstErr
		st.SocketMissing = missing
		if set.Enabled && enabledCount == 0 {
			st.Error = "no Docker host configured"
			now := time.Now()
			st.CheckedAt = &now
		}
	}
	return st
}

// Retry reconnects one endpoint (or all) now and returns the status after
// the attempt.
func (s *Service) Retry(ctx context.Context, endpointID string) Status {
	start := time.Now()
	set := s.settings(ctx)
	targets := []*endpointConn{}
	for _, c := range s.connList(set) {
		if endpointID == "" || c.getEP().ID == endpointID {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		s.Reconfigure()
	}
	for _, c := range targets {
		c.kickNow()
	}
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return s.Status(ctx)
		case <-deadline:
			return s.Status(ctx)
		case <-tick.C:
			if len(targets) == 0 {
				targets = s.connList(set)
				if len(targets) == 0 {
					continue
				}
			}
			done := true
			for _, c := range targets {
				if st := c.getStatus(); st.CheckedAt == nil || !st.CheckedAt.After(start) {
					done = false
				}
			}
			if done {
				time.Sleep(50 * time.Millisecond)
				return s.Status(ctx)
			}
		}
	}
}

// TestResult is POST /api/docker/endpoints/test.
type TestResult struct {
	OK             bool   `json:"ok"`
	Version        string `json:"version,omitempty"`
	Containers     int    `json:"containers"`
	Running        int    `json:"running"`
	Error          string `json:"error,omitempty"`
	SSHFingerprint string `json:"sshFingerprint,omitempty"`
}

// TestEndpoint connects to an (unsaved) endpoint once. Stored secrets of the
// endpoint with the same id fill blanks.
func (s *Service) TestEndpoint(ctx context.Context, ep model.DockerEndpoint) (TestResult, error) {
	set := s.settings(ctx)
	ep.Name = strings.ToLower(strings.TrimSpace(ep.Name))
	if ep.Name == "" {
		ep.Name = "test"
	}
	if ep.Type == "" {
		ep.Type = typeFromURL(ep.URL)
	}
	KeepEndpointSecrets(set.Endpoints, &ep)
	if errs := ValidateEndpoint(ep); len(errs) > 0 {
		return TestResult{}, errs.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	d, err := s.dial(ctx, ep)
	if err != nil {
		res := TestResult{Error: err.Error()}
		var mismatch *HostKeyMismatchError
		if errors.As(err, &mismatch) {
			res.SSHFingerprint = mismatch.Got
		}
		return res, nil
	}
	defer d.Close()
	res := TestResult{Version: d.version.Version, SSHFingerprint: d.fingerprint}
	list, err := d.api.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		res.Error = "connected, but listing containers failed: " + err.Error()
		return res, nil
	}
	res.OK = true
	res.Containers = len(list)
	for _, c := range list {
		if string(c.State) == "running" {
			res.Running++
		}
	}
	return res, nil
}

// trustHostKey stores the SSH host key fingerprint seen on first connect.
func (s *Service) trustHostKey(ctx context.Context, c *endpointConn, fingerprint string) {
	c.mu.Lock()
	c.ep.SSHKnownHost = fingerprint
	c.key = endpointKey(c.ep)
	id, name := c.ep.ID, c.ep.Name
	c.mu.Unlock()
	set, err := store.LoadSettings[model.DockerSettings](ctx, s.app.Store, model.SettingsDocker)
	if err != nil {
		return
	}
	NormalizeSettings(&set)
	for i := range set.Endpoints {
		if set.Endpoints[i].ID == id && set.Endpoints[i].SSHKnownHost == "" {
			set.Endpoints[i].SSHKnownHost = fingerprint
			if err := s.app.Store.PutSettings(ctx, model.SettingsDocker, set); err != nil {
				s.app.Log.Warn("store ssh host key", "endpoint", name, "err", err)
				return
			}
			s.app.Audit(ctx, core.AuditEntry{Actor: &core.SystemActor, Action: "docker.endpoint.trust_host_key", Target: name, Detail: fingerprint, Result: "ok"})
			s.app.Activity(ctx, "docker.host_key", "info", "Trusted SSH host key of Docker host "+name, name, fingerprint)
		}
	}
}

// RegisterSettingsHook migrates, validates and redacts docker settings and
// reconnects on change.
func (s *Service) RegisterSettingsHook() {
	httpx.SettingsHooks[model.SettingsDocker] = &httpx.SettingsHook{
		Decorate: func(r *http.Request, v any) any {
			set := *(v.(*model.DockerSettings))
			NormalizeSettings(&set)
			RedactSettings(&set)
			return set
		},
		BeforeSave: func(r *http.Request, prev, next any) error {
			return prepareSettings(prev.(*model.DockerSettings), next.(*model.DockerSettings))
		},
		AfterSave: func(r *http.Request, prev, next any) {
			p := *(prev.(*model.DockerSettings))
			NormalizeSettings(&p)
			n := next.(*model.DockerSettings)
			for _, old := range p.Endpoints {
				for _, ep := range n.Endpoints {
					if ep.ID == old.ID && ep.Name != old.Name {
						s.renameEndpointRefs(r.Context(), old, ep)
					}
				}
			}
			RedactSettings(n)
			s.Reconfigure()
		},
	}
}

func prepareSettings(prev, next *model.DockerSettings) error {
	NormalizeSettings(prev)
	errs := model.Errs{}
	next.DomainPattern = strings.TrimSpace(next.DomainPattern)
	if next.DomainPattern == "" {
		next.DomainPattern = store.DefaultDocker().DomainPattern
	}
	if !strings.Contains(next.DomainPattern, "{name}") {
		errs.Add("domainPattern", "must contain {name}")
	}
	if next.Endpoints == nil && next.Endpoint != "" && len(prev.Endpoints) > 0 {
		// Old client that only knows the single endpoint field.
		next.Endpoints = prev.Endpoints
	}
	NormalizeSettings(next)
	names := map[string]int{}
	for i := range next.Endpoints {
		ep := &next.Endpoints[i]
		ep.Name = strings.ToLower(strings.TrimSpace(ep.Name))
		ep.URL = strings.TrimSpace(ep.URL)
		ep.UpstreamAddress = strings.TrimSpace(ep.UpstreamAddress)
		ep.SSHKnownHost = strings.TrimSpace(ep.SSHKnownHost)
		KeepEndpointSecrets(prev.Endpoints, ep)
		for field, msg := range ValidateEndpoint(*ep) {
			errs.Add(fmt.Sprintf("endpoints.%d.%s", i, field), "%s", msg)
		}
		if j, dup := names[ep.Name]; dup {
			errs.Add(fmt.Sprintf("endpoints.%d.name", i), "name already used by Docker host %d", j+1)
		}
		names[ep.Name] = i
	}
	next.Endpoint = legacyEndpoint(*next)
	return errs.Err()
}

// renameEndpointRefs rewrites host sourceRefs after an endpoint was renamed.
func (s *Service) renameEndpointRefs(ctx context.Context, old, ep model.DockerEndpoint) {
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return
	}
	for i := range hosts {
		h := &hosts[i]
		if h.Source != model.SourceDocker {
			continue
		}
		if name, ok := strings.CutPrefix(h.SourceRef, old.Name+"/"); ok {
			h.SourceRef = ep.Name + "/" + name
			_ = s.app.Store.Hosts().Update(ctx, h)
		}
	}
}

// ---------------------------------------------------------------- containers

var errDisabled = errors.New("Docker discovery is disabled")

// Containers returns all containers (running and stopped) of all connected
// endpoints, decorated with the host/backend that already uses them.
func (s *Service) Containers(ctx context.Context) ([]core.Container, error) {
	set := s.settings(ctx)
	if !set.Enabled {
		return nil, errDisabled
	}
	all := []core.Container{}
	connected := 0
	var firstErr error
	for _, c := range s.connList(set) {
		list, err := c.containers(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", c.getEP().Name, err)
			}
			continue
		}
		connected++
		all = append(all, list...)
	}
	if connected == 0 {
		if firstErr == nil {
			firstErr = errors.New("no Docker host is connected")
		}
		return nil, firstErr
	}
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	backends, err := s.app.Store.Backends().List(ctx)
	if err != nil {
		return nil, err
	}
	decorate(all, hosts, backends)
	return all, nil
}

// decorate sets HostID / BackendID for containers already in use.
func decorate(list []core.Container, hosts []model.ProxyHost, backends []model.Backend) {
	for i := range list {
		c := &list[i]
		ports := upstreamPorts(*c)
		ref := sourceRef(*c)
		for _, h := range hosts {
			if h.Source == model.SourceDocker && (h.SourceRef == ref || (h.SourceRef == c.Name && c.EndpointID == LocalEndpointID)) {
				c.HostID = h.ID
				break
			}
			if c.UpstreamHost != "" && h.Upstream.Host == c.UpstreamHost && (h.Upstream.Port == c.SuggestedPort || ports[h.Upstream.Port]) {
				c.HostID = h.ID
				break
			}
		}
		for _, b := range backends {
			for _, srv := range b.Servers {
				if (c.UpstreamHost != "" && srv.Address == c.UpstreamHost && (srv.Port == c.SuggestedPort || ports[srv.Port])) || (c.EndpointID == LocalEndpointID && srv.Address == c.Name) {
					c.BackendID = b.ID
					c.Reason = "belongs to backend " + b.Name
					break
				}
			}
			if c.BackendID != "" {
				break
			}
		}
	}
}

var errKicked = errors.New("reconfigured")

// resyncEndpoint refreshes one endpoint and reconciles label-driven config.
func (s *Service) resyncEndpoint(ctx context.Context, c *endpointConn, initial bool) {
	if err := c.refresh(ctx); err != nil {
		s.app.Log.Warn("docker refresh", "endpoint", c.getEP().Name, "err", err)
		return
	}
	c.mu.Lock()
	list := make([]core.Container, len(c.raw))
	copy(list, c.raw)
	ep := c.ep
	c.mu.Unlock()
	set := s.settings(ctx)
	if set.Enabled {
		s.reconcile(ctx, c, ep, list, set, initial)
	}
	s.processRemovals(ctx)
}

// watchBus reconnects after a backup restore replaced the settings.
func (s *Service) watchBus(ctx context.Context) {
	ch, cancel := s.app.Bus.Subscribe(16)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Topic == relayevents.BackupChanged {
				if d, ok := ev.Data.(map[string]any); ok && d["status"] == "restored" {
					s.Reconfigure()
				}
			}
		}
	}
}
