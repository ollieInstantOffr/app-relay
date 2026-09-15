package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configure a Server.
type Options struct {
	// Stderr receives the nginx-style error log lines (lifecycle markers
	// included). nil discards them.
	Stderr io.Writer
	// BindHost restricts the HTTP, HTTPS and QUIC listeners to one address
	// ("" = all interfaces). Tests use 127.0.0.1.
	BindHost string
	// CertPollInterval is how often certificate files are checked for
	// changes (0 = 30 s).
	CertPollInterval time.Duration
}

// Server is a running Relay Edge data plane.
type Server struct {
	opts    Options
	errlog  *errorLog
	certs   *certStore
	cache   *assetCache
	metrics *metrics

	table     atomic.Pointer[runtime]
	accessLog atomic.Pointer[logSink]
	streamLog atomic.Pointer[logSink]
	stopping  atomic.Bool

	droppedBefore atomic.Uint64 // access lines dropped by closed sinks

	mu        sync.Mutex // serializes Start, Reload and Shutdown
	logDir    string
	listeners map[string]*boundListener
	draining  sync.WaitGroup

	bgCancel context.CancelFunc
	bgDone   chan struct{}
}

func NewServer(opts Options) (*Server, error) {
	certs, err := newCertStore()
	if err != nil {
		return nil, err
	}
	if opts.CertPollInterval <= 0 {
		opts.CertPollInterval = 30 * time.Second
	}
	return &Server{
		opts:      opts,
		errlog:    newErrorLog(opts.Stderr),
		certs:     certs,
		cache:     newAssetCache(256 << 20),
		metrics:   newMetrics(),
		listeners: map[string]*boundListener{},
	}, nil
}

// Start compiles cfg, binds every listener and starts serving.
func (s *Server) Start(cfg *Config, baseDir, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.table.Load() != nil {
		return errors.New("edge: server already started")
	}
	if err := s.apply(cfg, baseDir, hash); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.bgCancel, s.bgDone = cancel, make(chan struct{})
	go s.background(ctx)
	return nil
}

// Reload switches to cfg. New listeners are bound before the routing table
// is swapped; on any error the previous configuration keeps serving.
func (s *Server) Reload(cfg *Config, baseDir, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.table.Load() == nil || s.stopping.Load() {
		return errors.New("edge: server is not running")
	}
	err := s.apply(cfg, baseDir, hash)
	if err != nil {
		s.metrics.reloadsFailed.Add(1)
	} else {
		s.metrics.reloadsOK.Add(1)
	}
	return err
}

// apply does the work of Start and Reload; s.mu must be held.
func (s *Server) apply(cfg *Config, baseDir, hash string) error {
	prev := s.table.Load()
	rt, err := compile(cfg, baseDir, compileEnv{certs: s.certs, prev: prev, metrics: s.metrics, bindHost: s.opts.BindHost})
	if err != nil {
		return err
	}
	rt.hash = hash
	if prev == nil {
		s.setLogDir(cfg.LogDir) // so bind errors reach error.log too
	}

	want := make(map[string]listenSpec, len(rt.listeners))
	var opened []*boundListener
	for _, spec := range rt.listeners {
		want[spec.key] = spec
		if s.listeners[spec.key] != nil {
			continue
		}
		bl, err := s.open(spec)
		if err != nil {
			for _, o := range opened {
				o.closeUnserved()
			}
			return fmt.Errorf("bind %s (%s): %w", spec.label, spec.addr, err)
		}
		opened = append(opened, bl)
	}

	s.certs.swap(rt.certs)
	s.cache.resize(rt.cacheBytes)
	s.setLogDir(cfg.LogDir)
	s.table.Store(rt)

	for _, bl := range opened {
		s.listeners[bl.spec.key] = bl
		bl.serve()
	}
	for key, bl := range s.listeners {
		spec, keep := want[key]
		if keep {
			if spec.stream != nil {
				bl.stream.Store(spec.stream)
			}
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
	if prev != nil {
		for k, t := range prev.transports {
			if rt.transports[k] != t {
				t.tr.CloseIdleConnections()
			}
		}
	}
	return nil
}

// drainTimeout bounds graceful shutdown (docs/EDGE.md §2).
const drainTimeout = 20 * time.Second

func (s *Server) setLogDir(dir string) {
	if dir == s.logDir && (dir == "" || s.accessLog.Load() != nil) {
		return
	}
	s.logDir = dir
	s.errlog.setDir(dir)
	var access, stream *logSink
	if dir != "" {
		access = newFileSink(filepath.Join(dir, "access.log"))
		stream = newFileSink(filepath.Join(dir, "stream-access.log"))
	}
	oldAccess, oldStream := s.accessLog.Swap(access), s.streamLog.Swap(stream)
	if oldAccess != nil {
		s.droppedBefore.Add(oldAccess.dropped.Load())
	}
	oldAccess.close()
	oldStream.close()
}

// Shutdown stops accepting on every listener and waits for active requests
// and stream sessions until ctx ends, then closes the rest.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping.Store(true)
	var wg sync.WaitGroup
	var status *boundListener
	for key, bl := range s.listeners {
		delete(s.listeners, key)
		if bl.spec.kind == "status" {
			status = bl // answers /healthz with 503 until the end
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			bl.shutdown(ctx)
		}()
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { s.draining.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if status != nil {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		status.shutdown(sctx)
		cancel()
	}
	if s.bgCancel != nil {
		s.bgCancel()
		<-s.bgDone
	}
	if rt := s.table.Load(); rt != nil {
		for _, t := range rt.transports {
			t.tr.CloseIdleConnections()
		}
	}
	s.accessLog.Swap(nil).close()
	s.streamLog.Swap(nil).close()
	s.errlog.flush()
	return ctx.Err()
}

// Close releases the error log; call after Shutdown.
func (s *Server) Close() { s.errlog.close() }

// Hash returns the active configuration hash ("" before Start).
func (s *Server) Hash() string {
	if rt := s.table.Load(); rt != nil {
		return rt.hash
	}
	return ""
}

// Addrs returns the bound listener addresses by listener key (for tests and
// diagnostics), sorted by key.
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
		bl := s.listeners[k]
		if bl.ln != nil {
			out[k] = bl.ln.Addr()
		} else if bl.pc != nil {
			out[k] = bl.pc.LocalAddr()
		}
	}
	return out
}

// background polls certificate files and refreshes OCSP staples.
func (s *Server) background(ctx context.Context) {
	defer close(s.bgDone)
	client := &http.Client{Timeout: 20 * time.Second}
	staple := func() {
		now := time.Now()
		for _, e := range s.certs.snapshot() {
			if err := e.staple(ctx, client, now); err != nil && !errors.Is(err, errNoOCSP) && ctx.Err() == nil {
				s.errlog.logf(levelWarn, "OCSP stapling for certificate %s skipped: %v", e.ref.ID, err)
			}
		}
	}
	staple()
	certTick := time.NewTicker(s.opts.CertPollInterval)
	defer certTick.Stop()
	ocspTick := time.NewTicker(time.Minute)
	defer ocspTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-certTick.C:
			reloaded, errs := s.certs.refresh()
			for _, err := range errs {
				s.errlog.logf(levelError, "certificate reload failed, keeping the loaded one: %v", err)
			}
			if len(reloaded) > 0 {
				s.metrics.certReloads.Add(uint64(len(reloaded)))
				s.errlog.logf(levelNotice, "certificates reloaded: %s", strings.Join(reloaded, ", "))
				staple()
			}
		case <-ocspTick.C:
			staple()
		}
	}
}
