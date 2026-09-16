package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// Version is this build's version, sent to gateways in Hello (set by main).
var Version = "dev"

// DrainTimeout bounds how long Shutdown lets streams in flight finish.
const DrainTimeout = 5 * time.Second

// timing holds the engine's timers; tests shorten them.
type timing struct {
	backoffMin   time.Duration // first reconnect delay
	backoffMax   time.Duration // reconnect delay cap
	backoffReset time.Duration // a session that lived this long resets the backoff
	incompatible time.Duration // retry interval after a protocol mismatch
	quicAuto     time.Duration // QUIC handshake bound with transport auto
	dial         time.Duration // dial + handshake bound otherwise
	quicPenalty  time.Duration // auto: after QUIC failed, TCP goes first this long
	hello        time.Duration // control stream + peer Hello
	header       time.Duration // data stream header
	target       time.Duration // connecting to a proxy engine socket
	ping         time.Duration // control ping interval
	drain        time.Duration // streams in flight at shutdown
}

var defaultTiming = timing{
	backoffMin:   time.Second,
	backoffMax:   30 * time.Second,
	backoffReset: 60 * time.Second,
	incompatible: 5 * time.Minute,
	quicAuto:     5 * time.Second,
	dial:         10 * time.Second,
	quicPenalty:  10 * time.Minute,
	hello:        10 * time.Second,
	header:       10 * time.Second,
	target:       5 * time.Second,
	ping:         15 * time.Second,
	drain:        DrainTimeout,
}

func (t timing) withDefaults() timing {
	d := defaultTiming
	for _, f := range []struct{ v, def *time.Duration }{
		{&t.backoffMin, &d.backoffMin}, {&t.backoffMax, &d.backoffMax}, {&t.backoffReset, &d.backoffReset},
		{&t.incompatible, &d.incompatible}, {&t.quicAuto, &d.quicAuto}, {&t.dial, &d.dial},
		{&t.quicPenalty, &d.quicPenalty}, {&t.hello, &d.hello}, {&t.header, &d.header},
		{&t.target, &d.target}, {&t.ping, &d.ping}, {&t.drain, &d.drain},
	} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	return t
}

// Options configure an Engine.
type Options struct {
	// Stderr receives nginx-style log lines ("[notice] started hash=…").
	Stderr io.Writer
	// Version is sent to gateways in Hello ("" = Version).
	Version string
	// Mux tunes tunnel sessions (zero = mux defaults).
	Mux mux.Config

	// dial replaces transport dialing (tests).
	dial func(ctx context.Context, gw Gateway) (mux.Session, error)
	// timing overrides timers (tests); zero fields use the defaults.
	timing timing
}

// Engine is a running tunnel engine: one connection loop per dialable
// gateway, the streams they carry and the runtime socket.
type Engine struct {
	opts Options
	t    timing
	log  *logger

	mu       sync.Mutex // serializes Start, Reload and Shutdown; guards the fields below
	running  bool
	stopping bool
	hash     string
	started  time.Time
	gws      map[string]*gateway
	sock     *runtimeSocket

	drainCh  chan struct{} // closed when Shutdown begins
	draining atomic.Bool
	inflight atomic.Int64 // data streams being handled
	loops    sync.WaitGroup
}

// New returns an engine that is not started yet.
func New(opts Options) *Engine {
	if opts.Version == "" {
		opts.Version = Version
	}
	return &Engine{
		opts:    opts,
		t:       opts.timing.withDefaults(),
		log:     newLogger(opts.Stderr),
		gws:     map[string]*gateway{},
		drainCh: make(chan struct{}),
	}
}

// Start loads cfg's gateway list, binds the runtime socket and starts
// dialing.
func (e *Engine) Start(cfg *Config, hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || e.stopping {
		return errors.New("tunnel: engine already started")
	}
	if err := e.apply(cfg, hash); err != nil {
		return err
	}
	e.running = true
	e.started = time.Now()
	return nil
}

// Reload switches to cfg and re-reads the gateway list. Sessions of gateways
// whose connection settings did not change stay up (their new routes are
// pushed); on error nothing changes.
func (e *Engine) Reload(cfg *Config, hash string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || e.stopping {
		return errors.New("tunnel: engine is not running")
	}
	return e.apply(cfg, hash)
}

// apply does the work of Start and Reload; e.mu must be held.
func (e *Engine) apply(cfg *Config, hash string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	gws, err := loadGateways(cfg.GatewaysFile)
	if err != nil {
		return err
	}
	var newSock *runtimeSocket
	sockPath := ""
	if e.sock != nil {
		sockPath = e.sock.path
	}
	if cfg.RuntimeSocket != "" && cfg.RuntimeSocket != sockPath {
		if newSock, err = listenRuntime(cfg.RuntimeSocket, e); err != nil {
			return fmt.Errorf("runtime socket %s: %w", cfg.RuntimeSocket, err)
		}
	}

	// Point of no return.
	e.hash = hash
	if newSock != nil || cfg.RuntimeSocket == "" {
		if e.sock != nil {
			e.sock.close()
		}
		e.sock = newSock
		if newSock != nil {
			go newSock.serve()
		}
	}
	routes := map[string]Route{}
	for _, r := range cfg.Routes {
		routes[r.GatewayID] = r
	}
	seen := map[string]bool{}
	for _, def := range gws.Gateways {
		seen[def.ID] = true
		g := e.gws[def.ID]
		if g == nil {
			g = &gateway{e: e, id: def.ID}
			e.gws[def.ID] = g
		}
		r, ok := routes[def.ID]
		var table *routeTable
		if ok {
			table = newRouteTable(cfg, r)
		}
		g.configure(def, table)
	}
	for id, g := range e.gws {
		if !seen[id] {
			g.remove()
			delete(e.gws, id)
		}
	}
	return nil
}

// Hash returns the active release ("" before Start).
func (e *Engine) Hash() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hash
}

// Status reports every gateway in the gateway list, sorted by id.
func (e *Engine) Status() Status {
	e.mu.Lock()
	st := Status{Hash: e.hash, StartedAt: e.started, Gateways: []GatewayStatus{}}
	gws := make([]*gateway, 0, len(e.gws))
	for _, g := range e.gws {
		gws = append(gws, g)
	}
	e.mu.Unlock()
	sort.Slice(gws, func(i, j int) bool { return gws[i].id < gws[j].id })
	for _, g := range gws {
		st.Gateways = append(st.Gateways, g.status())
	}
	return st
}

// Shutdown stops dialing, lets streams in flight finish for up to
// DrainTimeout (or until ctx ends), closes every session and removes the
// runtime socket.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		return nil
	}
	e.stopping = true
	e.draining.Store(true)
	close(e.drainCh)
	gws := make([]*gateway, 0, len(e.gws))
	for _, g := range e.gws {
		gws = append(gws, g)
	}
	sock := e.sock
	e.sock = nil
	e.mu.Unlock()

	drain := time.NewTimer(e.t.drain)
	defer drain.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
wait:
	for e.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			break wait
		case <-drain.C:
			break wait
		case <-tick.C:
		}
	}
	for _, g := range gws {
		g.remove()
	}
	done := make(chan struct{})
	go func() {
		e.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if sock != nil {
		sock.close()
	}
	return ctx.Err()
}

// ---------------------------------------------------------------- routes

// routeTable is what one gateway publishes, compiled for stream lookups.
type routeTable struct {
	gen         uint64
	names       []string // normalized, sorted, unique (as pushed)
	ports       []uint16 // sorted, unique (as pushed)
	match       *wire.Names
	http, https string
	tcp         map[uint16]string
}

func newRouteTable(cfg *Config, r Route) *routeTable {
	t := &routeTable{names: []string{}, ports: []uint16{}, http: cfg.Targets.HTTP, https: cfg.Targets.HTTPS, tcp: map[uint16]string{}}
	for _, n := range r.Names {
		if n = wire.NormalizeName(n); n != "" {
			t.names = append(t.names, n)
		}
	}
	slices.Sort(t.names)
	t.names = slices.Compact(t.names)
	t.match = wire.NewNames(t.names)
	for _, tr := range r.TCP {
		if tr.Port == 0 || tr.Socket == "" {
			continue
		}
		if _, dup := t.tcp[tr.Port]; !dup {
			t.ports = append(t.ports, tr.Port)
		}
		t.tcp[tr.Port] = tr.Socket
	}
	slices.Sort(t.ports)
	return t
}

// sameRoutes reports whether both tables push the same Routes message.
func (t *routeTable) sameRoutes(o *routeTable) bool {
	return t != nil && o != nil && slices.Equal(t.names, o.names) && slices.Equal(t.ports, o.ports)
}

// resolve returns the proxy engine socket for a stream header, or "" when
// the name or port is not published through this gateway.
func (t *routeTable) resolve(h wire.StreamHeader) string {
	if t == nil {
		return ""
	}
	switch h.Kind {
	case wire.KindHTTPS:
		if t.match.Match(h.Name) {
			return t.https
		}
	case wire.KindHTTP:
		if t.match.Match(h.Name) {
			return t.http
		}
	case wire.KindTCP:
		return t.tcp[h.Port]
	}
	return ""
}

func (t *routeTable) message() *wire.Message {
	return &wire.Message{Routes: &wire.Routes{Generation: t.gen, Names: t.names, TCP: t.ports}}
}
