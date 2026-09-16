// Package gateway is the Relay tunnel gateway (`relay gateway run`), run on a
// public server for a home Relay without inbound connectivity (CGNAT, no port
// forwarding). The home dials the gateway's tunnel port and keeps one
// multiplexed session open (internal/tunnel/mux); the gateway forwards every
// client connection on :80, :443 and published TCP ports as a stream over
// that session (internal/tunnel/wire).
//
// The gateway never decrypts TLS: :443 is routed by the ClientHello SNI and
// :80 by the Host header. It is deliberately dumb and untrusted; the home
// re-checks every stream against its own published routes.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/instantoffr/relay/internal/tunnel/mux"
	"github.com/instantoffr/relay/internal/tunnel/pair"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// Version is the build version sent in Hello; set by main.
var Version = "dev"

// Defaults for Options and the run flags.
const (
	DefaultTunnelAddr    = ":7443"
	DefaultHTTPAddr      = ":80"
	DefaultHTTPSAddr     = ":443"
	DefaultAllowPorts    = "1024-65535"
	DefaultMaxConns      = 10000
	DefaultMaxConnsPerIP = 512
)

const (
	handshakeTimeout = 10 * time.Second // tunnel TLS handshake; also the pairing exchange
	helloTimeout     = 10 * time.Second
	sniffTimeout     = 10 * time.Second
	openTimeout      = 10 * time.Second
	statsInterval    = 10 * time.Second

	httpsPeek = 32 << 10
	httpPeek  = 16 << 10

	maxHandshakes      = 256 // pending tunnel TLS handshakes and pairings
	maxHandshakesPerIP = 16

	pairFailuresPerIP  = 10
	pairFailuresGlobal = 100
	pairFailureWindow  = time.Minute
)

// Options configure a Server. An empty address disables that listener.
type Options struct {
	DataDir    string // identity and pairing state live in DataDir/gateway
	TunnelAddr string // TCP and UDP (QUIC) tunnel port, e.g. ":7443"
	HTTPAddr   string // e.g. ":80"
	HTTPSAddr  string // e.g. ":443"
	// BindHost is the host published TCP ports are bound on ("" = all
	// interfaces, dual-stack).
	BindHost string
	// AllowPorts are the TCP ports the home may publish ("1024-65535" when
	// empty). The tunnel, HTTP and HTTPS ports, 22, 80 and 443 are always excluded.
	AllowPorts string
	// PairToken pairs an unpaired gateway once; ignored when already paired.
	PairToken string
	// PublicIPs are announced in Hello instead of the interface addresses.
	PublicIPs     []string
	MaxConns      int // concurrent client connections (default 10000)
	MaxConnsPerIP int // concurrent client connections per client IP (default 512)
	Version       string
	Log           *slog.Logger
	Mux           mux.Config
}

// Addrs are the bound listener addresses (nil when disabled).
type Addrs struct {
	Tunnel     net.Addr // TCP
	TunnelQUIC net.Addr // UDP
	HTTP       net.Addr
	HTTPS      net.Addr
}

// Server is a tunnel gateway.
type Server struct {
	opts       Options
	log        *slog.Logger
	id         pair.Identity
	dir        string
	token      *pair.Token
	allow      []portRange
	reserved   map[uint16]bool
	publicIPs  []string
	limiter    *pair.Limiter
	statsEvery time.Duration

	pairing atomic.Pointer[Pairing] // nil = not paired
	routes  atomic.Pointer[routeTable]
	active  atomic.Pointer[tunnelSession] // session past its hello; nil refuses clients
	stats   counters

	mu        sync.Mutex // lifecycle, session, pairing and port listeners
	started   bool
	closed    bool
	tokenUsed bool
	session   *tunnelSession // newest session, possibly still in hello
	ports     map[uint16]net.Listener
	closers   []io.Closer
	addrs     Addrs

	connMu     sync.Mutex // admission of accepted connections
	closing    bool
	conns      map[net.Conn]struct{}
	clients    int
	clientsIP  map[netip.Addr]int
	handshakes int
	handIP     map[netip.Addr]int

	wg sync.WaitGroup
}

// routeTable is what the home published, replaced as a whole by Routes.
type routeTable struct {
	generation uint64
	names      *wire.Names
	tcp        map[uint16]bool
}

type counters struct {
	active       atomic.Int64
	accepted     atomic.Uint64
	rejectedName atomic.Uint64
	rejectedPort atomic.Uint64
	bytesIn      atomic.Uint64
	bytesOut     atomic.Uint64
}

// New loads (or creates) the identity and pairing state in opts.DataDir and
// validates opts. It binds nothing.
func New(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, errors.New("gateway: data dir is required")
	}
	if opts.Version == "" {
		opts.Version = Version
	}
	if opts.AllowPorts == "" {
		opts.AllowPorts = DefaultAllowPorts
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = DefaultMaxConns
	}
	if opts.MaxConnsPerIP <= 0 {
		opts.MaxConnsPerIP = DefaultMaxConnsPerIP
	}
	s := &Server{
		opts:       opts,
		log:        opts.Log,
		dir:        stateDir(opts.DataDir),
		limiter:    pair.NewLimiter(pairFailuresPerIP, pairFailuresGlobal, pairFailureWindow),
		statsEvery: statsInterval,
		ports:      map[uint16]net.Listener{},
		conns:      map[net.Conn]struct{}{},
		clientsIP:  map[netip.Addr]int{},
		handIP:     map[netip.Addr]int{},
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s.routes.Store(&routeTable{names: wire.NewNames(nil)})

	var err error
	if s.allow, err = parsePortRanges(opts.AllowPorts); err != nil {
		return nil, fmt.Errorf("gateway: allowed ports: %w", err)
	}
	for _, a := range opts.PublicIPs {
		for _, f := range strings.Split(a, ",") {
			if f = strings.TrimSpace(f); f == "" {
				continue
			}
			ip, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("gateway: invalid public IP %q", f)
			}
			s.publicIPs = append(s.publicIPs, ip.Unmap().String())
		}
	}
	if tok := strings.TrimSpace(opts.PairToken); tok != "" {
		t, err := pair.ParseToken(tok)
		if err != nil {
			return nil, fmt.Errorf("gateway: pairing token: %w", err)
		}
		s.token = &t
	}

	id, created, err := loadOrCreateIdentity(s.dir)
	if err != nil {
		return nil, fmt.Errorf("gateway: identity: %w", err)
	}
	s.id = id
	if created {
		s.log.Info("created gateway identity", "dir", s.dir, "fingerprint", id.Fingerprint())
	}
	p, err := loadPairing(s.dir)
	if err != nil {
		return nil, err
	}
	if p != nil {
		s.pairing.Store(p)
		s.tokenUsed = true // a token is only good for pairing an unpaired gateway
	}
	return s, nil
}

// Fingerprint is the gateway's key fingerprint, which the home pins.
func (s *Server) Fingerprint() string { return s.id.Fingerprint() }

// HomePin returns the paired home's fingerprint, or "" when not paired.
func (s *Server) HomePin() string {
	if p := s.pairing.Load(); p != nil {
		return p.HomePin
	}
	return ""
}

// Addrs returns the bound listener addresses (valid after Start).
func (s *Server) Addrs() Addrs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addrs
}

// Stats returns the traffic counters since start.
func (s *Server) Stats() wire.Stats {
	return wire.Stats{
		ActiveConns:  s.stats.active.Load(),
		Accepted:     s.stats.accepted.Load(),
		RejectedName: s.stats.rejectedName.Load(),
		RejectedPort: s.stats.rejectedPort.Load(),
		BytesIn:      s.stats.bytesIn.Load(),
		BytesOut:     s.stats.bytesOut.Load(),
	}
}

// Start binds the tunnel and client listeners and starts serving. Cancelling
// ctx closes the server like Close.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return errors.New("gateway: server closed")
	case s.started:
		return errors.New("gateway: server already started")
	}
	s.started = true
	fail := func(err error) error {
		for _, c := range s.closers {
			c.Close()
		}
		s.closers = nil
		s.closed = true
		return err
	}

	lc := listenConfig()
	var tunnelLn net.Listener
	var quicLn *quic.Listener
	if s.opts.TunnelAddr != "" {
		var err error
		if tunnelLn, quicLn, err = s.listenTunnel(ctx, lc); err != nil {
			return fail(fmt.Errorf("gateway: tunnel listener %s: %w", s.opts.TunnelAddr, err))
		}
		s.closers = append(s.closers, tunnelLn, quicLn)
		s.addrs.Tunnel, s.addrs.TunnelQUIC = tunnelLn.Addr(), quicLn.Addr()
	}
	var httpLn, httpsLn net.Listener
	for _, l := range []struct {
		addr string
		ln   *net.Listener
		dst  *net.Addr
	}{{s.opts.HTTPAddr, &httpLn, &s.addrs.HTTP}, {s.opts.HTTPSAddr, &httpsLn, &s.addrs.HTTPS}} {
		if l.addr == "" {
			continue
		}
		ln, err := lc.Listen(ctx, "tcp", l.addr)
		if err != nil {
			return fail(fmt.Errorf("gateway: listen %s: %w", l.addr, err))
		}
		*l.ln, *l.dst = ln, ln.Addr()
		s.closers = append(s.closers, ln)
	}

	s.reserved = map[uint16]bool{22: true, 80: true, 443: true}
	for _, a := range []net.Addr{s.addrs.Tunnel, s.addrs.HTTP, s.addrs.HTTPS} {
		if a != nil {
			s.reserved[addrPort(a).Port()] = true
		}
	}
	for _, a := range []string{s.opts.TunnelAddr, s.opts.HTTPAddr, s.opts.HTTPSAddr} {
		if p := addrPortNum(a); p != 0 {
			s.reserved[p] = true
		}
	}

	if tunnelLn != nil {
		s.wg.Add(2)
		go s.serve(tunnelLn, s.acceptTunnel)
		go s.serveQUIC(quicLn)
	}
	if httpLn != nil {
		port := addrPort(httpLn.Addr()).Port()
		s.wg.Add(1)
		go s.serve(httpLn, func(c net.Conn) { s.acceptClient(c, func(c net.Conn) { s.serveHTTP(c, port) }) })
	}
	if httpsLn != nil {
		port := addrPort(httpsLn.Addr()).Port()
		s.wg.Add(1)
		go s.serve(httpsLn, func(c net.Conn) { s.acceptClient(c, func(c net.Conn) { s.serveHTTPS(c, port) }) })
	}
	context.AfterFunc(ctx, func() { s.Close() })

	attrs := []any{"version", s.opts.Version, "fingerprint", s.Fingerprint()}
	for _, a := range []struct {
		name string
		addr net.Addr
	}{{"tunnel", s.addrs.Tunnel}, {"http", s.addrs.HTTP}, {"https", s.addrs.HTTPS}} {
		if a.addr != nil {
			attrs = append(attrs, a.name, a.addr.String())
		}
	}
	s.log.Info("gateway started", attrs...)
	switch p := s.pairing.Load(); {
	case p != nil && s.token != nil:
		s.log.Info("paired with home; the pairing token is ignored and can be removed from the command line", "home", p.HomePin)
	case p != nil:
		s.log.Info("paired with home", "home", p.HomePin, "pairedAt", p.PairedAt)
	case s.token == nil:
		s.log.Warn("not paired and no pairing token given: start the gateway with a pairing token from Relay")
	case s.token.Expired(time.Now()):
		s.log.Warn("not paired and the pairing token has expired: create a new one in Relay", "expired", s.token.Expires)
	default:
		s.log.Info("waiting for pairing", "tokenExpires", s.token.Expires)
	}
	return nil
}

// Close stops the listeners, closes the session and every connection, and
// waits for the goroutines to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed && !s.started {
		s.mu.Unlock()
		return nil
	}
	already := s.closed
	s.closed = true
	closers := s.closers
	s.closers = nil
	for p, ln := range s.ports {
		closers = append(closers, ln)
		delete(s.ports, p)
	}
	ts := s.session
	s.session = nil
	s.active.Store(nil)
	s.mu.Unlock()

	for _, c := range closers {
		c.Close()
	}
	if ts != nil {
		ts.sess.Close()
	}
	s.connMu.Lock()
	s.closing = true
	for c := range s.conns {
		c.Close()
	}
	s.connMu.Unlock()
	s.wg.Wait()
	if !already {
		s.log.Info("gateway stopped")
	}
	return nil
}

// serve accepts connections until ln is closed. handle must not block.
func (s *Server) serve(ln net.Listener, handle func(net.Conn)) {
	defer s.wg.Done()
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of file descriptors and the like: back off instead of spinning.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.log.Warn("accept failed", "addr", ln.Addr().String(), "err", err, "retryIn", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		handle(c)
	}
}

type slotKind int

const (
	slotClient slotKind = iota
	slotHandshake
)

// admit counts c against the connection limits for its kind and tracks it
// for Close. It reports false (and c must be closed) when over a limit or
// closing; otherwise the caller must call release.
func (s *Server) admit(c net.Conn, kind slotKind) (netip.Addr, bool) {
	ip := addrPort(c.RemoteAddr()).Addr()
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closing {
		return ip, false
	}
	switch kind {
	case slotClient:
		if s.clients >= s.opts.MaxConns || s.clientsIP[ip] >= s.opts.MaxConnsPerIP {
			return ip, false
		}
		s.clients++
		s.clientsIP[ip]++
	case slotHandshake:
		if s.handshakes >= maxHandshakes || s.handIP[ip] >= maxHandshakesPerIP {
			return ip, false
		}
		s.handshakes++
		s.handIP[ip]++
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return ip, true
}

// release undoes admit. The connection is no longer closed by Close.
func (s *Server) release(c net.Conn, ip netip.Addr, kind slotKind) {
	s.connMu.Lock()
	m := s.clientsIP
	if kind == slotClient {
		s.clients--
	} else {
		s.handshakes--
		m = s.handIP
	}
	if m[ip]--; m[ip] <= 0 {
		delete(m, ip)
	}
	delete(s.conns, c)
	s.connMu.Unlock()
	s.wg.Done()
}

// listenConfig enables TCP keepalive on accepted connections; established
// pipes have no idle timeout of their own.
func listenConfig() net.ListenConfig {
	return net.ListenConfig{KeepAliveConfig: net.KeepAliveConfig{
		Enable:   true,
		Idle:     30 * time.Second,
		Interval: 15 * time.Second,
		Count:    4,
	}}
}
