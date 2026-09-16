package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// The freezable test PacketConn is not a *net.UDPConn.
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	os.Exit(m.Run())
}

func TestEcho(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		p := tr.dial(t, Config{})
		serveEcho(p.home)
		const size = 32 << 20
		st := open(t, p.gw)
		defer st.Close()
		errc := make(chan error, 1)
		go func() { errc <- pump(st, 1, size) }()
		got, n, err := drain(st)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := waitErr(t, errc, "write"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != size || got != sum(1, size) {
			t.Fatalf("echo corrupted: got %d bytes", n)
		}
	})
}

func TestFullDuplex(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		// Small windows so both directions block on flow control at once.
		p := tr.dial(t, Config{StreamWindow: 128 << 10, ConnWindow: 512 << 10})
		const size = 8 << 20
		type result struct {
			sum [32]byte
			n   int64
			err error
		}
		homeRes := make(chan result, 1)
		homeWrite := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st, err := p.home.AcceptStream(ctx)
			if err != nil {
				homeRes <- result{err: err}
				homeWrite <- err
				return
			}
			defer st.Close()
			pumped := make(chan error, 1)
			go func() { pumped <- pump(st, 2, size) }()
			s, n, err := drain(st)
			homeRes <- result{s, n, err}
			homeWrite <- <-pumped
		}()
		st := open(t, p.gw)
		defer st.Close()
		gwWrite := make(chan error, 1)
		go func() { gwWrite <- pump(st, 1, size) }()
		gotSum, n, err := drain(st)
		if err != nil || n != size || gotSum != sum(2, size) {
			t.Fatalf("gateway read: n=%d err=%v sum ok=%v", n, err, gotSum == sum(2, size))
		}
		if err := waitErr(t, gwWrite, "gateway write"); err != nil {
			t.Fatalf("gateway write: %v", err)
		}
		var hr result
		select {
		case hr = <-homeRes:
		case <-time.After(10 * time.Second):
			t.Fatal("home read blocked")
		}
		if hr.err != nil || hr.n != size || hr.sum != sum(1, size) {
			t.Fatalf("home read: n=%d err=%v", hr.n, hr.err)
		}
		if err := waitErr(t, homeWrite, "home write"); err != nil {
			t.Fatalf("home write: %v", err)
		}
	})
}

func TestHalfClose(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		p := tr.dial(t, Config{})
		errc := make(chan error, 1)
		go func() {
			errc <- func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				st, err := p.home.AcceptStream(ctx)
				if err != nil {
					return err
				}
				defer st.Close()
				req, err := io.ReadAll(st)
				if err != nil {
					return fmt.Errorf("home read: %w", err)
				}
				if string(req) != "ping" {
					return fmt.Errorf("home read %q", req)
				}
				if _, err := st.Write([]byte("pong")); err != nil {
					return fmt.Errorf("home write after peer EOF: %w", err)
				}
				return st.CloseWrite()
			}()
		}()
		st := open(t, p.gw)
		defer st.Close()
		if _, err := st.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if err := st.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write([]byte("x")); err == nil {
			t.Fatal("Write after CloseWrite succeeded")
		}
		reply, err := io.ReadAll(st)
		if err != nil || string(reply) != "pong" {
			t.Fatalf("gateway read %q, %v", reply, err)
		}
		if n, err := st.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Fatalf("read after EOF: %d, %v", n, err)
		}
		if err := waitErr(t, errc, "home"); err != nil {
			t.Fatal(err)
		}
	})
}

// Close right after CloseWrite must not discard data already written.
func TestCloseAfterCloseWriteDelivers(t *testing.T) {
	const size = 1 << 20
	forEach(t, func(t *testing.T, tr transport) {
		t.Run("gateway", func(t *testing.T) {
			p := tr.dial(t, Config{})
			errc := make(chan error, 1)
			go func() {
				errc <- func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					st, err := p.home.AcceptStream(ctx)
					if err != nil {
						return err
					}
					defer st.Close()
					s, n, err := drain(st)
					if err != nil || n != size || s != sum(3, size) {
						return fmt.Errorf("home got %d bytes, %v", n, err)
					}
					return nil
				}()
			}()
			st := open(t, p.gw)
			if err := pump(st, 3, size); err != nil {
				t.Fatal(err)
			}
			st.Close()
			if err := waitErr(t, errc, "home"); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("home", func(t *testing.T) {
			p := tr.dial(t, Config{})
			errc := make(chan error, 1)
			go func() {
				errc <- func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					st, err := p.home.AcceptStream(ctx)
					if err != nil {
						return err
					}
					err = pump(st, 4, size)
					st.Close()
					return err
				}()
			}()
			st := open(t, p.gw)
			defer st.Close()
			s, n, err := drain(st)
			if err != nil || n != size || s != sum(4, size) {
				t.Fatalf("gateway got %d bytes, %v", n, err)
			}
			if err := waitErr(t, errc, "home"); err != nil {
				t.Fatal(err)
			}
		})
	})
}

func TestManyStreams(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		p := tr.dial(t, Config{})
		serveEcho(p.home)
		const streams = 1000
		var wg sync.WaitGroup
		errc := make(chan error, streams)
		for i := range streams {
			wg.Go(func() {
				errc <- func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					st, err := p.gw.OpenStream(ctx)
					if err != nil {
						return fmt.Errorf("open %d: %w", i, err)
					}
					defer st.Close()
					msg := bytes.Repeat([]byte(fmt.Sprintf("stream %d|", i)), 100)
					if _, err := st.Write(msg); err != nil {
						return fmt.Errorf("write %d: %w", i, err)
					}
					if err := st.CloseWrite(); err != nil {
						return err
					}
					got, err := io.ReadAll(st)
					if err != nil {
						return fmt.Errorf("read %d: %w", i, err)
					}
					if !bytes.Equal(got, msg) {
						return fmt.Errorf("stream %d: echo mismatch (%d bytes)", i, len(got))
					}
					return nil
				}()
			})
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestCloseUnblocks(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		t.Run("session", func(t *testing.T) {
			p := tr.dial(t, Config{})
			gst := open(t, p.gw)
			if _, err := gst.Write([]byte("hi")); err != nil {
				t.Fatal(err)
			}
			hst := accept(t, p.home)
			if _, err := io.ReadFull(hst, make([]byte, 2)); err != nil {
				t.Fatal(err)
			}
			gRead, hRead, hAccept := make(chan error, 1), make(chan error, 1), make(chan error, 1)
			go func() { _, err := gst.Read(make([]byte, 1)); gRead <- err }()
			go func() { _, err := hst.Read(make([]byte, 1)); hRead <- err }()
			go func() { _, err := p.home.AcceptStream(context.Background()); hAccept <- err }()
			if !blocked(gRead) || !blocked(hRead) || !blocked(hAccept) {
				t.Fatal("calls did not block")
			}
			if p.gw.Err() != nil || p.home.Err() != nil {
				t.Fatal("Err set on a live session")
			}
			p.gw.Close()
			waitDone(t, p.gw, time.Second, "gateway")
			if err := p.gw.Err(); !errors.Is(err, ErrClosed) {
				t.Fatalf("gateway Err = %v", err)
			}
			if err := waitErr(t, gRead, "gateway Read"); err == nil {
				t.Fatal("gateway Read succeeded")
			}
			waitDone(t, p.home, 5*time.Second, "home")
			if p.home.Err() == nil {
				t.Fatal("home Err nil after peer closed")
			}
			if err := waitErr(t, hRead, "home Read"); err == nil {
				t.Fatal("home Read succeeded")
			}
			if err := waitErr(t, hAccept, "AcceptStream"); !errors.Is(err, ErrClosed) {
				t.Fatalf("AcceptStream = %v", err)
			}
			if _, err := p.gw.OpenStream(context.Background()); !errors.Is(err, ErrClosed) {
				t.Fatalf("OpenStream on closed session = %v", err)
			}
		})
		t.Run("accept", func(t *testing.T) {
			p := tr.dial(t, Config{})
			errc := make(chan error, 1)
			go func() { _, err := p.home.AcceptStream(context.Background()); errc <- err }()
			if !blocked(errc) {
				t.Fatal("AcceptStream did not block")
			}
			p.home.Close()
			if err := waitErr(t, errc, "AcceptStream"); !errors.Is(err, ErrClosed) {
				t.Fatalf("AcceptStream = %v", err)
			}
			if err := p.home.Err(); !errors.Is(err, ErrClosed) {
				t.Fatalf("home Err = %v", err)
			}
			waitDone(t, p.gw, 5*time.Second, "gateway")
		})
		t.Run("open", func(t *testing.T) {
			p := tr.dial(t, Config{MaxStreams: 1})
			open(t, p.gw) // never accepted: fills the only slot
			errc := make(chan error, 1)
			go func() { _, err := p.gw.OpenStream(context.Background()); errc <- err }()
			if !blocked(errc) {
				t.Fatalf("OpenStream did not block: %v", <-errc)
			}
			p.gw.Close()
			if err := waitErr(t, errc, "OpenStream"); !errors.Is(err, ErrClosed) {
				t.Fatalf("OpenStream = %v", err)
			}
		})
		t.Run("stream", func(t *testing.T) {
			p := tr.dial(t, Config{StreamWindow: 128 << 10, ConnWindow: 512 << 10})
			gst := open(t, p.gw)
			hst := accept(t, p.home)
			big := make([]byte, 8<<20)

			// Reads on both ends.
			gRead, hRead := make(chan error, 1), make(chan error, 1)
			go func() { _, err := gst.Read(make([]byte, 1)); gRead <- err }()
			go func() { _, err := hst.Read(make([]byte, 1)); hRead <- err }()
			if !blocked(gRead) || !blocked(hRead) {
				t.Fatal("reads did not block")
			}
			gst.Close()
			hst.Close()
			if err := waitErr(t, gRead, "gateway Read"); err == nil {
				t.Fatal("gateway Read succeeded")
			}
			if err := waitErr(t, hRead, "home Read"); err == nil {
				t.Fatal("home Read succeeded")
			}

			// Writes blocked on flow control, on both ends.
			for _, side := range []string{"gateway", "home"} {
				gst := open(t, p.gw)
				hst := accept(t, p.home)
				w := gst
				if side == "home" {
					w = hst
				}
				errc := make(chan error, 1)
				go func() { _, err := w.Write(big); errc <- err }()
				if !blocked(errc) {
					t.Fatalf("%s Write did not block: %v", side, <-errc)
				}
				w.Close()
				if err := waitErr(t, errc, side+" Write"); err == nil {
					t.Fatalf("%s Write succeeded", side)
				}
				gst.Close()
				hst.Close()
			}
			if p.gw.Err() != nil || p.home.Err() != nil {
				t.Fatal("closing streams killed the session")
			}
		})
	})
}

func TestPeerGone(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		if tr.name == "pipe" {
			t.Skip("no network")
		}
		cfg := Config{KeepAlive: 100 * time.Millisecond, IdleTimeout: 500 * time.Millisecond}
		p := tr.dial(t, cfg)
		gst := open(t, p.gw)
		if _, err := gst.Write([]byte("hi")); err != nil {
			t.Fatal(err)
		}
		hst := accept(t, p.home)
		if _, err := io.ReadFull(hst, make([]byte, 2)); err != nil {
			t.Fatal(err)
		}
		// Idle for longer than IdleTimeout: keepalives hold the session up.
		time.Sleep(time.Second)
		if p.gw.Err() != nil || p.home.Err() != nil {
			t.Fatalf("idle session died: %v / %v", p.gw.Err(), p.home.Err())
		}
		gRead := make(chan error, 1)
		go func() { _, err := gst.Read(make([]byte, 1)); gRead <- err }()
		start := time.Now()
		p.freeze()
		waitDone(t, p.gw, 5*time.Second, "gateway")
		waitDone(t, p.home, 5*time.Second, "home")
		if d := time.Since(start); d > 3*cfg.IdleTimeout {
			t.Errorf("detection took %v", d)
		}
		for _, s := range []Session{p.gw, p.home} {
			if err := s.Err(); err == nil || errors.Is(err, ErrClosed) {
				t.Errorf("Err = %v, want the failure", err)
			}
		}
		if err := waitErr(t, gRead, "gateway Read"); err == nil {
			t.Fatal("Read succeeded")
		}
		t.Logf("detected in %v: gateway %v, home %v", time.Since(start), p.gw.Err(), p.home.Err())
	})
}

func TestWrongSide(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		p := tr.dial(t, Config{})
		if _, err := p.home.OpenStream(context.Background()); err != ErrWrongSide {
			t.Errorf("home OpenStream = %v", err)
		}
		if _, err := p.gw.AcceptStream(context.Background()); err != ErrWrongSide {
			t.Errorf("gateway AcceptStream = %v", err)
		}
		if p.gw.Transport() != tr.name || p.home.Transport() != tr.name {
			t.Errorf("Transport = %q/%q", p.gw.Transport(), p.home.Transport())
		}
		if p.gw.LocalAddr() == nil || p.gw.RemoteAddr() == nil || p.home.LocalAddr() == nil || p.home.RemoteAddr() == nil {
			t.Error("nil address")
		}
	})
}

func TestOpenStreamContext(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		p := tr.dial(t, Config{MaxStreams: 1, OpenTimeout: 300 * time.Millisecond})
		first := open(t, p.gw)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := p.gw.OpenStream(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline: %v", err)
		}

		ctx, cancel = context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		if _, err := p.gw.OpenStream(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}

		start := time.Now()
		if _, err := p.gw.OpenStream(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("OpenTimeout: %v", err)
		}
		if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
			t.Fatalf("OpenTimeout took %v", d)
		}

		// The slot frees once the first stream is done.
		hst := accept(t, p.home)
		first.Close()
		hst.Close()
		st := open(t, p.gw)
		st.Close()
		if p.gw.Err() != nil {
			t.Fatal(p.gw.Err())
		}
	})
}

func TestDeadline(t *testing.T) {
	forEach(t, func(t *testing.T, tr transport) {
		p := tr.dial(t, Config{})
		for _, side := range []string{"gateway", "home"} {
			// Separate streams: on HTTP/2 an expired deadline aborts the stream.
			gst := open(t, p.gw)
			hst := accept(t, p.home)
			st := gst
			if side == "home" {
				st = hst
			}
			st.SetDeadline(time.Now().Add(time.Hour))
			st.SetDeadline(time.Now().Add(100 * time.Millisecond))
			start := time.Now()
			_, err := st.Read(make([]byte, 1))
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("%s Read = %v", side, err)
			}
			if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
				t.Fatalf("%s deadline fired after %v", side, d)
			}
			gst.Close()
			hst.Close()
		}
	})
}
