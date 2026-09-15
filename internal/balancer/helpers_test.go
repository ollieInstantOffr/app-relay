package balancer

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeBind(t testing.TB) string { return "127.0.0.1:" + strconv.Itoa(freePort(t)) }

// syncBuffer collects log output safely.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t testing.TB, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// shortSocket returns a unix socket path short enough for macOS.
func shortSocket(t testing.TB) string {
	dir, err := os.MkdirTemp("/tmp", "rb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "rt.sock")
}

func baseConfig(t testing.TB) *spec.Config {
	return &spec.Config{
		Schema:        spec.SchemaVersion,
		RuntimeSocket: shortSocket(t),
		Timeouts:      spec.Timeouts{ConnectMs: 500, ClientMs: 5000, ServerMs: 5000},
		Check:         spec.CheckDefaults{IntervalMs: 40, Rise: 2, Fall: 2},
	}
}

type env struct {
	t      testing.TB
	srv    *Server
	cfg    *spec.Config
	stderr *syncBuffer
}

func startEnv(t testing.TB, cfg *spec.Config) *env {
	t.Helper()
	e := &env{t: t, cfg: cfg, stderr: &syncBuffer{}}
	e.srv = NewServer(Options{Stderr: e.stderr, Version: "test"})
	if err := e.srv.Start(cfg, "h1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.srv.Shutdown(ctx)
		e.srv.Close()
	})
	return e
}

func (e *env) reload(hash string) error { return e.srv.Reload(e.cfg, hash) }

// logs flushes and returns everything logged so far.
func (e *env) logs() string {
	e.srv.log.flush()
	return e.stderr.String()
}

func (e *env) feURL(i int, path string) string { return "http://" + e.cfg.Frontends[i].Bind + path }

// cmd runs a runtime API command through the unix socket.
func (e *env) cmd(line string) string {
	e.t.Helper()
	c, err := net.Dial("unix", e.cfg.RuntimeSocket)
	if err != nil {
		e.t.Fatalf("runtime socket: %v", err)
	}
	defer c.Close()
	io.WriteString(c, line+"\n")
	b, _ := io.ReadAll(c)
	return string(b)
}

// stat returns the show stat row of px/sv keyed by column.
func (e *env) stat(px, sv string) map[string]string {
	e.t.Helper()
	return statFrom(e.t, e.srv.Command("show stat"), px, sv)
}

func statFrom(t testing.TB, out, px, sv string) map[string]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(out, "# ")))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		t.Fatalf("show stat: %v\n%s", err, out)
	}
	for _, rec := range recs[1:] {
		if len(rec) > 1 && rec[0] == px && rec[1] == sv {
			m := map[string]string{}
			for i, h := range recs[0] {
				if i < len(rec) {
					m[h] = rec[i]
				}
			}
			return m
		}
	}
	t.Fatalf("no show stat row %s/%s in\n%s", px, sv, out)
	return nil
}

// upstream is a test HTTP server that answers with its name.
type upstream struct {
	*httptest.Server
	name string
	port int
	hits atomic.Int64
}

func newUpstream(t testing.TB, name string, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{name: name}
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%s %s xff=%s", u.name, r.URL.RequestURI(), strings.Join(r.Header.Values("X-Forwarded-For"), "|"))
		}
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(u.Close)
	_, p, _ := net.SplitHostPort(u.Listener.Addr().String())
	u.port, _ = strconv.Atoi(p)
	return u
}

func (u *upstream) server() spec.Server {
	return spec.Server{ID: u.name, Name: u.name, Address: "127.0.0.1", Port: u.port, Weight: 100, State: spec.StateReady}
}

func httpBackend(id string, servers ...spec.Server) spec.Backend {
	return spec.Backend{ID: id, Name: id, Mode: spec.ModeHTTP, Algorithm: spec.AlgoRoundRobin, Servers: servers}
}

func tcpBackend(id string, servers ...spec.Server) spec.Backend {
	return spec.Backend{ID: id, Name: id, Mode: spec.ModeTCP, Algorithm: spec.AlgoRoundRobin, Servers: servers}
}

func httpFrontend(t testing.TB, name, def string) spec.Frontend {
	return spec.Frontend{ID: name, Name: name, Mode: spec.ModeHTTP, Bind: freeBind(t), DefaultBackend: def}
}

func tcpFrontend(t testing.TB, name, def string) spec.Frontend {
	return spec.Frontend{ID: name, Name: name, Mode: spec.ModeTCP, Bind: freeBind(t), DefaultBackend: def}
}

var testClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64},
}

type result struct {
	*http.Response
	body string
}

func do(t testing.TB, req *http.Request) result {
	t.Helper()
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return result{res, string(b)}
}

func get(t testing.TB, url string, headers ...string) result {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if strings.EqualFold(headers[i], "Host") {
			req.Host = headers[i+1]
		} else {
			req.Header.Add(headers[i], headers[i+1])
		}
	}
	return do(t, req)
}

// tcpServer accepts connections and runs handle for each.
func tcpServer(t testing.TB, handle func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoTCP writes a greeting then echoes lines prefixed with it.
func echoTCP(greeting string) func(net.Conn) {
	return func(c net.Conn) {
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			io.WriteString(c, greeting+":"+line)
		}
	}
}

func closedPort(t testing.TB) int { return freePort(t) }
