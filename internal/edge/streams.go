package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// proxyProtocolV1 returns the PROXY protocol v1 header nginx sends with
// `proxy_protocol on` for a connection from src to dst.
func proxyProtocolV1(src, dst net.Addr) []byte {
	sa, sOK := src.(*net.TCPAddr)
	da, dOK := dst.(*net.TCPAddr)
	if !sOK || !dOK {
		return []byte("PROXY UNKNOWN\r\n")
	}
	family := "TCP4"
	sip, dip := sa.IP.To4(), da.IP.To4()
	if sip == nil || dip == nil {
		family, sip, dip = "TCP6", sa.IP.To16(), da.IP.To16()
	}
	return []byte(fmt.Sprintf("PROXY %s %s %s %d %d\r\n", family, sip, dip, sa.Port, da.Port))
}

// StreamStats is reported per finished stream session (for the access log).
type StreamStats struct {
	Proto      string
	Client     net.Addr
	Upstream   string
	BytesIn    int64 // client → upstream
	BytesOut   int64 // upstream → client
	Duration   time.Duration
	Err        error
	ListenPort int
}

type tcpStream struct {
	ln            net.Listener
	target        func(listenPort int) string
	proxyProtocol bool
	idle          time.Duration
	onDone        func(StreamStats)

	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

func (t *tcpStream) serve() {
	port := t.ln.Addr().(*net.TCPAddr).Port
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
	start := time.Now()
	st := StreamStats{Proto: "tcp", Client: client.RemoteAddr(), ListenPort: port}
	defer func() {
		st.Duration = time.Since(start)
		if t.onDone != nil {
			t.onDone(st)
		}
	}()
	if !t.track(client, true) {
		client.Close()
		return
	}
	defer t.track(client, false)
	defer client.Close()

	st.Upstream = t.target(port)
	upstream, err := net.DialTimeout("tcp", st.Upstream, 10*time.Second)
	if err != nil {
		st.Err = err
		return
	}
	defer upstream.Close()
	if t.proxyProtocol {
		if _, err := upstream.Write(proxyProtocolV1(client.RemoteAddr(), client.LocalAddr())); err != nil {
			st.Err = err
			return
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		st.BytesIn = copyIdle(upstream, client, t.idle)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		st.BytesOut = copyIdle(client, upstream, t.idle)
		closeWrite(client)
	}()
	wg.Wait()
}

// copyIdle copies src to dst, giving up when neither side sees traffic for idle.
func copyIdle(dst, src net.Conn, idle time.Duration) int64 {
	buf := make([]byte, 32*1024)
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
				return n
			}
		}
		if err != nil {
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

type udpStream struct {
	pc     net.PacketConn
	target func(listenPort int) string
	idle   time.Duration
	onDone func(StreamStats)

	mu       sync.Mutex
	sessions map[string]*udpSession
	closed   bool
	wg       sync.WaitGroup
}

type udpSession struct {
	client   net.Addr
	upstream *net.UDPConn
	started  time.Time
	last     time.Time
	in, out  int64
}

func (u *udpStream) serve() {
	port := u.pc.LocalAddr().(*net.UDPAddr).Port
	idle := u.idle
	if idle <= 0 {
		idle = 30 * time.Second
	}
	go u.reap(idle)
	buf := make([]byte, 65535)
	for {
		n, addr, err := u.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s, err := u.session(addr, port)
		if err != nil {
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

func (u *udpStream) session(addr net.Addr, port int) (*udpSession, error) {
	key := addr.String()
	u.mu.Lock()
	if s := u.sessions[key]; s != nil {
		u.mu.Unlock()
		return s, nil
	}
	if u.closed {
		u.mu.Unlock()
		return nil, net.ErrClosed
	}
	u.mu.Unlock()
	target := u.target(port)
	raddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &udpSession{client: addr, upstream: conn, started: now, last: now}
	u.mu.Lock()
	if existing := u.sessions[key]; existing != nil {
		u.mu.Unlock()
		conn.Close()
		return existing, nil
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
	return s, nil
}

func (u *udpStream) reap(idle time.Duration) {
	t := time.NewTicker(idle / 3)
	defer t.Stop()
	for range t.C {
		u.mu.Lock()
		if u.closed {
			u.mu.Unlock()
			return
		}
		now := time.Now()
		for k, s := range u.sessions {
			if now.Sub(s.last) > idle {
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
		st := StreamStats{Proto: "udp", Client: s.client, Upstream: s.upstream.RemoteAddr().String(), BytesIn: s.in, BytesOut: s.out, Duration: s.last.Sub(s.started), ListenPort: u.pc.LocalAddr().(*net.UDPAddr).Port}
		go u.onDone(st)
	}
}

func (u *udpStream) shutdown() {
	u.pc.Close()
	u.mu.Lock()
	u.closed = true
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

var _ = io.EOF
