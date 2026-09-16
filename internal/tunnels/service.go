// Package tunnels manages tunnel gateways from the Relay app: this Relay's
// identity per gateway, pairing, the runtime gateway list the tunnel engine
// reads (<data>/tunnel/gateways.json), and connection status (activity and
// notifications when a gateway goes down or comes back).
//
// Publishing hosts and streams through a gateway is ordinary config
// (tunnelGatewayId, applied with the rest); gateways themselves are runtime
// state like certificates and take effect immediately.
package tunnels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/tunnel"
	"github.com/instantoffr/relay/internal/tunnel/pair"
)

const (
	pollInterval = 10 * time.Second
	// downAfter debounces "gateway down" alerts so reconnects don't page anyone.
	downAfter = 60 * time.Second
)

type Service struct {
	app *core.App
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	status  *tunnel.Status
	statusT time.Time
	conn    map[string]*connState // gateway id → last seen connection state
	writeMu sync.Mutex            // serialises gateways.json writes
	hooks   sync.Once
}

type connState struct {
	connected  bool
	since      time.Time // of the current state
	alerted    bool      // a down alert was sent for the current outage
	wasUp      bool      // connected at least once since Relay started
}

func New(app *core.App) *Service {
	return &Service{app: app, log: app.Log.With("svc", "tunnels"), now: time.Now, conn: map[string]*connState{}}
}

// Start writes the gateway list for the tunnel engine and watches connections.
func (s *Service) Start(ctx context.Context) error {
	if err := s.writeGateways(ctx); err != nil {
		s.log.Warn("write tunnel gateways", "err", err)
	}
	go s.pollLoop(ctx)
	return nil
}

// dir is <data>/tunnel.
func (s *Service) dir() string { return filepath.Join(s.app.Config.DataDir, "tunnel") }

// gatewayDir holds this Relay's identity for one gateway.
func (s *Service) gatewayDir(id string) string { return filepath.Join(s.dir(), safeID(id)) }

func (s *Service) identityPaths(id string) (cert, key string) {
	d := s.gatewayDir(id)
	return filepath.Join(d, "home.crt"), filepath.Join(d, "home.key")
}

func safeID(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

// identity loads or creates this Relay's key for a gateway.
func (s *Service) identity(id string) (pair.Identity, error) {
	certPath, keyPath := s.identityPaths(id)
	certPEM, err1 := os.ReadFile(certPath)
	keyPEM, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		return pair.LoadIdentity(certPEM, keyPEM)
	}
	ident, err := pair.NewIdentity("relay-home")
	if err != nil {
		return pair.Identity{}, err
	}
	certPEM, keyPEM, err = ident.MarshalPEM()
	if err != nil {
		return pair.Identity{}, err
	}
	if err := os.MkdirAll(s.gatewayDir(id), 0o750); err != nil {
		return pair.Identity{}, err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return pair.Identity{}, err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return pair.Identity{}, err
	}
	return ident, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// writeGateways renders the runtime gateway list for the tunnel engine.
func (s *Service) writeGateways(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	list, err := s.app.Store.Gateways().List(ctx)
	if err != nil {
		return err
	}
	out := tunnel.Gateways{Schema: 1, Gateways: []tunnel.Gateway{}}
	for _, g := range list {
		certPath, keyPath := s.identityPaths(g.ID)
		pin := ""
		if g.PairState == model.GatewayPaired {
			pin = g.GatewayPin
		}
		out.Gateways = append(out.Gateways, tunnel.Gateway{
			ID: g.ID, Name: g.Name, Address: g.DialAddress(), Transport: g.Transport, Enabled: g.Enabled,
			Pin: pin, CertFile: certPath, KeyFile: keyPath,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(), 0o750); err != nil {
		return err
	}
	path := filepath.Join(s.dir(), tunnel.GatewaysFileName)
	if prev, err := os.ReadFile(path); err == nil && string(prev) == string(data)+"\n" {
		return nil
	}
	return writeFileAtomic(path, append(data, '\n'), 0o640)
}

// gatewaysChanged rewrites the gateway list and makes a running tunnel engine
// re-read it.
func (s *Service) gatewaysChanged(ctx context.Context) {
	if err := s.writeGateways(ctx); err != nil {
		s.log.Warn("write tunnel gateways", "err", err)
		return
	}
	if c := s.app.Tunnel; c != nil {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := c.Status(sctx)
		cancel()
		if err == nil && st.Running {
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if resp, err := c.Reload(rctx); err != nil {
				s.log.Warn("reload the tunnel engine", "err", err)
			} else if !resp.OK {
				s.log.Warn("reload the tunnel engine", "output", resp.Output)
			}
			cancel()
		}
	}
	s.app.Bus.Publish(events.TunnelChanged, map[string]any{"reason": "gateways"})
}

// ---------------------------------------------------------------- status

// EngineStatus asks the tunnel engine for its runtime status. It returns nil
// (and no error) when the engine isn't running.
func (s *Service) EngineStatus(ctx context.Context) (*tunnel.Status, error) {
	s.mu.Lock()
	if s.status != nil && s.now().Sub(s.statusT) < 2*time.Second {
		st := s.status
		s.mu.Unlock()
		return st, nil
	}
	s.mu.Unlock()
	c := s.app.Tunnel
	if c == nil {
		return nil, nil
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ast, err := c.Status(sctx)
	if err != nil || !ast.Running {
		return nil, nil
	}
	out, err := c.Runtime(sctx, "status")
	if err != nil {
		return nil, err
	}
	var st tunnel.Status
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &st); err != nil {
		return nil, fmt.Errorf("tunnel engine status: %w", err)
	}
	s.mu.Lock()
	s.status, s.statusT = &st, s.now()
	s.mu.Unlock()
	return &st, nil
}

func (s *Service) pollLoop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.poll(ctx)
		}
	}
}

// poll records gateway reports and raises activity / notifications on
// connection changes.
func (s *Service) poll(ctx context.Context) {
	gateways, err := s.app.Store.Gateways().List(ctx)
	if err != nil || len(gateways) == 0 {
		return
	}
	st, err := s.EngineStatus(ctx)
	if err != nil {
		s.log.Debug("tunnel status", "err", err)
		return
	}
	byID := map[string]tunnel.GatewayStatus{}
	if st != nil {
		for _, g := range st.Gateways {
			byID[g.ID] = g
		}
	}
	changed := false
	for i := range gateways {
		g := &gateways[i]
		gs, reported := byID[g.ID]
		if reported && gs.State == tunnel.StateConnected && (gs.Version != g.Version || !slices.Equal(gs.PublicIPs, g.PublicIPs)) {
			g.Version, g.PublicIPs = gs.Version, gs.PublicIPs
			if err := s.app.Store.Gateways().Update(ctx, g); err != nil {
				s.log.Warn("record gateway report", "gateway", g.ID, "err", err)
			}
			changed = true
		}
		// Only gateways the engine is supposed to keep connected count.
		expected := reported && gs.State != tunnel.StateIdle && gs.State != tunnel.StateDisabled && gs.State != tunnel.StateUnpaired
		s.track(ctx, g, expected, reported && gs.State == tunnel.StateConnected, gs.LastError)
	}
	if changed {
		s.app.Bus.Publish(events.TunnelChanged, map[string]any{"reason": "report"})
	}
}

func (s *Service) track(ctx context.Context, g *model.Gateway, expected, connected bool, lastErr string) {
	now := s.now()
	s.mu.Lock()
	cs := s.conn[g.ID]
	if cs == nil {
		cs = &connState{connected: connected, since: now, wasUp: connected}
		s.conn[g.ID] = cs
		s.mu.Unlock()
		return
	}
	var up, down bool
	switch {
	case !expected:
		cs.connected, cs.since, cs.alerted = connected, now, false
	case connected && !cs.connected:
		up = cs.alerted
		cs.connected, cs.since, cs.alerted, cs.wasUp = true, now, false, true
	case !connected && cs.connected:
		cs.connected, cs.since = false, now
	case !connected && !cs.alerted && cs.wasUp && now.Sub(cs.since) >= downAfter:
		cs.alerted, down = true, true
	}
	s.mu.Unlock()
	switch {
	case up:
		s.app.Activity(ctx, "tunnel.up", "ok", fmt.Sprintf("Tunnel to %s is connected again", g.Name), g.Name, "")
		s.app.Bus.Publish(events.TunnelChanged, map[string]any{"gateway": g.ID, "connected": true})
	case down:
		detail := lastErr
		if detail == "" {
			detail = "the gateway is unreachable"
		}
		s.app.Activity(ctx, "tunnel.down", "error", fmt.Sprintf("Tunnel to %s is down", g.Name), g.Name, detail)
		if s.app.Notify != nil {
			s.app.Notify.Notify(ctx, core.Notification{Event: model.EventTunnelDown, Level: "error",
				Title: fmt.Sprintf("Tunnel to %s is down", g.Name), Message: detail, URL: "/tunnels"})
		}
		s.app.Bus.Publish(events.TunnelChanged, map[string]any{"gateway": g.ID, "connected": false})
	}
}

// ---------------------------------------------------------------- deletion

// References lists what publishes through a gateway (current config and live).
type References struct {
	Hosts   []string `json:"hosts"`   // first domain
	Streams []string `json:"streams"` // names
}

func (r References) empty() bool { return len(r.Hosts) == 0 && len(r.Streams) == 0 }

// references returns what the current configuration publishes through id.
func (s *Service) references(ctx context.Context, id string) (References, error) {
	out := References{Hosts: []string{}, Streams: []string{}}
	snap, err := s.app.Store.Snapshot(ctx)
	if err != nil {
		return out, err
	}
	for _, h := range snap.Hosts {
		if h.TunnelGatewayID == id {
			name := h.ID
			if len(h.Domains) > 0 {
				name = h.Domains[0]
			}
			out.Hosts = append(out.Hosts, name)
		}
	}
	for _, st := range snap.Streams {
		if st.TunnelGatewayID == id {
			out.Streams = append(out.Streams, st.Name)
		}
	}
	return out, nil
}

var errNoGateway = errors.New("gateway not found")
