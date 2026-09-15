package adminlisten

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newApp(t *testing.T, envPort int) *core.App {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := core.Config{Listen: "127.0.0.1:" + strconv.Itoa(envPort)}
	return core.New(cfg, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func get(port int) error {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func TestSwitchPortKeepsOldPortUntilNewOneIsReached(t *testing.T) {
	p1, p2 := freePort(t), freePort(t)
	app := newApp(t, p1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New(app, okHandler())
	m.grace, m.tick = 50*time.Millisecond, 20*time.Millisecond
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())
	if err := get(p1); err != nil {
		t.Fatalf("initial port: %v", err)
	}

	if err := m.SwitchPort(ctx, p2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // longer than grace: nothing reached p2 yet
	if err := get(p1); err != nil {
		t.Fatalf("old port closed before the new one was reached: %v", err)
	}
	if got := m.Ports(); len(got) != 2 || got[0] != p2 {
		t.Fatalf("ports = %v, want [%d %d]", got, p2, p1)
	}

	if err := get(p2); err != nil {
		t.Fatalf("new port: %v", err)
	}
	waitFor(t, "old port to close", func() bool { return get(p1) != nil })
	if got := m.Ports(); len(got) != 1 || got[0] != p2 {
		t.Fatalf("ports = %v, want [%d]", got, p2)
	}
	if n := m.loadConfirmed(ctx); n != p2 {
		t.Fatalf("confirmed port = %d, want %d", n, p2)
	}
}

func TestStartAlsoServesLastConfirmedPort(t *testing.T) {
	envPort, want := freePort(t), freePort(t)
	app := newApp(t, envPort)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := store.DefaultGeneral()
	g.AdminPort = want
	if err := app.Store.PutSettings(ctx, model.SettingsGeneral, g); err != nil {
		t.Fatal(err)
	}

	m := New(app, okHandler())
	m.tick = time.Hour
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())
	if err := get(want); err != nil {
		t.Fatalf("configured port: %v", err)
	}
	if err := get(envPort); err != nil {
		t.Fatalf("fallback port: %v", err)
	}
}

func TestFirstStartFollowsRelayListenOverDefault(t *testing.T) {
	envPort := freePort(t)
	app := newApp(t, envPort)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New(app, okHandler())
	m.tick = time.Hour
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown(context.Background())
	if got := m.Ports(); len(got) != 1 || got[0] != envPort {
		t.Fatalf("ports = %v, want only RELAY_LISTEN port %d", got, envPort)
	}
	g, err := store.LoadSettings[model.GeneralSettings](ctx, app.Store, model.SettingsGeneral)
	if err != nil || g.AdminPort != envPort {
		t.Fatalf("stored admin port = %d (%v), want %d", g.AdminPort, err, envPort)
	}
}

func TestCheckPortRejectsBusyPort(t *testing.T) {
	p := freePort(t)
	app := newApp(t, p)
	m := New(app, okHandler())
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	m.host = "127.0.0.1"
	port := busy.Addr().(*net.TCPAddr).Port
	if err := m.CheckPort(port); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("CheckPort(busy) = %v", err)
	}
	if err := m.CheckPort(freePort(t)); err != nil {
		t.Fatalf("CheckPort(free) = %v", err)
	}
}
