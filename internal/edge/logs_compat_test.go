package edge_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/edge"
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

func port(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestLogsParseEdgeOutput feeds real Relay Edge log output to the ingester's
// parsers (internal/logs), which must accept it unchanged.
func TestLogsParseEdgeOutput(t *testing.T) {
	dir := t.TempDir()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	upHost, upPort, _ := net.SplitHostPort(up.Listener.Addr().String())
	upPortN, _ := strconv.Atoi(upPort)
	httpPort, streamPort, deadPort := port(t), port(t), port(t)
	cfg := &edge.Config{
		Schema: 1, HTTPPort: httpPort, LogDir: filepath.Join(dir, "logs"), Default: edge.DefaultServer{Action: "404"},
		Hosts: []edge.Host{
			{ID: "h", Domains: []string{"h.test"}, Locations: []edge.Location{{Path: "/", Kind: "proxy", Upstream: edge.Upstream{Scheme: "http", Host: upHost, Port: upPortN}}}},
			{ID: "dead", Domains: []string{"dead.test"}, Locations: []edge.Location{{Path: "/", Kind: "proxy", Upstream: edge.Upstream{Scheme: "http", Host: "127.0.0.1", Port: deadPort}}}},
		},
		Streams: []edge.Stream{{ID: "s", TCP: true, ListenAddr: "127.0.0.1", ListenLo: streamPort, ListenHi: streamPort, ForwardHost: "127.0.0.1", ForwardLo: deadPort, ForwardHi: deadPort}},
	}
	stderr := &lockedBuffer{}
	srv, err := edge.NewServer(edge.Options{Stderr: stderr, BindHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(cfg, dir, "rel"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		srv.Close()
	}()

	for _, host := range []string{"h.test", "dead.test"} {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(httpPort)+"/p?x=1", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(streamPort)); err == nil {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.Read(make([]byte, 1))
		c.Close()
	}

	readLines := func(name string, n int) []string {
		var lines []string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(filepath.Join(cfg.LogDir, name))
			lines = strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= n && lines[0] != "" {
				return lines
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: got %q", name, lines)
		return nil
	}
	access := readLines("access.log", 2)
	rec, err := logs.ParseAccessLine([]byte(access[0]))
	if err != nil {
		t.Fatal(err)
	}
	if rec.HostID != "h" || rec.Status != 200 || rec.Path != "/p?x=1" || rec.Host != "h.test" || rec.UpstreamStatus != "200" ||
		rec.UpstreamResponseTime == nil || rec.ClientIP != "127.0.0.1" || rec.BytesSent != 2 || time.Since(rec.TS) > time.Minute {
		t.Errorf("access record: %+v", rec)
	}
	rec, err = logs.ParseAccessLine([]byte(access[1]))
	if err != nil || rec.Status != 502 || rec.UpstreamStatus != "502" || rec.UpstreamHeaderTime != nil {
		t.Errorf("502 record: %+v %v", rec, err)
	}
	stream := readLines("stream-access.log", 1)
	srec, err := logs.ParseStreamLine([]byte(stream[0]))
	if err != nil || srec.HostID != "s" || srec.Status != 502 || srec.Protocol != "TCP" || srec.Extra["server_port"] != strconv.Itoa(streamPort) {
		t.Errorf("stream record: %+v %v", srec, err)
	}
	var sawError bool
	for _, l := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		er := logs.ParseNginxErrorLine(l, time.Time{})
		if er.TS.IsZero() {
			t.Errorf("error line without timestamp: %q", l)
		}
		if er.Level == "error" && strings.Contains(er.Message, "upstream") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no parsed upstream error in %q", stderr.String())
	}
}
