package balancer

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

func TestHTTPHealthTransitions(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var checks atomic.Int64
	u := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			checks.Add(1)
			if r.Host != "app.lan" || r.Proto != "HTTP/1.1" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(int(status.Load()))
			return
		}
		io.WriteString(w, "a")
	})
	s := u.server()
	s.Check = true
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Check = &spec.HealthCheck{Type: spec.CheckHTTP, Method: "GET", Path: "/health", Host: "app.lan", Expect: "200-299,404", IntervalMs: 20, Rise: 2, Fall: 2}
	}, s)
	st := func() map[string]string { return e.stat("web", "a") }
	if !waitFor(t, 3*time.Second, func() bool { return st()["status"] == "UP" && st()["check_status"] == "L7OK" }) {
		t.Fatalf("not UP: %v", st())
	}
	if v := st(); v["check_code"] != "200" || v["check_health"] != "3" || v["check_desc"] != "Layer7 check passed" {
		t.Fatalf("up row: %v", v)
	}
	status.Store(404) // accepted by the expect list
	time.Sleep(100 * time.Millisecond)
	if v := st(); v["status"] != "UP" || v["check_code"] != "404" {
		t.Fatalf("404 in expect list: %v", v)
	}
	status.Store(503)
	if !waitFor(t, 3*time.Second, func() bool { return st()["status"] == "DOWN" }) {
		t.Fatalf("not DOWN: %v", st())
	}
	if v := st(); v["check_status"] != "L7STS" || v["check_code"] != "503" || v["chkdown"] != "1" || v["chkfail"] != "2" {
		t.Fatalf("down row: %v", v)
	}
	if r := get(t, e.feURL(0, "/")); r.StatusCode != 503 {
		t.Fatalf("traffic to a DOWN server: %d", r.StatusCode)
	}
	e.waitLog(t, "Server web/a is DOWN, reason: Layer7 wrong status, code: 503, info: \"503 Service Unavailable (expected 200-299,404)\", check duration: ")
	e.waitLog(t, "0 active and 0 backup servers left.")
	e.waitLog(t, "backend web has no server available!")

	// Maintenance pauses checks.
	e.cmd("set server web/a state maint")
	time.Sleep(60 * time.Millisecond)
	n := checks.Load()
	time.Sleep(120 * time.Millisecond)
	if checks.Load() > n+1 {
		t.Fatalf("checks continued in maintenance: %d → %d", n, checks.Load())
	}
	status.Store(200)
	e.cmd("set server web/a state ready")
	if !waitFor(t, 3*time.Second, func() bool { return st()["status"] == "UP" }) {
		t.Fatalf("not back UP: %v", st())
	}
	if r := get(t, e.feURL(0, "/")); r.StatusCode != 200 {
		t.Fatalf("traffic after recovery: %d", r.StatusCode)
	}

	// Going down shows HAProxy's intermediate status.
	status.Store(500)
	if !waitFor(t, 3*time.Second, func() bool { return st()["status"] == "DOWN" }) {
		t.Fatal("down again")
	}
	status.Store(200)
	sawGoingUp := waitFor(t, 3*time.Second, func() bool { return st()["status"] == "DOWN 1/2" })
	if !waitFor(t, 3*time.Second, func() bool { return st()["status"] == "UP" }) || !sawGoingUp {
		t.Logf("DOWN 1/2 observed: %v", sawGoingUp)
	}
	e.waitLog(t, "Server web/a is UP, reason: Layer7 check passed, code: 200, check duration: ")
}

func TestFreshServerGoesDownOnFirstFailure(t *testing.T) {
	s := spec.Server{Name: "dead", Address: "127.0.0.1", Port: closedPort(t), Weight: 1, State: spec.StateReady, Check: true}
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Check = &spec.HealthCheck{Type: spec.CheckTCP, IntervalMs: 100, Rise: 2, Fall: 3}
	}, s)
	if st := e.stat("web", "dead"); st["status"] != "UP 1/3" || st["check_status"] != "INI" {
		t.Fatalf("fresh server: %v", st)
	}
	if !waitFor(t, 2*time.Second, func() bool { return e.stat("web", "dead")["status"] == "DOWN" }) {
		t.Fatalf("status %v", e.stat("web", "dead"))
	}
	if st := e.stat("web", "dead"); st["chkfail"] != "1" || st["chkdown"] != "1" {
		t.Fatalf("one failure must bring a fresh server down: %v", st)
	}
}

func TestProtocolHealthChecks(t *testing.T) {
	pg := tcpServer(t, func(c net.Conn) {
		var n [4]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return
		}
		io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(n[:])-4))
		c.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3})
	})
	my := tcpServer(t, func(c net.Conn) {
		payload := append([]byte{0x0a}, "10.11.6-MariaDB\x00"...)
		c.Write(append([]byte{byte(len(payload)), 0, 0, 0}, payload...))
	})
	redis := tcpServer(t, func(c net.Conn) {
		if l, _ := bufio.NewReader(c).ReadString('\n'); l == "PING\r\n" {
			io.WriteString(c, "+PONG\r\n")
		}
	})
	badRedis := tcpServer(t, func(c net.Conn) {
		bufio.NewReader(c).ReadString('\n')
		io.WriteString(c, "-LOADING\r\n")
	})
	cfg := baseConfig(t)
	mk := func(name, typ string, port int) spec.Backend {
		b := tcpBackend(name, spec.Server{Name: name, Address: "127.0.0.1", Port: port, Weight: 1, State: spec.StateReady, Check: true})
		b.Check = &spec.HealthCheck{Type: typ, User: "relay", IntervalMs: 20, Rise: 1, Fall: 1}
		return b
	}
	cfg.Backends = []spec.Backend{mk("pg", spec.CheckPgSQL, pg), mk("my", spec.CheckMySQL, my), mk("redis", spec.CheckRedis, redis), mk("bad", spec.CheckRedis, badRedis)}
	e := startEnv(t, cfg)
	for _, c := range []struct{ name, status, check, info string }{
		{"pg", "UP", "L7OK", "PostgreSQL server is ok"},
		{"my", "UP", "L7OK", "MariaDB"},
		{"redis", "UP", "L7OK", "PONG"},
		{"bad", "DOWN", "L7STS", "LOADING"},
	} {
		if !waitFor(t, 3*time.Second, func() bool {
			st := e.stat(c.name, c.name)
			return st["status"] == c.status && st["check_status"] == c.check && strings.Contains(st["last_chk"], c.info)
		}) {
			t.Errorf("%s: %v", c.name, e.stat(c.name, c.name))
		}
	}
}

func TestStateSurvivesReload(t *testing.T) {
	var healthy atomic.Bool
	u := newUpstream(t, "a", func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(500)
		}
	})
	b := newUpstream(t, "b", nil)
	sa, sb := u.server(), b.server()
	sa.Check = true
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Check = &spec.HealthCheck{Type: spec.CheckHTTP, IntervalMs: 20, Rise: 3, Fall: 1}
	}, sa, sb)
	if !waitFor(t, 3*time.Second, func() bool { return e.stat("web", "a")["status"] == "DOWN" }) {
		t.Fatal("not down")
	}
	e.cmd("set server web/b state drain")
	e.cmd("set server web/b weight 42")
	e.cfg.Backends[0].Retries = 2 // any change
	e.cfg.Check.IntervalMs = 25
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	if st := e.stat("web", "a"); st["status"] != "DOWN" || st["chkdown"] != "1" {
		t.Fatalf("health lost on reload: %v", st)
	}
	if st := e.stat("web", "b"); st["status"] != "DRAIN" || st["weight"] != "42" {
		t.Fatalf("admin state / weight lost on reload: %v", st)
	}
	// A config that changes the server's state or weight wins.
	e.cfg.Backends[0].Servers[1].State = spec.StateMaint
	e.cfg.Backends[0].Servers[1].Weight = 5
	if err := e.reload("h3"); err != nil {
		t.Fatal(err)
	}
	if st := e.stat("web", "b"); st["status"] != "MAINT" || st["weight"] != "5" {
		t.Fatalf("config change must apply: %v", st)
	}
	// A server whose address changed starts fresh.
	healthy.Store(true)
	e.cfg.Backends[0].Servers[0].Address = "localhost"
	e.srv.opts.Resolve = func(ctx context.Context, host string) (netip.Addr, error) {
		return netip.MustParseAddr("127.0.0.1"), nil
	}
	if err := e.reload("h4"); err != nil {
		t.Fatal(err)
	}
	if st := e.stat("web", "a"); st["chkdown"] != "0" || !strings.HasPrefix(st["status"], "UP") || st["addr"] != "127.0.0.1:"+st["addr"][len("127.0.0.1:"):] {
		t.Fatalf("new address must start fresh: %v", st)
	}
}

func TestUnresolvableServerStartsDown(t *testing.T) {
	u := newUpstream(t, "ok", nil)
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{httpBackend("web",
		spec.Server{Name: "ghost", Address: "ghost.invalid", Port: 80, Weight: 100, State: spec.StateReady},
		u.server())}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web")}
	e := &env{t: t, cfg: cfg, stderr: &syncBuffer{}}
	e.srv = NewServer(Options{Stderr: e.stderr, Resolve: func(ctx context.Context, host string) (netip.Addr, error) {
		return netip.Addr{}, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}})
	if err := e.srv.Start(cfg, "h1"); err != nil {
		t.Fatalf("load must succeed: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.srv.Shutdown(ctx)
		e.srv.Close()
	})
	if st := e.stat("web", "ghost"); st["status"] != "DOWN" || st["addr"] != "ghost.invalid:80" {
		t.Fatalf("unresolved server: %v", st)
	}
	for range 4 {
		if r := get(t, e.feURL(0, "/")); !strings.HasPrefix(r.body, "ok") {
			t.Fatalf("traffic to unresolved server: %q", r.body)
		}
	}
}
