package balancer

import (
	"io"
	"net"
	"testing"
	"time"
)

// Taking a connection from the pool kicks its idle watcher; a watcher that
// only starts reading after the kick must not extend the deadline again and
// make the request wait the whole server timeout.
func TestPoolGetDoesNotWaitForWatcher(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ic := &idleConn{Conn: raw, timeout: 5 * time.Second}
	ic.rearm()
	sc := &sconn{conn: ic, raw: ic}
	sc.buffers()
	defer sc.close()
	p := newConnPool()
	for i := range 2000 {
		p.put(sc)
		start := time.Now()
		got := p.get()
		if got != sc {
			t.Fatalf("iteration %d: pool returned %v", i, got)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("iteration %d: get waited %s for the idle watcher", i, d)
		}
	}
}
