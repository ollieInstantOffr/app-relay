package balancer

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// Like HAProxy (servers have no maxconn to queue behind), a request that finds
// no usable server gets 503 at once instead of waiting for the queue timeout.
func TestNoUsableServerFailsAtOnce(t *testing.T) {
	u := newUpstream(t, "a", nil)
	s := u.server()
	s.State = spec.StateMaint
	e := singleBackendEnv(t, func(cfg *spec.Config) { cfg.Backends[0].Timeouts.QueueMs = 3000 }, s)
	start := time.Now()
	if r := get(t, e.feURL(0, "/")); r.StatusCode != 503 || time.Since(start) > time.Second {
		t.Fatalf("no usable server: %d after %s, want 503 at once", r.StatusCode, time.Since(start))
	}
	e.waitLog(t, " SC-- ")
	if out := e.cmd("set server web/a state ready"); strings.TrimSpace(out) != "" {
		t.Fatalf("set server: %q", out)
	}
	if r := get(t, e.feURL(0, "/")); r.StatusCode != 200 {
		t.Fatalf("after ready: %d", r.StatusCode)
	}
}

// The client timeout is an inactivity timeout on the request body too: a body
// that keeps arriving is fine even if it takes longer in total, a stalled one
// gets 408.
func TestClientTimeoutAppliesToRequestBody(t *testing.T) {
	u := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "got %d", len(b))
	})
	e := singleBackendEnv(t, func(cfg *spec.Config) { cfg.Frontends[0].Timeouts.ClientMs = 300 }, u.server())

	slow, err := net.Dial("tcp", e.cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fmt.Fprintf(slow, "POST /slow HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\n")
	for i := 0; i < 10; i++ {
		time.Sleep(60 * time.Millisecond)
		slow.Write([]byte("x"))
	}
	slow.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(slow), nil)
	if err != nil {
		t.Fatalf("slow body: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "got 10" {
		t.Fatalf("slow body: %d %q", resp.StatusCode, body)
	}

	stall, err := net.Dial("tcp", e.cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer stall.Close()
	start := time.Now()
	fmt.Fprintf(stall, "POST /stall HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n0123456789")
	stall.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err = http.ReadResponse(bufio.NewReader(stall), nil)
	if err != nil {
		t.Fatalf("stalled body: %v", err)
	}
	if resp.StatusCode != http.StatusRequestTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("stalled body: %d after %s, want 408 soon after the client timeout", resp.StatusCode, time.Since(start))
	}
	e.waitLog(t, " cD-- ")
}

// Changing a frontend's client timeout on reload must not disconnect idle
// keep-alive clients.
func TestClientTimeoutChangeKeepsKeepAliveConnections(t *testing.T) {
	u := newUpstream(t, "a", nil)
	e := singleBackendEnv(t, func(cfg *spec.Config) { cfg.Frontends[0].Timeouts.ClientMs = 5000 }, u.server())
	conn, err := net.Dial("tcp", e.cfg.Frontends[0].Bind)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	send := func(path string) int {
		t.Helper()
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path)
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s on the kept-alive connection: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := send("/before"); c != 200 {
		t.Fatalf("before reload: %d", c)
	}
	e.cfg.Frontends[0].Timeouts.ClientMs = 7000
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if c := send("/after"); c != 200 {
		t.Fatalf("after reload: %d", c)
	}
	if r := get(t, e.feURL(0, "/new")); r.StatusCode != 200 {
		t.Fatalf("new connection after reload: %d", r.StatusCode)
	}
}
