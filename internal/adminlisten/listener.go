// Package adminlisten serves Relay's admin UI/API on the port from
// Settings → General and moves it to another port at runtime, without a
// restart.
//
// Moving is lockout-safe: the new port is opened next to the old one, and the
// old port is only closed once the new one has served a request (so it is
// reachable) and the admin UI proxy host, if any, no longer has a pending
// change pointing nginx at the old port. The last port that served requests
// is remembered, so after a restart Relay also listens there until the
// configured port has been reached.
package adminlisten

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	kvConfirmed = "admin.listen.confirmed_port"
	defaultPort = 8181
	addrFile    = "admin-addr" // in RELAY_RUN_DIR, read by `relay healthcheck`
)

type server struct {
	port  int
	srv   *http.Server
	hitAt atomic.Int64 // unix nanos of the first request, 0 = none yet
}

// Manager owns the admin HTTP listeners.
type Manager struct {
	app     *core.App
	handler http.Handler
	log     *slog.Logger
	host    string // bind host from RELAY_LISTEN ("" = all interfaces)
	envPort int

	// grace is how long the old port stays open after the new one served its
	// first request; tick is how often pending closes are re-checked.
	grace time.Duration
	tick  time.Duration

	mu        sync.Mutex
	servers   map[int]*server
	current   int
	confirmed int
}

func New(app *core.App, handler http.Handler) *Manager {
	host, port := splitListen(app.Config.Listen)
	return &Manager{
		app: app, handler: handler, log: app.Log, host: host, envPort: port,
		grace: 30 * time.Second, tick: 5 * time.Second,
		servers: map[int]*server{},
	}
}

// Start opens the configured admin port (and the last confirmed port while the
// configured one hasn't been reached yet) and keeps reconciling until ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	want := m.envPort
	g, gerr := store.LoadSettings[model.GeneralSettings](ctx, m.app.Store, model.SettingsGeneral)
	if gerr == nil && g.AdminPort > 0 {
		want = g.AdminPort
	}
	confirmed := m.loadConfirmed(ctx)
	if confirmed == 0 {
		// First start with runtime port switching: the port Relay has been
		// reachable on so far is the one from RELAY_LISTEN.
		confirmed = m.envPort
		m.saveConfirmed(ctx, confirmed)
		// A default (never effective) setting follows RELAY_LISTEN; a port the
		// user picked is kept and opened next to it.
		if gerr == nil && (g.AdminPort <= 0 || g.AdminPort == defaultPort) && g.AdminPort != m.envPort {
			g.AdminPort = m.envPort
			if err := m.app.Store.PutSettings(ctx, model.SettingsGeneral, g); err != nil {
				m.log.Warn("admin UI: store port", "err", err)
			}
			want = m.envPort
		}
	}

	m.mu.Lock()
	m.confirmed = confirmed
	wantErr := m.listen(want)
	if wantErr == nil {
		m.current = want
	}
	if confirmed != want {
		if err := m.listen(confirmed); err == nil && wantErr != nil {
			m.current = confirmed
		}
	}
	open := len(m.servers)
	m.mu.Unlock()

	if open == 0 {
		return fmt.Errorf("admin UI: can't listen on port %d: %w", want, wantErr)
	}
	m.writeAddr()
	if wantErr != nil {
		m.log.Warn("admin UI: configured port unavailable, using fallback", "port", want, "fallback", m.current, "err", wantErr)
		m.app.Activity(ctx, "admin.listen", "warn", fmt.Sprintf("Admin UI couldn't listen on port %d", want), strconv.Itoa(want),
			fmt.Sprintf("%s · still serving on port %d", portError(want, wantErr), m.current))
	}
	go m.loop(ctx)
	return nil
}

// Ports returns the open admin ports, the current (configured) one first.
func (m *Manager) Ports() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, 0, len(m.servers))
	for p := range m.servers {
		if p != m.current {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	if _, ok := m.servers[m.current]; ok {
		out = append([]int{m.current}, out...)
	}
	return out
}

// CheckPort reports whether Relay could move the admin UI to port.
func (m *Manager) CheckPort(port int) error {
	m.mu.Lock()
	_, open := m.servers[port]
	m.mu.Unlock()
	if open {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(m.host, strconv.Itoa(port)))
	if err != nil {
		return errors.New(portError(port, err))
	}
	return ln.Close()
}

// SwitchPort starts serving on port and makes it the current admin port. The
// previous port keeps serving until the new one is confirmed reachable.
func (m *Manager) SwitchPort(ctx context.Context, port int) error {
	m.mu.Lock()
	if port == m.current {
		m.mu.Unlock()
		return nil
	}
	if _, ok := m.servers[port]; !ok {
		if err := m.listen(port); err != nil {
			m.mu.Unlock()
			return errors.New(portError(port, err))
		}
	}
	prev := m.current
	m.current = port
	m.writeAddrLocked()
	// Ports that never served a request (an earlier unreachable attempt) can
	// go right away; ports that did are kept until the new one works.
	var drop []*server
	for p, s := range m.servers {
		if p != port && s.hitAt.Load() == 0 {
			drop = append(drop, s)
			delete(m.servers, p)
		}
	}
	m.mu.Unlock()
	for _, s := range drop {
		m.close(s)
	}
	m.log.Info("admin UI: switching port", "from", prev, "to", port)
	m.app.Activity(ctx, "admin.listen", "info", fmt.Sprintf("Admin UI now listening on port %d", port), strconv.Itoa(port),
		fmt.Sprintf("Port %d stays open until port %d has been reached", prev, port))
	go m.reconcile(context.WithoutCancel(ctx))
	return nil
}

// Shutdown gracefully stops every listener.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	servers := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	m.servers = map[int]*server{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			if err := s.srv.Shutdown(ctx); err != nil {
				s.srv.Close()
			}
		}(s)
	}
	wg.Wait()
}

// listen opens port; m.mu must be held.
func (m *Manager) listen(port int) error {
	if _, ok := m.servers[port]; ok {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(m.host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	s := &server{port: port}
	s.srv = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.hitAt.Load() == 0 && s.hitAt.CompareAndSwap(0, time.Now().UnixNano()) {
				go m.reconcile(context.Background())
			}
			m.handler.ServeHTTP(w, r)
		}),
	}
	m.servers[port] = s
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("admin UI listener stopped", "port", port, "err", err)
		}
	}()
	m.log.Info("admin UI listening", "addr", ln.Addr().String())
	return nil
}

func (m *Manager) loop(ctx context.Context) {
	t := time.NewTicker(m.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcile(ctx)
		}
	}
}

// reconcile records the current port as confirmed once it served a request and
// closes the other ports after the grace period.
func (m *Manager) reconcile(ctx context.Context) {
	m.mu.Lock()
	cur := m.servers[m.current]
	if cur == nil || cur.hitAt.Load() == 0 {
		m.mu.Unlock()
		return
	}
	current, hitAt := m.current, time.Unix(0, cur.hitAt.Load())
	newlyConfirmed := m.confirmed != current
	m.confirmed = current
	stale := len(m.servers) - 1
	m.mu.Unlock()

	if newlyConfirmed {
		m.saveConfirmed(ctx, current)
	}
	if stale == 0 || time.Since(hitAt) < m.grace || m.adminHostPending(ctx) {
		return
	}

	m.mu.Lock()
	if m.current != current {
		m.mu.Unlock()
		return
	}
	var closing []*server
	for p, s := range m.servers {
		if p != current {
			closing = append(closing, s)
			delete(m.servers, p)
		}
	}
	m.mu.Unlock()
	for _, s := range closing {
		m.close(s)
		m.log.Info("admin UI: closed old port", "port", s.port, "current", current)
		m.app.Activity(ctx, "admin.listen", "ok", fmt.Sprintf("Admin UI moved to port %d", current), strconv.Itoa(current),
			fmt.Sprintf("Stopped listening on port %d", s.port))
	}
}

// adminHostPending reports whether the admin UI proxy host has an unapplied
// change, i.e. nginx may still send the admin domain to the old port.
func (m *Manager) adminHostPending(ctx context.Context) bool {
	if m.app.Engine == nil {
		return false
	}
	hosts, err := m.app.Store.Hosts().List(ctx)
	if err != nil {
		return true
	}
	id := ""
	for _, h := range hosts {
		if h.System {
			id = h.ID
		}
	}
	if id == "" {
		return false
	}
	p, err := m.app.Engine.Pending(ctx)
	if err != nil || p == nil {
		return err != nil
	}
	for _, it := range p.Items {
		if it.Kind == model.KindHost && it.ID == id {
			return true
		}
	}
	return false
}

func (m *Manager) close(s *server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(ctx); err != nil {
		s.srv.Close() // long-lived event streams
	}
}

func (m *Manager) loadConfirmed(ctx context.Context) int {
	v, err := m.app.Store.GetKV(ctx, kvConfirmed)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(string(v))
	return n
}

func (m *Manager) saveConfirmed(ctx context.Context, port int) {
	if err := m.app.Store.PutKV(ctx, kvConfirmed, []byte(strconv.Itoa(port))); err != nil {
		m.log.Warn("admin UI: remember port", "err", err)
	}
}

func (m *Manager) writeAddr() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeAddrLocked()
}

func (m *Manager) writeAddrLocked() {
	if m.app.Config.RunDir == "" || m.current == 0 {
		return
	}
	addr := loopbackAddr(m.host, m.current)
	if err := os.WriteFile(filepath.Join(m.app.Config.RunDir, addrFile), []byte(addr+"\n"), 0o644); err != nil {
		m.log.Warn("admin UI: record address", "err", err)
	}
}

// ReadAddr returns the admin address a running server recorded in runDir, or
// the loopback address for RELAY_LISTEN.
func ReadAddr(runDir, listen string) string {
	if b, err := os.ReadFile(filepath.Join(runDir, addrFile)); err == nil {
		if a := strings.TrimSpace(string(b)); a != "" {
			return a
		}
	}
	host, port := splitListen(listen)
	return loopbackAddr(host, port)
}

func loopbackAddr(host string, port int) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func splitListen(listen string) (string, int) {
	host, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", defaultPort
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return host, defaultPort
	}
	return host, port
}

func portError(port int, err error) string {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return fmt.Sprintf("Port %d is already in use by another program", port)
	case errors.Is(err, syscall.EACCES):
		return fmt.Sprintf("Not allowed to listen on port %d", port)
	}
	return fmt.Sprintf("Can't listen on port %d: %v", port, err)
}
