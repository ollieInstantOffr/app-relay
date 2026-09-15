package balancer_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/balancer"
	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/logs"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestLogsParseBalancerOutput feeds real Relay Balancer output to the log
// ingester's HAProxy parser (internal/logs), which must accept it unchanged.
func TestLogsParseBalancerOutput(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	_, p, _ := net.SplitHostPort(up.Listener.Addr().String())
	upPort, _ := strconv.Atoi(p)
	httpBind := "127.0.0.1:" + strconv.Itoa(freePort(t))
	tcpBind := "127.0.0.1:" + strconv.Itoa(freePort(t))
	dead := freePort(t)
	cfg := &spec.Config{
		Schema:   spec.SchemaVersion,
		Timeouts: spec.Timeouts{ConnectMs: 200},
		Frontends: []spec.Frontend{
			{Name: "web-in", Mode: spec.ModeHTTP, Bind: httpBind, DefaultBackend: "b1"},
			{Name: "pg-in", Mode: spec.ModeTCP, Bind: tcpBind, DefaultBackend: "b2"},
		},
		Backends: []spec.Backend{
			{ID: "b1", Name: "web", Mode: spec.ModeHTTP, Algorithm: spec.AlgoRoundRobin,
				Servers: []spec.Server{{Name: "web-1", Address: "127.0.0.1", Port: upPort, Weight: 100, State: spec.StateReady}}},
			{ID: "b2", Name: "pg", Mode: spec.ModeTCP, Algorithm: spec.AlgoLeastConn,
				Check:   &spec.HealthCheck{Type: spec.CheckTCP, IntervalMs: 30, Rise: 1, Fall: 1},
				Servers: []spec.Server{{Name: "pg-1", Address: "127.0.0.1", Port: dead, Weight: 100, State: spec.StateReady, Check: true}}},
		},
	}
	stderr := &lockedBuffer{}
	srv := balancer.NewServer(balancer.Options{Stderr: stderr})
	if err := srv.Start(cfg, "rel"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		srv.Close()
	}()
	res, err := http.Get("http://" + httpBind + "/p?x=1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stderr.String(), "has no server available") {
		time.Sleep(10 * time.Millisecond)
	}
	if c, err := net.Dial("tcp", tcpBind); err == nil {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.Read(make([]byte, 1))
		c.Close()
	}

	want := map[string]string{ // message substring → level
		"Proxy web-in started.":                  "notice",
		`"GET /p?x=1 HTTP/1.1"`:                  "info",
		"Server pg/pg-1 is DOWN, reason: Layer4": "warn",
		"backend pg has no server available!":    "alert",
		"] pg-in pg/<NOSRV> ":                    "info",
	}
	seen := map[string]bool{}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(seen) < len(want) {
		for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
			rec, ok := logs.ParseHAProxyLine(agent.LogLine{At: time.Now(), Stream: "stderr", Text: line})
			if !ok || rec.Source != "haproxy" || rec.Message == "" {
				t.Fatalf("unparsed line %q: %+v", line, rec)
			}
			if strings.HasPrefix(line, "[") && strings.HasPrefix(rec.Message, "(") {
				t.Fatalf("pid prefix left in message: %q", rec.Message)
			}
			for sub, level := range want {
				if strings.Contains(rec.Message, sub) {
					if rec.Level != level {
						t.Errorf("%q parsed as %s, want %s", line, rec.Level, level)
					}
					if level == "info" && rec.Message != strings.TrimSpace(line) {
						t.Errorf("traffic line altered: %q", rec.Message)
					}
					seen[sub] = true
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for sub := range want {
		if !seen[sub] {
			t.Errorf("no line containing %q in:\n%s", sub, stderr.String())
		}
	}
}
