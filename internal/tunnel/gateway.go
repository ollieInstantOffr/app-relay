package tunnel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
)

// Gateway transports.
const (
	TransportAuto = "auto"
	TransportQUIC = "quic"
	TransportTCP  = "tcp"
)

// loadGateways reads and checks gateways.json.
func loadGateways(path string) (*Gateways, error) {
	g, err := LoadGateways(path)
	if err != nil {
		return nil, err
	}
	if err := validateGateways(g); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return g, nil
}

// validateGateways checks the structural rules of a gateway list. Problems
// that only affect dialing (unreadable keys, bad addresses) are reported per
// gateway in its status instead.
func validateGateways(g *Gateways) error {
	var errs []error
	seen := map[string]bool{}
	for i, gw := range g.Gateways {
		switch {
		case gw.ID == "":
			errs = append(errs, fmt.Errorf("gateway #%d without id", i+1))
			continue
		case seen[gw.ID]:
			errs = append(errs, fmt.Errorf("gateway %s: duplicate id", gw.ID))
		}
		seen[gw.ID] = true
		switch gw.Transport {
		case "", TransportAuto, TransportQUIC, TransportTCP:
		default:
			errs = append(errs, fmt.Errorf("gateway %s: unknown transport %q (expected auto, quic or tcp)", gw.ID, gw.Transport))
		}
	}
	return errors.Join(errs...)
}

// gateway is one entry of the gateway list: its settings, the connection
// loop while it is dialable, and counters that survive reconnects.
type gateway struct {
	e  *Engine
	id string

	mu        sync.Mutex
	def       Gateway
	identity  [sha256.Size]byte // digest of the key files, to notice a new identity
	gen       uint64            // last routes generation assigned
	loop      *loop             // nil while not dialing
	st        GatewayStatus     // state fields (counters are below)
	connected bool              // a session was established before (Reconnects)
	quicFail  time.Time         // auto transport: when QUIC last failed
	lastLog   string            // last connection error logged
	lastLogAt time.Time

	routes   atomic.Pointer[routeTable] // nil = nothing published through it
	active   atomic.Int64
	streams  atomic.Uint64
	rejected atomic.Uint64
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64

	rejectLog rateLimit
	targetLog rateLimit
}

// dialSettings are the fields whose change requires a new connection.
func dialSettings(g Gateway) Gateway {
	g.Name = ""
	if g.Transport == "" {
		g.Transport = TransportAuto
	}
	return g
}

func identityDigest(def Gateway) [sha256.Size]byte {
	h := sha256.New()
	for _, f := range []string{def.CertFile, def.KeyFile} {
		b, err := os.ReadFile(f)
		if err != nil {
			b = []byte(err.Error())
		}
		h.Write([]byte{0})
		h.Write(b)
	}
	return [sha256.Size]byte(h.Sum(nil))
}

// configure applies a gateway's settings and routes (nil = none).
func (g *gateway) configure(def Gateway, table *routeTable) {
	g.mu.Lock()
	defer g.mu.Unlock()
	digest := identityDigest(def)
	changed := g.def.ID != "" && (dialSettings(g.def) != dialSettings(def) || digest != g.identity)
	g.def, g.identity = def, digest
	g.st.ID, g.st.Address = def.ID, def.Address

	routesChanged := false
	if table != nil {
		if old := g.routes.Load(); old.sameRoutes(table) {
			table.gen = old.gen
		} else {
			g.gen++
			table.gen = g.gen
			routesChanged = true
		}
	}
	g.routes.Store(table)

	want := StateConnecting
	switch {
	case !def.Enabled:
		want = StateDisabled
	case def.Pin == "":
		want = StateUnpaired
	case table == nil:
		want = StateIdle
	}
	if want != StateConnecting {
		g.stopLocked()
		g.st.State = want
		return
	}
	if g.loop != nil && changed {
		g.e.log.noticef("gateway %s: connection settings changed, reconnecting", g.id)
		g.stopLocked()
	}
	if g.loop == nil {
		g.startLocked()
		return
	}
	if routesChanged && g.loop.conn != nil {
		g.loop.conn.kick()
	}
}

// remove stops the gateway's connection (it left the list, or shutdown).
func (g *gateway) remove() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopLocked()
}

func (g *gateway) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	l := &loop{g: g, ctx: ctx, cancel: cancel}
	g.loop = l
	g.st.State = StateConnecting
	g.e.loops.Add(1)
	go l.run()
}

func (g *gateway) stopLocked() {
	if g.loop == nil {
		return
	}
	g.loop.cancel()
	g.loop = nil
	g.st.Transport = ""
	g.st.ConnectedAt = nil
	g.st.RTTMs = 0
}

// update runs f on the status if l is still the gateway's loop.
func (g *gateway) update(l *loop, f func(st *GatewayStatus)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.loop != l {
		return false
	}
	f(&g.st)
	return true
}

func (g *gateway) status() GatewayStatus {
	g.mu.Lock()
	st := g.st
	st.PublicIPs = slices.Clone(st.PublicIPs)
	st.PortErrors = slices.Clone(st.PortErrors)
	if st.GatewayStats != nil {
		s := *st.GatewayStats
		st.GatewayStats = &s
	}
	g.mu.Unlock()
	st.ActiveStreams = g.active.Load()
	st.Streams = g.streams.Load()
	st.Rejected = g.rejected.Load()
	st.BytesIn = g.bytesIn.Load()
	st.BytesOut = g.bytesOut.Load()
	return st
}

// ---------------------------------------------------------------- connection loop

// loop dials a gateway and serves its sessions until canceled.
type loop struct {
	g      *gateway
	ctx    context.Context
	cancel context.CancelFunc
	conn   *conn // live connection; guarded by g.mu
}

// jitter spreads d by ±20%.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (l *loop) run() {
	g, e := l.g, l.g.e
	defer e.loops.Done()
	backoff := e.t.backoffMin
	for {
		if l.ctx.Err() != nil || e.draining.Load() {
			return
		}
		g.mu.Lock()
		def := g.def
		g.mu.Unlock()
		g.update(l, func(st *GatewayStatus) {
			if st.State != StateIncompatible {
				st.State = StateConnecting
			}
		})
		res := l.connect(def)
		if l.ctx.Err() != nil {
			return
		}
		if res.lived >= e.t.backoffReset {
			backoff = e.t.backoffMin
		}
		wait := jitter(backoff)
		backoff = min(backoff*2, e.t.backoffMax)
		now := time.Now()
		msg := "connection closed"
		if res.err != nil {
			msg = res.err.Error()
		}
		g.update(l, func(st *GatewayStatus) {
			st.State = StateDisconnected
			if res.incompatible {
				st.State = StateIncompatible
			}
			st.Transport, st.ConnectedAt, st.RTTMs = "", nil, 0
			st.LastError, st.LastErrorAt = msg, &now
		})
		switch {
		case res.incompatible:
			wait = e.t.incompatible
		case res.lived > 0:
			e.log.warnf("gateway %s: disconnected after %s: %s; reconnecting", g.id, res.lived.Round(time.Second), msg)
		default:
			g.logDialError(def.Address, msg)
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-l.ctx.Done():
			t.Stop()
			return
		case <-e.drainCh:
			t.Stop()
			return
		}
	}
}

// logDialError logs a failed connection attempt when the error changed or
// every 5 minutes while it persists.
func (g *gateway) logDialError(address, msg string) {
	g.mu.Lock()
	log := msg != g.lastLog || time.Since(g.lastLogAt) >= 5*time.Minute
	if log {
		g.lastLog, g.lastLogAt = msg, time.Now()
	}
	g.mu.Unlock()
	if log {
		g.e.log.warnf("gateway %s: cannot connect to %s: %s", g.id, address, msg)
	}
}

type connectResult struct {
	err          error
	incompatible bool
	lived        time.Duration // how long the session was established (0 = never)
}

// connect dials once and serves the session until it ends.
func (l *loop) connect(def Gateway) connectResult {
	e := l.g.e
	ctx, cancel := context.WithCancel(l.ctx)
	go func() {
		select {
		case <-e.drainCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	sess, err := l.g.dial(ctx, def)
	cancel()
	if err != nil {
		return connectResult{err: err}
	}
	if l.ctx.Err() != nil || e.draining.Load() {
		sess.Close()
		return connectResult{err: context.Canceled}
	}
	c := newConn(l, sess)
	return c.serve()
}

// dial connects to the gateway with its transport policy.
func (g *gateway) dial(ctx context.Context, def Gateway) (mux.Session, error) {
	e := g.e
	if e.opts.dial != nil {
		return e.opts.dial(ctx, def)
	}
	if def.Address == "" {
		return nil, errors.New("no gateway address")
	}
	cert, err := os.ReadFile(def.CertFile)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	key, err := os.ReadFile(def.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	id, err := pair.LoadIdentity(cert, key)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}

	order := []string{def.Transport}
	auto := def.Transport == "" || def.Transport == TransportAuto
	if auto {
		g.mu.Lock()
		penalty := !g.quicFail.IsZero() && time.Since(g.quicFail) < e.t.quicPenalty
		g.mu.Unlock()
		order = []string{TransportQUIC, TransportTCP}
		if penalty {
			order = []string{TransportTCP, TransportQUIC}
		}
	}
	var errs []string
	for _, tr := range order {
		timeout := e.t.dial
		if auto && tr == TransportQUIC {
			timeout = e.t.quicAuto
		}
		sess, err := dialTransport(ctx, tr, def.Address, id, def.Pin, timeout, e.opts.Mux)
		if auto && tr == TransportQUIC {
			g.mu.Lock()
			if err == nil {
				g.quicFail = time.Time{}
			} else if ctx.Err() == nil {
				g.quicFail = time.Now()
			}
			g.mu.Unlock()
		}
		if err == nil {
			return sess, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, tr+": "+err.Error())
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// dialTransport opens one tunnel session over QUIC or TLS/TCP.
func dialTransport(ctx context.Context, transport, address string, id pair.Identity, pin string, timeout time.Duration, cfg mux.Config) (mux.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch transport {
	case TransportQUIC:
		conn, err := quic.DialAddr(ctx, address, pair.ClientConfig(id, pin, mux.ALPNQUIC), mux.QUICConfig(cfg))
		if err != nil {
			return nil, err
		}
		return mux.HomeQUIC(conn, cfg), nil
	case TransportTCP:
		d := tls.Dialer{
			NetDialer: &net.Dialer{KeepAlive: 30 * time.Second},
			Config:    pair.ClientConfig(id, pin, mux.ALPNH2),
		}
		nc, err := d.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		sess, err := mux.HomeTCP(nc.(*tls.Conn), cfg)
		if err != nil {
			nc.Close()
			return nil, err
		}
		return sess, nil
	}
	return nil, fmt.Errorf("unknown transport %q", transport)
}
