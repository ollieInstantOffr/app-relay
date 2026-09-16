package mux

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

type pair struct {
	gw, home Session
	freeze   func() // silently drops all traffic from now on; nil for pipe
}

type transport struct {
	name string
	dial func(t *testing.T, cfg Config) pair
}

var transports = []transport{
	{"pipe", pipePair},
	{"quic", quicPair},
	{"tcp", tcpPair},
}

// forEach runs test once per transport, each with its own leak check.
func forEach(t *testing.T, test func(t *testing.T, tr transport)) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) { test(t, tr) })
	}
}

func pipePair(t *testing.T, cfg Config) pair {
	checkLeaks(t)
	gw, home := Pipe(cfg)
	t.Cleanup(func() {
		gw.Close()
		home.Close()
	})
	return pair{gw: gw, home: home}
}

var (
	certOnce sync.Once
	cert     tls.Certificate
)

func testCert(t *testing.T) tls.Certificate {
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "relay-test"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			DNSNames:     []string{"relay-test"},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	})
	return cert
}

func quicPair(t *testing.T, cfg Config) pair {
	checkLeaks(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cpcRaw, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cpc := &freezePacketConn{PacketConn: cpcRaw}
	str := &quic.Transport{Conn: spc}
	ctr := &quic.Transport{Conn: cpc}
	ln, err := str.Listen(&tls.Config{Certificates: []tls.Certificate{testCert(t)}, NextProtos: []string{ALPNQUIC}}, QUICConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *quic.Conn, 1)
	go func() {
		c, _ := ln.Accept(ctx)
		accepted <- c
	}()
	cc, err := ctr.Dial(ctx, spc.LocalAddr(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{ALPNQUIC}}, QUICConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	sc := <-accepted
	if sc == nil {
		t.Fatal("accept failed")
	}
	gw, home := GatewayQUIC(sc, cfg), HomeQUIC(cc, cfg)
	t.Cleanup(func() {
		gw.Close()
		home.Close()
		ln.Close()
		str.Close()
		ctr.Close()
		spc.Close()
		cpcRaw.Close()
	})
	return pair{gw, home, func() { cpc.frozen.Store(true) }}
}

func tcpPair(t *testing.T, cfg Config) pair {
	checkLeaks(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan *tls.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{testCert(t)}, NextProtos: []string{ALPNH2}})
		if tc.HandshakeContext(ctx) != nil {
			c.Close()
			tc = nil
		}
		accepted <- tc
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fc := &freezeConn{Conn: raw, closed: make(chan struct{})}
	cc := tls.Client(fc, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{ALPNH2}})
	if err := cc.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	sc := <-accepted
	if sc == nil {
		t.Fatal("accept failed")
	}
	gw, err := GatewayTCP(sc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	home, err := HomeTCP(cc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gw.Close()
		home.Close()
	})
	return pair{gw, home, func() { fc.frozen.Store(true) }}
}

// freezePacketConn drops every datagram in both directions once frozen.
type freezePacketConn struct {
	net.PacketConn
	frozen atomic.Bool
}

func (c *freezePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		if err != nil || !c.frozen.Load() {
			return n, addr, err
		}
	}
}

func (c *freezePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.frozen.Load() {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}

// freezeConn stops passing bytes once frozen, without closing: writes are
// discarded and reads block until Close.
type freezeConn struct {
	net.Conn
	frozen atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func (c *freezeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.frozen.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return n, err
}

func (c *freezeConn) Write(b []byte) (int, error) {
	if c.frozen.Load() {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *freezeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// checkLeaks fails the test if goroutines started during it outlive its
// cleanups. Call it before registering other cleanups.
func checkLeaks(t *testing.T) {
	base := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > base {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("goroutines leaked: %d > %d\n%s", runtime.NumGoroutine(), base, buf)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func open(t *testing.T, s Session) Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := s.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	return st
}

func accept(t *testing.T, s Session) Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := s.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	return st
}

// serveEcho echoes every stream accepted on home until the session dies.
func serveEcho(home Session) {
	go func() {
		for {
			st, err := home.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				defer st.Close()
				if _, err := io.Copy(st, st); err == nil {
					st.CloseWrite()
				}
			}()
		}
	}()
}

func source(seed uint64, size int64) io.Reader {
	return io.LimitReader(mrand.NewChaCha8([32]byte{byte(seed)}), size)
}

func sum(seed uint64, size int64) [32]byte {
	h := sha256.New()
	io.Copy(h, source(seed, size))
	return [32]byte(h.Sum(nil))
}

// pump writes size bytes of seeded data to st and closes its write side.
func pump(st Stream, seed uint64, size int64) error {
	if _, err := io.Copy(st, source(seed, size)); err != nil {
		return err
	}
	return st.CloseWrite()
}

// drain reads st to EOF and returns the SHA-256 and byte count.
func drain(st Stream) ([32]byte, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, st)
	return [32]byte(h.Sum(nil)), n, err
}

// waitErr waits for a result from errc.
func waitErr(t *testing.T, errc <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: still blocked", what)
		return nil
	}
}

func waitDone(t *testing.T, s Session, within time.Duration, what string) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(within):
		t.Fatalf("%s: Done not closed within %v", what, within)
	}
}

func blocked(errc <-chan error) bool {
	select {
	case <-errc:
		return false
	case <-time.After(150 * time.Millisecond):
		return true
	}
}
