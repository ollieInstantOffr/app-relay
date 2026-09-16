package pair

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPairHappyPathThenPinnedTunnel(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)

	r := runPair(t, home, gw, tok, tok)
	if r.homeErr != nil || r.gatewayErr != nil {
		t.Fatalf("pair: home %v, gateway %v", r.homeErr, r.gatewayErr)
	}
	if r.homePin != gw.Fingerprint() {
		t.Errorf("home pinned %q, want gateway %q", r.homePin, gw.Fingerprint())
	}
	if r.gatewayPin != home.Fingerprint() {
		t.Errorf("gateway pinned %q, want home %q", r.gatewayPin, home.Fingerprint())
	}

	client, server, cerr, serr := handshake(t,
		ClientConfig(home, r.homePin, tunnelALPN),
		ServerConfig(gw, fixedPin(r.gatewayPin), tunnelALPN))
	if cerr != nil || serr != nil {
		t.Fatalf("pinned handshake: client %v, server %v", cerr, serr)
	}
	if p := client.ConnectionState().NegotiatedProtocol; p != tunnelALPN {
		t.Errorf("client negotiated %q", p)
	}
	if p := server.ConnectionState().NegotiatedProtocol; p != tunnelALPN {
		t.Errorf("server negotiated %q", p)
	}
	if cerr, serr := connect(t,
		ClientConfig(home, r.homePin, tunnelALPN),
		ServerConfig(gw, fixedPin(r.gatewayPin), tunnelALPN)); cerr != nil || serr != nil {
		t.Fatalf("pinned exchange: client %v, server %v", cerr, serr)
	}

	other := mustIdentity(t, "other")
	t.Run("wrong gateway pin on client", func(t *testing.T) {
		cerr, serr := connect(t,
			ClientConfig(home, other.Fingerprint(), tunnelALPN),
			ServerConfig(gw, fixedPin(r.gatewayPin), tunnelALPN))
		if cerr == nil || serr == nil {
			t.Fatalf("want both sides to fail: client %v, server %v", cerr, serr)
		}
	})
	t.Run("wrong home pin on server", func(t *testing.T) {
		cerr, serr := connect(t,
			ClientConfig(home, r.homePin, tunnelALPN),
			ServerConfig(gw, fixedPin(other.Fingerprint()), tunnelALPN))
		if cerr == nil || serr == nil {
			t.Fatalf("want both sides to fail: client %v, server %v", cerr, serr)
		}
	})
	t.Run("impostor home with the right pin configured", func(t *testing.T) {
		cerr, serr := connect(t,
			ClientConfig(other, r.homePin, tunnelALPN),
			ServerConfig(gw, fixedPin(r.gatewayPin), tunnelALPN))
		if serr == nil || cerr == nil {
			t.Fatalf("want both sides to fail: client %v, server %v", cerr, serr)
		}
	})
}

func TestPairWrongSecret(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)
	wrong := tok
	wrong.Secret[0] ^= 1

	r := runPair(t, home, gw, wrong, tok)
	if !errors.Is(r.gatewayErr, ErrBadProof) {
		t.Errorf("gateway err = %v, want ErrBadProof", r.gatewayErr)
	}
	if !errors.Is(r.homeErr, ErrBadProof) {
		t.Errorf("home err = %v, want ErrBadProof", r.homeErr)
	}
	if r.homePin != "" || r.gatewayPin != "" {
		t.Errorf("pins returned on failure: %q %q", r.homePin, r.gatewayPin)
	}
}

func TestPairExpired(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)
	expired := tok
	expired.Expires = time.Unix(time.Now().Add(-time.Minute).Unix(), 0)

	t.Run("both expired", func(t *testing.T) {
		r := runPair(t, home, gw, expired, expired)
		if !errors.Is(r.homeErr, ErrExpired) {
			t.Errorf("home err = %v, want ErrExpired", r.homeErr)
		}
		if r.gatewayErr == nil {
			t.Error("gateway paired with an expired token")
		}
	})
	t.Run("gateway token expired", func(t *testing.T) {
		// The home's copy claims a later expiry, so the gateway's own clock decides.
		r := runPair(t, home, gw, tok, expired)
		if !errors.Is(r.gatewayErr, ErrExpired) {
			t.Errorf("gateway err = %v, want ErrExpired", r.gatewayErr)
		}
		if !errors.Is(r.homeErr, ErrBadProof) {
			t.Errorf("home err = %v, want ErrBadProof", r.homeErr)
		}
	})
	t.Run("expiry differs", func(t *testing.T) {
		later := tok
		later.Expires = tok.Expires.Add(time.Hour)
		r := runPair(t, home, gw, later, tok)
		if !errors.Is(r.gatewayErr, ErrBadProof) || !errors.Is(r.homeErr, ErrBadProof) {
			t.Errorf("home %v, gateway %v; want ErrBadProof on both", r.homeErr, r.gatewayErr)
		}
	})
}

// A home that knows the token but alters the expiry field on the wire (as an
// attacker extending a token's life would) is rejected, as is one whose proof
// covers the altered expiry.
func TestPairTamperedExpiryOnWire(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)
	later := tok.expiry() + 3600

	for name, build := range map[string]func(s session) []byte{
		"field changed, proof for real expiry": func(s session) []byte {
			tr := transcript(s.ekm, s.peer, s.local, tok.expiry())
			return rawHomeMsg(msgVersion, later, proof(tok.Secret, labelHome, tr))
		},
		"field and proof changed": func(s session) []byte {
			tr := transcript(s.ekm, s.peer, s.local, later)
			return rawHomeMsg(msgVersion, later, proof(tok.Secret, labelHome, tr))
		},
		"wrong version": func(s session) []byte {
			tr := transcript(s.ekm, s.peer, s.local, tok.expiry())
			return rawHomeMsg(msgVersion+1, tok.expiry(), proof(tok.Secret, labelHome, tr))
		},
	} {
		t.Run(name, func(t *testing.T) {
			client, server, cerr, serr := handshake(t, ClientConfig(home, "", ALPN), ServerConfig(gw, noPin, ALPN))
			if cerr != nil || serr != nil {
				t.Fatal(cerr, serr)
			}
			ctx := testCtx(t)
			gwErr := make(chan error, 1)
			go func() {
				_, err := Gateway(ctx, server, tok)
				gwErr <- err
			}()
			s, err := prepare(ctx, client)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Write(build(s)); err != nil {
				t.Fatal(err)
			}
			var status [1]byte
			if _, err := io.ReadFull(client, status[:]); err != nil {
				t.Fatal(err)
			}
			if status[0] != statusFail {
				t.Errorf("gateway status = %d, want fail", status[0])
			}
			if err := <-gwErr; !errors.Is(err, ErrBadProof) {
				t.Errorf("gateway err = %v, want ErrBadProof", err)
			}
		})
	}
}

func rawHomeMsg(version byte, expiry uint64, p []byte) []byte {
	b := binary.BigEndian.AppendUint64([]byte{version}, expiry)
	return append(b, p...)
}

// countWriter counts bytes relayed through the proxy.
type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// A TLS-terminating proxy with its own identities towards each side, knowing
// no token, relays the pairing bytes verbatim. Both sides must fail because
// the proof is bound to each TLS session's exporter and the keys it saw.
func TestPairMITMRelayFails(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	mitmFront, mitmBack := mustIdentity(t, "gateway"), mustIdentity(t, "home")
	tok := mustToken(t)
	ctx := testCtx(t)

	gwLn, mitmLn := listen(t), listen(t)

	gwErr := make(chan error, 1)
	go func() {
		c, err := gwLn.Accept()
		if err != nil {
			gwErr <- err
			return
		}
		tc := tls.Server(c, ServerConfig(gw, noPin, ALPN))
		_, err = Gateway(ctx, tc, tok)
		tc.Close()
		gwErr <- err
	}()

	var toGateway, toHome atomic.Int64
	proxyErr := make(chan error, 1)
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		c, err := mitmLn.Accept()
		if err != nil {
			proxyErr <- err
			return
		}
		front := tls.Server(c, ServerConfig(mitmFront, noPin, ALPN))
		defer front.Close()
		if err := front.HandshakeContext(ctx); err != nil {
			proxyErr <- err
			return
		}
		d, err := (&tls.Dialer{Config: ClientConfig(mitmBack, "", ALPN)}).DialContext(ctx, "tcp", gwLn.Addr().String())
		if err != nil {
			proxyErr <- err
			return
		}
		back := d.(*tls.Conn)
		defer back.Close()
		proxyErr <- nil

		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = io.Copy(countWriter{back, &toGateway}, front)
			back.Close()
		})
		wg.Go(func() {
			_, _ = io.Copy(countWriter{front, &toHome}, back)
			front.Close()
		})
		wg.Wait()
	}()

	hc, err := (&tls.Dialer{Config: ClientConfig(home, "", ALPN)}).DialContext(ctx, "tcp", mitmLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	homeConn := hc.(*tls.Conn)
	if err := <-proxyErr; err != nil {
		t.Fatalf("proxy setup: %v", err)
	}
	if fp, _ := PeerFingerprint(homeConn.ConnectionState()); fp != mitmFront.Fingerprint() {
		t.Fatal("home is not connected to the proxy")
	}
	_, homeErr := Home(ctx, homeConn, tok)
	homeConn.Close()
	gatewayErr := <-gwErr
	<-relayed

	if !errors.Is(gatewayErr, ErrBadProof) {
		t.Errorf("gateway err = %v, want ErrBadProof", gatewayErr)
	}
	// ErrBadProof (not an I/O error) shows the home received the gateway's
	// rejection through the relay.
	if !errors.Is(homeErr, ErrBadProof) {
		t.Errorf("home err = %v, want ErrBadProof", homeErr)
	}
	if n := toGateway.Load(); n != homeMsgLen {
		t.Errorf("relayed %d bytes home→gateway, want %d", n, homeMsgLen)
	}
	if n := toHome.Load(); n != 1 {
		t.Errorf("relayed %d bytes gateway→home, want 1", n)
	}
}

// fakeGateway accepts one pairing connection and hands the handshaken conn to fn.
func fakeGateway(t *testing.T, gw Identity, fn func(*tls.Conn)) (addr string, done <-chan struct{}) {
	t.Helper()
	ln := listen(t)
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		tc := tls.Server(c, ServerConfig(gw, noPin, ALPN))
		defer tc.Close()
		if tc.Handshake() != nil {
			return
		}
		fn(tc)
	}()
	return ln.Addr().String(), ch
}

func dialPair(t *testing.T, home Identity, addr string) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", addr, ClientConfig(home, "", ALPN))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A gateway that does not know the token and echoes the home's proof back
// must not pass the home's verification.
func TestPairReflectedProofRejected(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)
	var ack atomic.Int32
	ack.Store(-1)
	addr, done := fakeGateway(t, gw, func(c *tls.Conn) {
		var msg [homeMsgLen]byte
		if _, err := io.ReadFull(c, msg[:]); err != nil {
			return
		}
		_, _ = c.Write(append([]byte{statusOK}, msg[9:]...))
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err == nil {
			ack.Store(int32(b[0]))
		}
	})
	_, err := Home(testCtx(t), dialPair(t, home, addr), tok)
	<-done
	if !errors.Is(err, ErrBadProof) {
		t.Fatalf("home err = %v, want ErrBadProof", err)
	}
	if ack.Load() != int32(statusFail) {
		t.Errorf("home sent %d after a bad gateway proof, want fail", ack.Load())
	}
}

// A proof recorded from one pairing session is useless in another.
func TestPairReplayedProofRejected(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)

	var recorded []byte
	addr, done := fakeGateway(t, gw, func(c *tls.Conn) {
		msg := make([]byte, homeMsgLen)
		if _, err := io.ReadFull(c, msg); err == nil {
			recorded = msg
		}
	})
	_, _ = Home(testCtx(t), dialPair(t, home, addr), tok)
	<-done
	if len(recorded) != homeMsgLen {
		t.Fatal("did not record a proof")
	}

	client, server, cerr, serr := handshake(t, ClientConfig(home, "", ALPN), ServerConfig(gw, noPin, ALPN))
	if cerr != nil || serr != nil {
		t.Fatal(cerr, serr)
	}
	ctx := testCtx(t)
	gwErr := make(chan error, 1)
	go func() {
		_, err := Gateway(ctx, server, tok)
		gwErr <- err
	}()
	if _, err := client.Write(recorded); err != nil {
		t.Fatal(err)
	}
	if err := <-gwErr; !errors.Is(err, ErrBadProof) {
		t.Fatalf("gateway err = %v, want ErrBadProof", err)
	}
}

func TestPairRequiresPairingALPN(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)
	client, server, cerr, serr := handshake(t,
		ClientConfig(home, gw.Fingerprint(), tunnelALPN),
		ServerConfig(gw, fixedPin(home.Fingerprint()), tunnelALPN))
	if cerr != nil || serr != nil {
		t.Fatal(cerr, serr)
	}
	ctx := testCtx(t)
	if _, err := Home(ctx, client, tok); err == nil {
		t.Error("Home paired over a tunnel connection")
	}
	if _, err := Gateway(ctx, server, tok); err == nil {
		t.Error("Gateway paired over a tunnel connection")
	}
}

func TestPairContext(t *testing.T) {
	home, gw := mustIdentity(t, "home"), mustIdentity(t, "gateway")
	tok := mustToken(t)

	t.Run("cancel", func(t *testing.T) {
		_, server, cerr, serr := handshake(t, ClientConfig(home, "", ALPN), ServerConfig(gw, noPin, ALPN))
		if cerr != nil || serr != nil {
			t.Fatal(cerr, serr)
		}
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		_, err := Gateway(ctx, server, tok)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Error("cancel did not unblock Gateway")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		client, _, cerr, serr := handshake(t, ClientConfig(home, "", ALPN), ServerConfig(gw, noPin, ALPN))
		if cerr != nil || serr != nil {
			t.Fatal(cerr, serr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := Home(ctx, client, tok); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}
