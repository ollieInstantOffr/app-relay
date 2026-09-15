package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// drainTimeout bounds graceful shutdown and the draining of removed
// listeners (docs/BALANCER.md §2).
const drainTimeout = 20 * time.Second

// Options configure a Server.
type Options struct {
	// Stderr receives HAProxy-style process messages and traffic logs.
	Stderr io.Writer
	// Version is reported by `show info` ("" = build info).
	Version string
	// Resolve looks up server host names (nil = the system resolver).
	Resolve func(ctx context.Context, host string) (netip.Addr, error)
}

// Server is a running Relay Balancer data plane.
type Server struct {
	opts    Options
	log     *logger
	limiter *limiter
	started time.Time

	rt       atomic.Pointer[runtime]
	stopping atomic.Bool
	reloads  atomic.Int64
	actconn  atomic.Int64
	cumConns atomic.Int64
	cumReq   atomic.Int64

	mu        sync.Mutex // serializes Start, Reload and Shutdown
	listeners map[string]*boundListener
	sock      *runtimeSocket
	draining  sync.WaitGroup
}

func NewServer(opts Options) *Server {
	return &Server{
		opts:      opts,
		log:       newLogger(opts.Stderr),
		limiter:   &limiter{},
		started:   time.Now(),
		listeners: map[string]*boundListener{},
	}
}

// Start compiles cfg, binds every listener and starts serving.
func (s *Server) Start(cfg *spec.Config, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rt.Load() != nil {
		return errors.New("balancer: server already started")
	}
	return s.apply(cfg, hash)
}

// Reload switches to cfg. New listeners are bound before the switch; on any
// error the previous configuration keeps serving.
func (s *Server) Reload(cfg *spec.Config, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rt.Load() == nil || s.stopping.Load() {
		return errors.New("balancer: server is not running")
	}
	if err := s.apply(cfg, hash); err != nil {
		return err
	}
	s.reloads.Add(1)
	return nil
}

// apply does the work of Start and Reload; s.mu must be held.
func (s *Server) apply(cfg *spec.Config, hash string) error {
	prev := s.rt.Load()
	rt, err := compile(cfg, compileEnv{prev: prev, log: s.log, resolve: s.opts.Resolve})
	if err != nil {
		return err
	}
	rt.hash = hash

	want := map[string]listenSpec{}
	var opened []*boundListener
	fail := func(err error) error {
		for _, bl := range opened {
			bl.closeUnserved()
		}
		return err
	}
	for _, ls := range rt.listeners {
		want[ls.key] = ls
		if s.listeners[ls.key] != nil {
			continue
		}
		bl, err := s.open(ls)
		if err != nil {
			what := "proxy stats"
			if ls.fe != nil {
				what = "frontend " + ls.fe.name
			}
			return fail(fmt.Errorf("Starting %s: cannot bind socket (%s) [%s]", what, bindReason(err), ls.addr))
		}
		opened = append(opened, bl)
	}
	var newSock *runtimeSocket
	sockPath := ""
	if s.sock != nil {
		sockPath = s.sock.path
	}
	if cfg.RuntimeSocket != "" && cfg.RuntimeSocket != sockPath {
		if newSock, err = listenRuntime(cfg.RuntimeSocket, s); err != nil {
			return fail(fmt.Errorf("runtime socket %s: %w", cfg.RuntimeSocket, err))
		}
	}

	// Point of no return.
	for _, w := range rt.warnings {
		s.log.warning("%s", w)
	}
	rt.commit(s.log)
	s.limiter.setMax(cfg.MaxConn)
	s.rt.Store(rt)
	for key, bl := range s.listeners {
		if ls, ok := want[key]; ok {
			bl.update(ls)
		}
	}
	for _, bl := range opened {
		bl.update(want[bl.key])
		s.listeners[bl.key] = bl
		bl.serve()
	}
	for key, bl := range s.listeners {
		if _, keep := want[key]; keep {
			continue
		}
		delete(s.listeners, key)
		s.draining.Add(1)
		go func() {
			defer s.draining.Done()
			ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			bl.shutdown(ctx)
		}()
	}
	if newSock != nil || cfg.RuntimeSocket == "" {
		if s.sock != nil {
			s.sock.close()
		}
		s.sock = newSock
		if newSock != nil {
			go newSock.serve()
		}
	}
	if prev != nil {
		prev.stopChecks()
		prev.closePools(rt)
	}
	rt.startChecks()
	s.logStarted(prev, rt)
	return nil
}

func bindReason(err error) string {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		msg := oe.Err.Error()
		if se := strings.TrimPrefix(msg, "bind: "); se != "" {
			msg = se
		}
		if len(msg) > 0 {
			return strings.ToUpper(msg[:1]) + msg[1:]
		}
	}
	return err.Error()
}

// logStarted prints HAProxy's "Proxy <name> started." for new proxies.
func (s *Server) logStarted(prev, rt *runtime) {
	old := map[string]bool{}
	if prev != nil {
		for _, f := range prev.frontends {
			old["f"+f.name] = true
		}
		for _, b := range prev.backends {
			old["b"+b.name] = true
		}
	}
	for _, f := range rt.frontends {
		if !old["f"+f.name] {
			s.log.notice("Proxy %s started.", f.name)
		}
	}
	for _, b := range rt.backends {
		if !old["b"+b.name] {
			s.log.notice("Proxy %s started.", b.name)
		}
	}
}

// Shutdown stops accepting on every listener, lets requests and sessions
// finish until ctx ends, then closes the rest.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping.Store(true)
	var wg sync.WaitGroup
	for key, bl := range s.listeners {
		delete(s.listeners, key)
		wg.Go(func() { bl.shutdown(ctx) })
	}
	wg.Wait()
	done := make(chan struct{})
	go func() {
		s.draining.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if rt := s.rt.Load(); rt != nil {
		rt.stopChecks()
		rt.closePools(nil)
	}
	if s.sock != nil {
		s.sock.close()
		s.sock = nil
	}
	s.log.flush()
	return ctx.Err()
}

// Close releases the logger; call after Shutdown.
func (s *Server) Close() { s.log.close() }

// Hash returns the active configuration hash ("" before Start).
func (s *Server) Hash() string {
	if rt := s.rt.Load(); rt != nil {
		return rt.hash
	}
	return ""
}

// Addrs returns the bound listener addresses by listener key
// ("http|127.0.0.1:8080", "tcp|…", "stats|…").
func (s *Server) Addrs() map[string]net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]net.Addr{}
	keys := make([]string, 0, len(s.listeners))
	for k := range s.listeners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = s.listeners[k].ln.Addr()
	}
	return out
}

// Command runs one runtime API command line (like the unix socket).
func (s *Server) Command(line string) string { return s.execLine(line) }
