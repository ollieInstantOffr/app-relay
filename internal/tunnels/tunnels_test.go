package tunnels

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
	"github.com/instantoffr/relay/internal/tunnel"
	"github.com/instantoffr/relay/internal/tunnel/pair"
)

type env struct {
	app *core.App
	svc *Service
	ctx context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	run, err := os.MkdirTemp("/tmp", "rltun")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	app := core.New(core.Config{DataDir: filepath.Join(dir, "data"), RunDir: run}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc := New(app)
	app.Tunnels = svc
	ctx := core.WithActor(context.Background(), core.Actor{Type: core.ActorUser, Name: "admin", Role: core.RoleAdmin})
	return &env{app: app, svc: svc, ctx: ctx}
}

func (e *env) createGateway(t *testing.T, addr string) *model.Gateway {
	t.Helper()
	g := &model.Gateway{Name: "vps", Address: addr, Enabled: true}
	if err := g.KeepSecrets(nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Store.Gateways().Create(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	return g
}

// fakeGateway accepts one pairing connection with tok and returns the home pin.
func fakeGateway(t *testing.T, tok func() pair.Token) (addr string, pins chan string, id pair.Identity) {
	t.Helper()
	id, err := pair.NewIdentity("relay-gateway")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pins = make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := tls.Server(c, pair.ServerConfig(id, func() string { return "" }, pair.ALPN))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				pin, err := pair.Gateway(ctx, tc, tok())
				if err == nil {
					pins <- pin
				}
			}()
		}
	}()
	return ln.Addr().String(), pins, id
}

func TestPairingFlow(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var token pair.Token
	setToken := func(t pair.Token) { mu.Lock(); token = t; mu.Unlock() }
	addr, pins, gwID := fakeGateway(t, func() pair.Token { mu.Lock(); defer mu.Unlock(); return token })
	g := e.createGateway(t, addr)

	// Pairing without a token is refused.
	if _, err := e.svc.Pair(e.ctx, g.ID); err == nil {
		t.Fatal("paired without a token")
	}
	p, err := e.svc.StartPairing(e.ctx, g.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Install, p.Token) || !strings.HasPrefix(p.Token, "rlypair1_") || len(p.Ports) != 4 {
		t.Fatalf("pairing %+v", p)
	}
	tok, err := pair.ParseToken(p.Token)
	if err != nil {
		t.Fatal(err)
	}
	setToken(tok)
	stored, _ := e.app.Store.Gateways().Get(e.ctx, g.ID)
	stored.Redact()
	if stored.PairToken != "" {
		t.Fatal("redacted gateway exposes the token")
	}

	got, err := e.svc.Pair(e.ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PairState != model.GatewayPaired || got.GatewayPin != gwID.Fingerprint() || got.PairToken != "" {
		t.Fatalf("after pairing %+v", got)
	}
	select {
	case pin := <-pins:
		if pin != got.HomeFingerprint {
			t.Fatalf("gateway pinned %s, home is %s", pin, got.HomeFingerprint)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not pair")
	}

	// The engine's gateway list has the pin and key files.
	data, err := os.ReadFile(filepath.Join(e.app.Config.DataDir, "tunnel", tunnel.GatewaysFileName))
	if err != nil {
		t.Fatal(err)
	}
	var list tunnel.Gateways
	json.Unmarshal(data, &list)
	if len(list.Gateways) != 1 || list.Gateways[0].Pin != gwID.Fingerprint() || !fileExists(list.Gateways[0].KeyFile) {
		t.Fatalf("gateways.json %s", data)
	}
	if fi, _ := os.Stat(list.Gateways[0].KeyFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}

	// Re-pairing drops the pin until the gateway pairs again.
	if _, err := e.svc.StartPairing(e.ctx, g.ID, false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(e.app.Config.DataDir, "tunnel", tunnel.GatewaysFileName))
	if strings.Contains(string(data), gwID.Fingerprint()) {
		t.Error("re-pairing kept the old pin in gateways.json")
	}
	// A wrong token is rejected with a clear error.
	setToken(pair.Token{Expires: time.Now().Add(time.Hour)})
	if _, err := e.svc.Pair(e.ctx, g.ID); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("wrong token: %v", err)
	}
}

func TestPairUnreachable(t *testing.T) {
	e := newEnv(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	g := e.createGateway(t, addr)
	if _, err := e.svc.StartPairing(e.ctx, g.ID, false); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Pair(e.ctx, g.ID)
	if err == nil || !strings.Contains(err.Error(), "Can't reach") {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestHooks(t *testing.T) {
	e := newEnv(t)
	httpx.GatewayHooks = httpx.Hooks[model.Gateway]{}
	httpx.HostHooks = httpx.Hooks[model.ProxyHost]{}
	httpx.StreamHooks = httpx.Hooks[model.Stream]{}
	t.Cleanup(func() {
		httpx.GatewayHooks = httpx.Hooks[model.Gateway]{}
		httpx.HostHooks = httpx.Hooks[model.ProxyHost]{}
		httpx.StreamHooks = httpx.Hooks[model.Stream]{}
	})
	e.svc.registerHooks()
	g := e.createGateway(t, "gw.example.com")
	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(e.ctx)
	httpx.GatewayHooks.AfterSave(req, nil, g)
	stored, _ := e.app.Store.Gateways().Get(e.ctx, g.ID)
	if !pair.ValidFingerprint(stored.HomeFingerprint) {
		t.Fatalf("home fingerprint %q", stored.HomeFingerprint)
	}

	host := &model.ProxyHost{Domains: []string{"app.example.com"}, TunnelGatewayID: "nope"}
	if err := httpx.HostHooks.BeforeSave(req, nil, host); err == nil {
		t.Error("unknown gateway accepted")
	}
	host.TunnelGatewayID = g.ID
	if err := httpx.HostHooks.BeforeSave(req, nil, host); err != nil {
		t.Fatal(err)
	}
	e.app.Store.Hosts().Create(e.ctx, host)
	udp := &model.Stream{Name: "dns", Protocol: "udp", TunnelGatewayID: g.ID}
	if err := httpx.StreamHooks.BeforeSave(req, nil, udp); err == nil {
		t.Error("udp stream published")
	}

	if err := httpx.GatewayHooks.BeforeDelete(req, g); err == nil || !strings.Contains(err.Error(), "app.example.com") {
		t.Errorf("delete in use: %v", err)
	}
	host.TunnelGatewayID = ""
	e.app.Store.Hosts().Update(e.ctx, host)
	if err := httpx.GatewayHooks.BeforeDelete(req, g); err != nil {
		t.Fatal(err)
	}
	httpx.GatewayHooks.AfterDelete(req, g)
	if _, err := os.Stat(e.svc.gatewayDir(g.ID)); !os.IsNotExist(err) {
		t.Error("identity kept after delete")
	}
}

func TestTrackAlerts(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.svc.now = func() time.Time { return now }
	g := &model.Gateway{Meta: model.Meta{ID: "g1"}, Name: "vps"}
	activity := func() int {
		rows, _ := e.app.Store.ListActivity(e.ctx, 50)
		n := 0
		for _, r := range rows {
			if strings.HasPrefix(r.Kind, "tunnel.") {
				n++
			}
		}
		return n
	}
	e.svc.track(e.ctx, g, true, true, "")
	e.svc.track(e.ctx, g, true, false, "timeout")
	now = now.Add(30 * time.Second)
	e.svc.track(e.ctx, g, true, false, "timeout")
	if activity() != 0 {
		t.Fatal("alerted before the debounce")
	}
	now = now.Add(31 * time.Second)
	e.svc.track(e.ctx, g, true, false, "timeout")
	e.svc.track(e.ctx, g, true, false, "timeout")
	if activity() != 1 {
		t.Fatalf("down alerts: %d", activity())
	}
	e.svc.track(e.ctx, g, true, true, "")
	if activity() != 2 {
		t.Fatalf("up activity: %d", activity())
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestSetupCheck(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var token pair.Token
	addr, _, _ := fakeGateway(t, func() pair.Token { mu.Lock(); defer mu.Unlock(); return token })

	// Nothing listening yet: the port step waits.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	g := e.createGateway(t, closed)
	if _, err := e.svc.StartPairing(e.ctx, g.ID, false); err != nil {
		t.Fatal(err)
	}
	c, err := e.svc.Check(e.ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Paired || c.Steps[0].Status != StepOK || c.Steps[1].Status != StepWaiting || c.Steps[2].Status != StepWaiting {
		t.Fatalf("closed port: %+v", c.Steps)
	}

	// An unresolvable name fails the first step.
	bad := e.createGateway(t, "does-not-exist.invalid")
	c, _ = e.svc.Check(e.ctx, bad.ID)
	if c.Steps[0].Status != StepFail {
		t.Fatalf("bad name: %+v", c.Steps)
	}

	// A running gateway pairs from the check.
	g2 := e.createGateway(t, addr)
	p, err := e.svc.StartPairing(e.ctx, g2.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Install, "--reset") || !strings.Contains(p.Install, "install-gateway.sh | sudo sh -s -- --token "+p.Token+" --ref main") {
		t.Errorf("install command: %s", p.Install)
	}
	tok, _ := pair.ParseToken(p.Token)
	mu.Lock()
	token = tok
	mu.Unlock()
	c, err = e.svc.Check(e.ctx, g2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Paired || c.Steps[2].Status != StepOK || c.Gateway.PairToken != "" {
		t.Fatalf("pairing check: %+v", c)
	}
	// Re-pairing asks the server to forget the old pairing.
	p, _ = e.svc.StartPairing(e.ctx, g2.ID, false)
	if !strings.Contains(p.Install, "--reset") {
		t.Errorf("re-pair command without --reset: %s", p.Install)
	}
}

func TestPublishTestUnreachable(t *testing.T) {
	e := newEnv(t)
	g := e.createGateway(t, "127.0.0.1:1")
	res, err := e.svc.TestPublished(e.ctx, g.ID, "App.Example.com.")
	if err != nil {
		t.Fatal(err)
	}
	if res.Domain != "app.example.com" || res.Reachable || res.Detail == "" {
		t.Fatalf("result %+v", res)
	}
	if _, err := e.svc.TestPublished(e.ctx, g.ID, "not a domain"); err == nil {
		t.Error("invalid domain accepted")
	}
}
