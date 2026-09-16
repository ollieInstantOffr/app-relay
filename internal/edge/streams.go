package edge

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/proxyproto"
)

// streamRT is a compiled Stream. Listeners keep a pointer to it that reloads
// swap, so forward targets and timeouts change without rebinding.
type streamRT struct {
	id            string
	forwardHost   string
	listenLo      int
	forwardLo     int
	forwardHi     int
	proxyProtocol bool
	idle          time.Duration
	connect       time.Duration
}

// target maps a listen port to the upstream "host:port". Hostnames are
// resolved by the dialer on every connection.
func (s *streamRT) target(listenPort int) string {
	port := s.forwardLo
	if s.forwardHi != s.forwardLo {
		port = s.forwardLo + (listenPort - s.listenLo)
	}
	return joinHostPort(s.forwardHost, port)
}

// StreamStats is reported per finished stream session (for the access log).
type StreamStats struct {
	Proto       string // tcp | udp
	Client      net.Addr
	Upstream    string
	BytesIn     int64 // client → upstream
	BytesOut    int64 // upstream → client
	Duration    time.Duration
	Connected   bool
	ConnectTime time.Duration
	Err         error
	ListenPort  int
	Tunnel      string // gateway id for connections that came through a tunnel
}

// status mirrors nginx's stream $status: 200 ok, 502 upstream unreachable,
// 500 internal failure after connecting.
func (st StreamStats) status() int {
	switch {
	case st.Err == nil:
		return 200
	case !st.Connected:
		return 502
	}
	return 500
}

type streamDone func(spec *streamRT, st StreamStats)

type tcpStream struct {
	ln     net.Listener
	port   int
	spec   *atomic.Pointer[streamRT]
	onDone streamDone

	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func newTCPStream(ln net.Listener, port int, spec *atomic.Pointer[streamRT], onDone streamDone) *tcpStream {
	return &tcpStream{ln: ln, port: port, spec: spec, onDone: onDone, conns: map[net.Conn]struct{}{}}
}

func (t *tcpStream) serve() {
	port := t.port
	for {
		c, err := t.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.handle(c, port)
		}()
	}
}

func (t *tcpStream) track(c net.Conn, add bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if add {
		if t.closed {
			return false
		}
		t.conns[c] = struct{}{}
	} else {
		delete(t.conns, c)
	}
	return true
}

func (t *tcpStream) handle(client net.Conn, port int) {
	spec := t.spec.Load()
	start := time.Now()
	st := StreamStats{Proto: "tcp", Client: client.RemoteAddr(), ListenPort: port, Tunnel: tunnelOf(client)}
	defer func() {
		st.Duration = time.Since(start)
		if t.onDone != nil {
			t.onDone(spec, st)
		}
	}()
	if !t.track(client, true) {
		client.Close()
		return
	}
	defer t.track(client, false)
	defer client.Close()

	target := spec.target(port)
	st.Upstream = target
	upstream, err := net.DialTimeout("tcp", target, spec.connect)
	if err != nil {
		st.Err = err
		return
	}
	st.Connected, st.ConnectTime, st.Upstream = true, time.Since(start), upstream.RemoteAddr().String()
	if !t.track(upstream, true) {
		upstream.Close()
		return
	}
	defer t.track(upstream, false)
	defer upstream.Close()
	if spec.proxyProtocol {
		upstream.SetWriteDeadline(time.Now().Add(spec.connect))
		if _, err := upstream.Write(proxyproto.V1(client.RemoteAddr(), client.LocalAddr())); err != nil {
			st.Err = err
			return
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		st.BytesIn = copyIdle(upstream, client, spec.idle)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		st.BytesOut = copyIdle(client, upstream, spec.idle)
		closeWrite(client)
	}()
	wg.Wait()
}

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// copyIdle copies src to dst, giving up when src sees no traffic for idle
// (nginx proxy_timeout applies to each direction separately).
func copyIdle(dst, src net.Conn, idle time.Duration) int64 {
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	buf := *bp
	var n int64
	for {
		if idle > 0 {
			src.SetReadDeadline(time.Now().Add(idle))
		}
		r, err := src.Read(buf)
		if r > 0 {
			if idle > 0 {
				dst.SetWriteDeadline(time.Now().Add(idle))
			}
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				// Unblock the other direction too.
				src.Close()
				return n
			}
		}
		if err != nil {
			if isTimeout(err) {
				dst.Close()
			}
			return n
		}
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// shutdown stops accepting and waits up to ctx for sessions to finish, then
// closes the remaining ones.
func (t *tcpStream) shutdown(ctx context.Context) {
	t.ln.Close()
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	t.mu.Lock()
	t.closed = true
	for c := range t.conns {
		c.Close()
	}
	t.mu.Unlock()
	<-done
}

// ---------------------------------------------------------------- UDP

// maxUDPSessions bounds the per-port session table.
const maxUDPSessions = 16384

type udpStream struct {
	pc     net.PacketConn
	spec   *atomic.Pointer[streamRT]
	onDone streamDone

	mu       sync.Mutex
	sessions map[string]*udpSession
	closed   bool
	wg       sync.WaitGroup
	stop     chan struct{}
}

type udpSession struct {
	spec     *streamRT
	client   net.Addr
	upstream *net.UDPConn
	started  time.Time
	last     time.Time
	in, out  int64
}

func newUDPStream(pc net.PacketConn, spec *atomic.Pointer[streamRT], onDone streamDone) *udpStream {
	return &udpStream{pc: pc, spec: spec, onDone: onDone, sessions: map[string]*udpSession{}, stop: make(chan struct{})}
}

func (u *udpStream) serve() {
	port := u.pc.LocalAddr().(*net.UDPAddr).Port
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.reap()
	}()
	buf := make([]byte, 65535)
	for {
		n, addr, err := u.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s := u.session(addr, port)
		if s == nil {
			continue
		}
		if w, err := s.upstream.Write(buf[:n]); err == nil {
			u.mu.Lock()
			s.in += int64(w)
			s.last = time.Now()
			u.mu.Unlock()
		}
	}
}

func (u *udpStream) session(addr net.Addr, port int) *udpSession {
	key := addr.String()
	u.mu.Lock()
	if s := u.sessions[key]; s != nil {
		u.mu.Unlock()
		return s
	}
	if u.closed || len(u.sessions) >= maxUDPSessions {
		u.mu.Unlock()
		return nil
	}
	u.mu.Unlock()
	spec := u.spec.Load()
	start := time.Now()
	target := spec.target(port)
	raddr, err := net.ResolveUDPAddr("udp", target)
	var conn *net.UDPConn
	if err == nil {
		conn, err = net.DialUDP("udp", nil, raddr)
	}
	if err != nil {
		if u.onDone != nil {
			u.onDone(spec, StreamStats{Proto: "udp", Client: addr, Upstream: target, Err: err, ListenPort: port, Duration: time.Since(start)})
		}
		return nil
	}
	now := time.Now()
	s := &udpSession{spec: spec, client: addr, upstream: conn, started: start, last: now}
	u.mu.Lock()
	if existing := u.sessions[key]; existing != nil || u.closed {
		u.mu.Unlock()
		conn.Close()
		return existing
	}
	u.sessions[key] = s
	u.mu.Unlock()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		buf := make([]byte, 65535)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if w, err := u.pc.WriteTo(buf[:n], addr); err == nil {
				u.mu.Lock()
				s.out += int64(w)
				s.last = time.Now()
				u.mu.Unlock()
			}
		}
	}()
	return s
}

func (u *udpStream) reap() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-u.stop:
			return
		case <-t.C:
		}
		u.mu.Lock()
		now := time.Now()
		for k, s := range u.sessions {
			if now.Sub(s.last) > s.spec.idle {
				delete(u.sessions, k)
				u.finish(s)
			}
		}
		u.mu.Unlock()
	}
}

// finish closes a session and reports it; u.mu must be held.
func (u *udpStream) finish(s *udpSession) {
	s.upstream.Close()
	if u.onDone != nil {
		st := StreamStats{Proto: "udp", Client: s.client, Upstream: s.upstream.RemoteAddr().String(), BytesIn: s.in, BytesOut: s.out,
			Duration: s.last.Sub(s.started), Connected: true, ListenPort: u.pc.LocalAddr().(*net.UDPAddr).Port}
		go u.onDone(s.spec, st)
	}
}

func (u *udpStream) shutdown() {
	u.pc.Close()
	u.mu.Lock()
	if !u.closed {
		u.closed = true
		close(u.stop)
	}
	for k, s := range u.sessions {
		delete(u.sessions, k)
		u.finish(s)
	}
	u.mu.Unlock()
	u.wg.Wait()
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
