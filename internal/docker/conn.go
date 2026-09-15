package docker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

// EndpointStatus is one entry of GET /api/docker/status → endpoints.
type EndpointStatus struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Type            string     `json:"type"`
	URL             string     `json:"url"`
	UpstreamAddress string     `json:"upstreamAddress"`
	Enabled         bool       `json:"enabled"`
	Connected       bool       `json:"connected"`
	Version         string     `json:"version"`
	APIVersion      string     `json:"apiVersion,omitempty"`
	Containers      int        `json:"containers"`
	Running         int        `json:"running"`
	Error           string     `json:"error,omitempty"`
	SocketMissing   bool       `json:"socketMissing,omitempty"`
	SSHFingerprint  string     `json:"sshFingerprint,omitempty"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
}

var errNotConnected = errors.New("not connected")

// endpointConn owns the connection, cache and event watcher of one endpoint.
type endpointConn struct {
	svc    *Service
	cancel context.CancelFunc
	kick   chan struct{}

	mu        sync.Mutex
	ep        model.DockerEndpoint
	key       string
	d         *dialed
	status    EndpointStatus
	raw       []core.Container
	cachedAt  time.Time
	inspected map[string]inspectInfo

	refreshMu sync.Mutex
}

// inspectInfo is the id-stable part of a container's configuration.
type inspectInfo struct {
	pc    portConfig // configured ports and bindings (used for stopped containers)
	hints Hints
	ok    bool
}

func newConn(s *Service, ep model.DockerEndpoint) *endpointConn {
	return &endpointConn{svc: s, ep: ep, key: endpointKey(ep), kick: make(chan struct{}, 1), inspected: map[string]inspectInfo{}}
}

func (c *endpointConn) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	go c.run(ctx)
}

func (c *endpointConn) stop() {
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *endpointConn) getEP() model.DockerEndpoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ep
}

func (c *endpointConn) getKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key
}

// updateMeta applies settings that don't need a reconnect.
func (c *endpointConn) updateMeta(ep model.DockerEndpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ep.Name != ep.Name || c.ep.UpstreamAddress != ep.UpstreamAddress {
		c.cachedAt = time.Time{}
	}
	c.ep = ep
	c.key = endpointKey(ep)
}

func (c *endpointConn) getStatus() EndpointStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *endpointConn) kickNow() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *endpointConn) setStatus(fn func(st *EndpointStatus)) {
	now := time.Now()
	c.mu.Lock()
	fn(&c.status)
	c.status.CheckedAt = &now
	c.mu.Unlock()
}

func (c *endpointConn) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		ep := c.getEP()
		d, err := c.svc.dial(ctx, ep)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var missing *socketMissingError
			var mismatch *HostKeyMismatchError
			c.setStatus(func(st *EndpointStatus) {
				*st = EndpointStatus{Error: err.Error(), SocketMissing: errors.As(err, &missing)}
				if errors.As(err, &mismatch) {
					st.SSHFingerprint = mismatch.Got
				}
			})
			c.svc.publish("error", "")
			select {
			case <-ctx.Done():
				return
			case <-c.kick:
				backoff = time.Second
			case <-time.After(backoff):
				backoff = min(backoff*2, time.Minute)
			}
			continue
		}
		backoff = time.Second
		if ep.Type == model.DockerSSH && ep.SSHKnownHost == "" && d.fingerprint != "" {
			c.svc.trustHostKey(ctx, c, d.fingerprint)
		}
		c.mu.Lock()
		c.d = d
		c.cachedAt = time.Time{}
		c.mu.Unlock()
		c.setStatus(func(st *EndpointStatus) {
			*st = EndpointStatus{Connected: true, Version: d.version.Version, APIVersion: d.version.APIVersion, SSHFingerprint: d.fingerprint}
		})
		c.svc.app.Log.Info("docker connected", "endpoint", ep.Name, "url", ep.URL, "version", d.version.Version)
		err = c.session(ctx, d)
		c.mu.Lock()
		c.d = nil
		c.raw = nil
		c.mu.Unlock()
		d.Close()
		if errors.Is(err, errKicked) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		c.svc.app.Log.Warn("docker connection lost", "endpoint", ep.Name, "err", err)
		c.setStatus(func(st *EndpointStatus) { *st = EndpointStatus{Error: "connection lost: " + err.Error()} })
		c.svc.publish("error", "")
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		case <-time.After(backoff):
		}
	}
}

// session watches Docker events until the connection fails, the endpoint is
// kicked, or ctx ends.
func (c *endpointConn) session(ctx context.Context, d *dialed) error {
	evCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	msgs, errs := d.api.Events(evCtx, events.ListOptions{Filters: filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("event", "create"), filters.Arg("event", "start"), filters.Arg("event", "die"),
		filters.Arg("event", "destroy"), filters.Arg("event", "rename"),
	)})
	c.svc.resyncEndpoint(ctx, c, true)
	c.svc.publish("connected", "")

	resync := time.NewTicker(60 * time.Second)
	defer resync.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.kick:
			return errKicked
		case err, ok := <-errs:
			if !ok || err == nil {
				return errors.New("event stream closed")
			}
			return err
		case m, ok := <-msgs:
			if !ok {
				return errors.New("event stream closed")
			}
			ep := c.getEP()
			name := m.Actor.Attributes["name"]
			switch string(m.Action) {
			case "destroy":
				c.svc.scheduleRemoval(ep.ID, name, time.Now())
				time.AfterFunc(c.svc.removeDelay+time.Second, func() { c.svc.processRemovals(ctx) })
			case "start", "create":
				c.svc.cancelRemoval(ep.ID, name)
			case "rename":
				c.svc.handleRename(ctx, ep, strings.TrimPrefix(m.Actor.Attributes["oldName"], "/"), name)
			}
			c.mu.Lock()
			c.cachedAt = time.Time{}
			c.mu.Unlock()
			c.svc.publish(string(m.Action), name)
			debounce.Reset(500 * time.Millisecond)
		case <-debounce.C:
			c.svc.resyncEndpoint(ctx, c, false)
		case <-resync.C:
			c.svc.resyncEndpoint(ctx, c, false)
		}
	}
}

// refresh lists all containers (running and stopped) of the endpoint.
func (c *endpointConn) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	d, ep := c.d, c.ep
	fresh := time.Since(c.cachedAt) < cacheTTL
	c.mu.Unlock()
	if d == nil {
		return errNotConnected
	}
	if fresh {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	summaries, err := d.api.ContainerList(lctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	list := make([]core.Container, 0, len(summaries))
	running := 0
	seen := map[string]bool{}
	for _, sm := range summaries {
		seen[sm.ID] = true
		info := c.inspect(lctx, d.api, sm)
		pc := info.pc
		if string(sm.State) == "running" || !info.ok {
			pc = summaryPorts(sm)
		}
		if string(sm.State) == "running" {
			running++
		}
		list = append(list, buildContainer(ep, sm, pc, info.hints))
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	c.mu.Lock()
	for id := range c.inspected {
		if !seen[id] {
			delete(c.inspected, id)
		}
	}
	c.raw = list
	c.cachedAt = time.Now()
	c.status.Containers = len(list)
	c.status.Running = running
	c.mu.Unlock()
	return nil
}

// inspect reads a container's configuration once (it can't change without
// recreating the container, which yields a new id): env and command for port
// detection, configured bindings for stopped containers.
func (c *endpointConn) inspect(ctx context.Context, api dockerAPI, sm container.Summary) inspectInfo {
	c.mu.Lock()
	info, ok := c.inspected[sm.ID]
	c.mu.Unlock()
	if ok {
		return info
	}
	resp, err := api.ContainerInspect(ctx, sm.ID)
	if err != nil {
		return inspectInfo{hints: Hints{Command: sm.Command}}
	}
	info = inspectInfo{pc: inspectPorts(resp), hints: inspectHints(resp, sm.Command), ok: true}
	c.mu.Lock()
	c.inspected[sm.ID] = info
	c.mu.Unlock()
	return info
}

func (c *endpointConn) containers(ctx context.Context) ([]core.Container, error) {
	if err := c.refresh(ctx); err != nil {
		if errors.Is(err, errNotConnected) {
			if st := c.getStatus(); st.Error != "" {
				return nil, errors.New(st.Error)
			}
		}
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.raw), nil
}

func (c *endpointConn) containerExists(ctx context.Context, name string) (bool, error) {
	c.mu.Lock()
	d := c.d
	c.mu.Unlock()
	if d == nil {
		return false, errNotConnected
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := d.api.ContainerList(lctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("name", "^/"+name+"$"))})
	if err != nil {
		return false, err
	}
	for _, sm := range list {
		if slices.Contains(sm.Names, "/"+name) {
			return true, nil
		}
	}
	return false, nil
}
