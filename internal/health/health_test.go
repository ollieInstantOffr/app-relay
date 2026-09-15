package health

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func upstreamOf(t *testing.T, rawURL string) model.Upstream {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	return model.Upstream{Scheme: u.Scheme, Host: host, Port: port}
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestProbeHTTP(t *testing.T) {
	ctx := context.Background()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	st := probeHTTP(ctx, upstreamOf(t, ok.URL), false)
	if st.Status != core.HealthHealthy || st.HTTPStatus != 200 || st.Detail != "200 OK" {
		t.Fatalf("200: %+v", st)
	}
	u := upstreamOf(t, ok.URL)
	u.Path = "/admin"
	if st := probeHTTP(ctx, u, false); st.Status != core.HealthHealthy || st.HTTPStatus != 302 {
		t.Fatalf("redirect must not be followed: %+v", st)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer bad.Close()
	if st := probeHTTP(ctx, upstreamOf(t, bad.URL), false); st.Status != core.HealthDegraded || st.Detail != "502 Bad Gateway" {
		t.Fatalf("502: %+v", st)
	}

	st = probeHTTP(ctx, model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: closedPort(t)}, false)
	if st.Status != core.HealthDown || st.Detail != "connection refused" {
		t.Fatalf("refused: %+v", st)
	}

	if st := probeHTTP(ctx, model.Upstream{Scheme: "http"}, false); st.Status != core.HealthUnknown {
		t.Fatalf("no address: %+v", st)
	}
}

func TestProbeHTTPSVerify(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	u := upstreamOf(t, srv.URL)
	if st := probeHTTP(ctx, u, false); st.Status != core.HealthHealthy {
		t.Fatalf("skip verify: %+v", st)
	}
	if st := probeHTTP(ctx, u, true); st.Status != core.HealthDown || !strings.Contains(st.Detail, "TLS certificate") {
		t.Fatalf("verify: %+v", st)
	}
	u.Scheme = "http"
	if st := probeHTTP(ctx, u, false); st.Status == core.HealthDown {
		t.Fatalf("plain HTTP to a TLS port still gets a (400) response: %+v", st)
	}
}

func TestProbeTCP(t *testing.T) {
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
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if st := probeTCP(context.Background(), "127.0.0.1", port); st.Status != core.HealthHealthy {
		t.Fatalf("open: %+v", st)
	}
	if st := probeTCP(context.Background(), "127.0.0.1", closedPort(t)); st.Status != core.HealthDown || st.Detail != "connection refused" {
		t.Fatalf("closed: %+v", st)
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]int{"25565": 25565, "2456-2458": 2456, "80,443": 80, "": 0, "x": 0, "70000": 0} {
		if got := firstPort(in); got != want {
			t.Errorf("firstPort(%q) = %d want %d", in, got, want)
		}
	}
	fronts := []model.Frontend{
		{Meta: model.Meta{ID: "f0"}, Mode: "http", Bind: "0.0.0.0:80", DefaultBackendID: "b1", Enabled: true},
		{Meta: model.Meta{ID: "f1"}, Mode: "http", Bind: "127.0.0.1:10080", DefaultBackendID: "b1", Enabled: true},
		{Meta: model.Meta{ID: "f2"}, Mode: "tcp", Bind: "127.0.0.1:10081", DefaultBackendID: "b2", Enabled: true},
	}
	u := resolveUpstream(model.Upstream{BackendID: "b1"}, fronts)
	if u.Host != "127.0.0.1" || u.Port != 10080 || u.Scheme != "http" {
		t.Fatalf("resolve: %+v", u)
	}
	if h, p, ok := frontendFor(fronts, "b2", "tcp"); !ok || h != "127.0.0.1" || p != 10081 {
		t.Fatalf("frontendFor tcp: %v %v %v", h, p, ok)
	}
}

func TestRecordTransitions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &core.App{Store: st, Bus: events.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := New(app)
	ctx := context.Background()
	ch, cancel := app.Bus.Subscribe(16)
	defer cancel()

	m := targetMeta{kind: "host", name: "jellyfin.home.lan", addr: "10.0.0.42:8096", url: "/hosts?edit=h1"}
	s.record(ctx, core.HealthStatus{Target: "host:h1", Status: core.HealthHealthy, Detail: "200 OK"}, m)
	first, _ := s.Get("host:h1")
	s.record(ctx, core.HealthStatus{Target: "host:h1", Status: core.HealthHealthy, Detail: "200 OK"}, m)
	second, _ := s.Get("host:h1")
	if !second.ChangedAt.Equal(first.ChangedAt) {
		t.Fatal("ChangedAt must be kept while the status is unchanged")
	}
	s.record(ctx, core.HealthStatus{Target: "host:h1", Status: core.HealthDown, Detail: "connection refused"}, m)
	s.record(ctx, core.HealthStatus{Target: "host:h1", Status: core.HealthHealthy, Detail: "200 OK"}, m)

	acts, err := st.ListActivity(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 2 || acts[1].Kind != "upstream.down" || acts[1].Subject != "10.0.0.42:8096" || acts[1].Detail != "jellyfin.home.lan · 502" ||
		acts[0].Kind != "upstream.up" || acts[0].Level != "ok" {
		t.Fatalf("activity: %+v", acts)
	}
	changes := 0
	for len(ch) > 0 {
		if ev := <-ch; ev.Topic == events.HealthChanged {
			changes++
		}
	}
	if changes != 3 {
		t.Fatalf("health.changed events = %d, want 3", changes)
	}
	recs, _ := st.ListHealthChecks(ctx)
	if len(recs) != 1 || recs[0].Status != core.HealthHealthy {
		t.Fatalf("persisted: %+v", recs)
	}

	// State reloads from the table.
	s2 := New(app)
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	if err := s2.Start(cctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := s2.Get("host:h1"); !ok || got.Status != core.HealthHealthy {
		t.Fatalf("reload: %+v %v", got, ok)
	}
	s2.remove(ctx, "host:h1")
	if _, ok := s2.Get("host:h1"); ok {
		t.Fatal("remove")
	}
}
