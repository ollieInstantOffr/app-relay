// Package health probes proxy host upstreams and stream targets (slice
// observe) and keeps their status in memory and in health_checks.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	sweepInterval = 30 * time.Second
	firstSweep    = 2 * time.Second
	confirmDelay  = 2 * time.Second
	sweepWorkers  = 16
)

type Service struct {
	app   *core.App
	mu    sync.RWMutex
	state map[string]core.HealthStatus
	kick  chan string // target, or "*" for a full sweep
}

func New(app *core.App) *Service {
	return &Service{app: app, state: map[string]core.HealthStatus{}, kick: make(chan string, 64)}
}

func (s *Service) Start(ctx context.Context) error {
	recs, err := s.app.Store.ListHealthChecks(ctx)
	if err != nil {
		s.app.Log.Warn("health: load previous state", "err", err)
	}
	s.mu.Lock()
	for _, r := range recs {
		s.state[r.Target] = core.HealthStatus{
			Target: r.Target, Status: r.Status, LatencyMs: r.LatencyMs, HTTPStatus: r.HTTPStatus,
			Detail: r.Detail, CheckedAt: r.CheckedAt, ChangedAt: r.ChangedAt,
		}
	}
	s.mu.Unlock()
	go s.loop(ctx)
	go s.watch(ctx)
	return nil
}

func (s *Service) Get(target string) (core.HealthStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.state[target]
	return st, ok
}

func (s *Service) All() map[string]core.HealthStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]core.HealthStatus, len(s.state))
	for k, v := range s.state {
		out[k] = v
	}
	return out
}

// Probe checks an upstream right now. Certificates are not verified (the
// question is whether something answers). Backend-routed upstreams without an
// address are resolved through their localhost HAProxy frontend.
func (s *Service) Probe(ctx context.Context, u model.Upstream) core.HealthStatus {
	if (u.Host == "" || u.Port == 0) && u.BackendID != "" {
		if fronts, err := s.app.Store.Frontends().List(ctx); err == nil {
			u = resolveUpstream(u, fronts)
		}
	}
	return probeHTTP(ctx, u, false)
}

func (s *Service) enqueue(target string) {
	select {
	case s.kick <- target:
	default:
	}
}

func (s *Service) loop(ctx context.Context) {
	timer := time.NewTimer(firstSweep)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.sweep(ctx)
			timer.Reset(sweepInterval)
		case t := <-s.kick:
			if t == "*" {
				s.sweep(ctx)
				timer.Reset(sweepInterval)
			} else {
				s.checkTarget(ctx, t)
			}
		}
	}
}

// watch re-probes immediately when a host or stream is saved.
func (s *Service) watch(ctx context.Context) {
	ch, cancel := s.app.Bus.Subscribe(128)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Topic == events.ApplyFinished {
				// Saved-but-unapplied targets may have been probed before the reload
				// made them reachable (e.g. new expose frontends); re-check everything.
				s.enqueue("*")
				continue
			}
			if ev.Topic != events.ConfigChanged {
				continue
			}
			c, ok := ev.Data.(core.ConfigChange)
			if !ok {
				continue
			}
			switch c.Kind {
			case model.KindHost:
				s.enqueue(core.HostTarget(c.ID))
			case model.KindStream:
				s.enqueue(core.StreamTarget(c.ID))
			case model.KindBackend, model.KindFrontend:
				s.enqueue("*")
			}
		}
	}
}

func (s *Service) sweep(ctx context.Context) {
	st := s.app.Store
	hosts, err := st.Hosts().List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.app.Log.Warn("health: list hosts", "err", err)
		}
		return
	}
	streams, err := st.Streams().List(ctx)
	if err != nil {
		return
	}
	fronts, _ := st.Frontends().List(ctx)

	seen := map[string]bool{}
	sem := make(chan struct{}, sweepWorkers)
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			fn()
		}()
	}
	for i := range hosts {
		h := &hosts[i]
		seen[core.HostTarget(h.ID)] = true
		run(func() { s.checkHost(ctx, h, fronts) })
	}
	for i := range streams {
		sm := &streams[i]
		seen[core.StreamTarget(sm.ID)] = true
		run(func() { s.checkStream(ctx, sm, fronts) })
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	for _, target := range s.targets() {
		if (strings.HasPrefix(target, "host:") || strings.HasPrefix(target, "stream:")) && !seen[target] {
			s.remove(ctx, target)
		}
	}
}

func (s *Service) targets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.state))
	for k := range s.state {
		out = append(out, k)
	}
	return out
}

func (s *Service) checkTarget(ctx context.Context, target string) {
	kind, id, _ := strings.Cut(target, ":")
	st := s.app.Store
	fronts, _ := st.Frontends().List(ctx)
	switch kind {
	case "host":
		h, err := st.Hosts().Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			s.remove(ctx, target)
			return
		}
		if err == nil {
			s.checkHost(ctx, h, fronts)
		}
	case "stream":
		sm, err := st.Streams().Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			s.remove(ctx, target)
			return
		}
		if err == nil {
			s.checkStream(ctx, sm, fronts)
		}
	}
}

type targetMeta struct {
	kind string // host | stream
	name string // first domain / stream name
	addr string // upstream host:port
	url  string // UI link
}

func (s *Service) checkHost(ctx context.Context, h *model.ProxyHost, fronts []model.Frontend) {
	target := core.HostTarget(h.ID)
	name := h.ID
	if len(h.Domains) > 0 {
		name = h.Domains[0]
	}
	u := resolveUpstream(h.Upstream, fronts)
	m := targetMeta{kind: "host", name: name, addr: upstreamAddr(u.Host, u.Port), url: "/hosts?edit=" + h.ID}
	if !h.Enabled {
		s.record(ctx, core.HealthStatus{Target: target, Status: core.HealthDisabled, Detail: "host disabled"}, m)
		return
	}
	st := probeHTTP(ctx, u, h.UpstreamTLSVerify)
	if st.Status == core.HealthDown && s.wasUp(target) && sleep(ctx, confirmDelay) {
		st = probeHTTP(ctx, u, h.UpstreamTLSVerify)
	}
	st.Target = target
	s.record(ctx, st, m)
}

func (s *Service) checkStream(ctx context.Context, sm *model.Stream, fronts []model.Frontend) {
	target := core.StreamTarget(sm.ID)
	name := sm.Name
	if name == "" {
		name = sm.ID
	}
	host, port := sm.ForwardHost, firstPort(sm.ForwardPorts)
	if port == 0 {
		port = firstPort(sm.ListenPorts)
	}
	if sm.BackendID != "" {
		if fh, fp, ok := frontendFor(fronts, sm.BackendID, "tcp"); ok {
			host, port = fh, fp
		}
	}
	m := targetMeta{kind: "stream", name: name, addr: upstreamAddr(host, port), url: "/streams"}
	switch {
	case !sm.Enabled:
		s.record(ctx, core.HealthStatus{Target: target, Status: core.HealthDisabled, Detail: "stream disabled"}, m)
		return
	case strings.EqualFold(sm.Protocol, "udp"):
		s.record(ctx, core.HealthStatus{Target: target, Status: core.HealthUnknown, Detail: "UDP is not probed"}, m)
		return
	}
	st := probeTCP(ctx, host, port)
	if st.Status == core.HealthDown && s.wasUp(target) && sleep(ctx, confirmDelay) {
		st = probeTCP(ctx, host, port)
	}
	st.Target = target
	s.record(ctx, st, m)
}

func (s *Service) wasUp(target string) bool {
	prev, ok := s.Get(target)
	return ok && (prev.Status == core.HealthHealthy || prev.Status == core.HealthDegraded)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *Service) record(ctx context.Context, st core.HealthStatus, m targetMeta) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	st.CheckedAt = now
	s.mu.Lock()
	prev, had := s.state[st.Target]
	if had && prev.Status == st.Status && !prev.ChangedAt.IsZero() {
		st.ChangedAt = prev.ChangedAt
	} else {
		st.ChangedAt = now
	}
	s.state[st.Target] = st
	s.mu.Unlock()

	if err := s.app.Store.UpsertHealthCheck(ctx, store.HealthRecord{
		Target: st.Target, Status: st.Status, LatencyMs: st.LatencyMs, HTTPStatus: st.HTTPStatus,
		Detail: st.Detail, CheckedAt: st.CheckedAt, ChangedAt: st.ChangedAt,
	}); err != nil && ctx.Err() == nil {
		s.app.Log.Warn("health: save state", "target", st.Target, "err", err)
	}
	if had && prev.Status == st.Status {
		return
	}
	s.app.Bus.Publish(events.HealthChanged, st)
	s.transition(ctx, prev, had, st, m)
}

// transition records dashboard activity and notifications for status changes.
func (s *Service) transition(ctx context.Context, prev core.HealthStatus, had bool, st core.HealthStatus, m targetMeta) {
	switch {
	case st.Status == core.HealthDown:
		detail := m.name + " · " + st.Detail
		if m.kind == "host" {
			code := "502"
			if strings.HasPrefix(st.Detail, "timed out") {
				code = "504"
			}
			detail = m.name + " · " + code
		}
		s.app.Activity(ctx, "upstream.down", "error", "Upstream unreachable", m.addr, detail)
		if s.app.Notify != nil {
			s.app.Notify.Notify(ctx, core.Notification{
				Event: model.EventUpstreamDown, Level: "error",
				Title:   m.name + " is down",
				Message: fmt.Sprintf("Upstream %s is unreachable · %s", m.addr, st.Detail),
				URL:     m.url,
			})
		}
	case had && prev.Status == core.HealthDown && (st.Status == core.HealthHealthy || st.Status == core.HealthDegraded):
		downFor := humanDuration(st.CheckedAt.Sub(prev.ChangedAt))
		s.app.Activity(ctx, "upstream.up", "ok", "Upstream recovered", m.addr, fmt.Sprintf("%s · %s · down %s", m.name, st.Detail, downFor))
		if s.app.Notify != nil {
			s.app.Notify.Notify(ctx, core.Notification{
				Event: model.EventUpstreamDown, Level: "info",
				Title:   m.name + " recovered",
				Message: fmt.Sprintf("Upstream %s is reachable again (%s) after %s", m.addr, st.Detail, downFor),
				URL:     m.url,
			})
		}
	case had && prev.Status == core.HealthHealthy && st.Status == core.HealthDegraded:
		s.app.Activity(ctx, "upstream.degraded", "warn", "Upstream returning errors", m.addr, m.name+" · "+st.Detail)
	}
}

func (s *Service) remove(ctx context.Context, target string) {
	s.mu.Lock()
	_, had := s.state[target]
	delete(s.state, target)
	s.mu.Unlock()
	_ = s.app.Store.DeleteHealthCheck(ctx, target)
	if had {
		s.app.Bus.Publish(events.HealthChanged, core.HealthStatus{Target: target, Status: core.HealthUnknown, CheckedAt: time.Now().UTC()})
	}
}

// resolveUpstream fills in 127.0.0.1:<port> for backend-routed upstreams that
// carry no address, using the enabled localhost HTTP frontend of the backend.
func resolveUpstream(u model.Upstream, fronts []model.Frontend) model.Upstream {
	if u.Host != "" && u.Port > 0 {
		return u
	}
	if u.BackendID != "" {
		if host, port, ok := frontendFor(fronts, u.BackendID, "http"); ok {
			u.Host, u.Port = host, port
			if u.Scheme == "" {
				u.Scheme = "http"
			}
		}
	}
	return u
}

func frontendFor(fronts []model.Frontend, backendID, mode string) (string, int, bool) {
	for _, f := range fronts {
		if !f.Enabled || f.DefaultBackendID != backendID || (f.Mode != "" && !strings.EqualFold(f.Mode, mode)) {
			continue
		}
		host, portStr, err := net.SplitHostPort(f.Bind)
		if err != nil || (host != "127.0.0.1" && host != "localhost") {
			continue
		}
		if port, err := strconv.Atoi(portStr); err == nil {
			return "127.0.0.1", port, true
		}
	}
	return "", 0, false
}

// firstPort returns the first port of "25565", "2456-2458" or "80,443".
func firstPort(spec string) int {
	spec = strings.TrimSpace(spec)
	if i := strings.IndexAny(spec, "-,"); i >= 0 {
		spec = spec[:i]
	}
	p, err := strconv.Atoi(strings.TrimSpace(spec))
	if err != nil || p < 1 || p > 65535 {
		return 0
	}
	return p
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}
