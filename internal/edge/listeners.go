package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// HTTP server limits (nginx: client_header_timeout 60s, keepalive_timeout 65s,
// large_client_header_buffers).
const (
	readHeaderTimeout = 60 * time.Second
	keepAliveTimeout  = 65 * time.Second
	maxHeaderBytes    = 64 << 10
)

// boundListener is an open socket plus whatever serves it. Reloads keep it
// while its listenSpec key is unchanged, so no connection is dropped.
type boundListener struct {
	spec   listenSpec
	ln     net.Listener
	pc     net.PacketConn
	http   *http.Server
	tls    bool
	h3     *http3.Server
	tcp    *tcpStream
	udp    *udpStream
	stream atomic.Pointer[streamRT]
	done   chan struct{}
}

// open binds spec without serving yet.
func (s *Server) open(spec listenSpec) (*boundListener, error) {
	bl := &boundListener{spec: spec, done: make(chan struct{})}
	switch spec.network {
	case "unix":
		ln, err := listenUnix(spec.addr)
		if err != nil {
			return nil, err
		}
		bl.ln = newProxyListener(ln, func(format string, args ...any) {
			if s.errlog.enabled(levelInfo) {
				s.errlog.logf(levelInfo, format, args...)
			}
		})
	case "tcp":
		ln, err := net.Listen("tcp", spec.addr)
		if err != nil {
			return nil, err
		}
		bl.ln = ln
	case "udp":
		pc, err := net.ListenPacket("udp", spec.addr)
		if err != nil {
			return nil, err
		}
		bl.pc = pc
	}
	// Tunnel listeners report the gateway's public port (80/443).
	role := &listenerRole{kind: spec.kind, scheme: "https", port: spec.port, portStr: strconv.Itoa(spec.port), tunnel: spec.network == "unix"}
	errLog := log.New(stdlibWriter{s.errlog}, "", 0)
	switch spec.kind {
	case "http", "https":
		if spec.kind == "http" {
			role.scheme = "http"
		}
		bl.http = &http.Server{
			Handler:           &listenerHandler{srv: s, role: role},
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       keepAliveTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
			ErrorLog:          errLog,
			ConnState:         s.metrics.connState,
		}
		if role.tunnel {
			bl.http.ConnContext = tunnelConnContext
		}
		if spec.kind == "https" {
			bl.tls = true
			bl.http.TLSConfig = s.baseTLS(role.tunnel)
			bl.http.Protocols = new(http.Protocols)
			bl.http.Protocols.SetHTTP1(true)
			bl.http.Protocols.SetHTTP2(true)
		}
	case "quic":
		role.quic = true
		bl.h3 = &http3.Server{
			Handler:        &listenerHandler{srv: s, role: role},
			TLSConfig:      s.baseTLS(false),
			MaxHeaderBytes: maxHeaderBytes,
			IdleTimeout:    keepAliveTimeout,
		}
	case "status":
		bl.http = &http.Server{Handler: s.statusHandler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: errLog}
	case "stream":
		bl.stream.Store(spec.stream)
		onDone := s.logStream
		if bl.ln != nil {
			bl.tcp = newTCPStream(bl.ln, spec.port, &bl.stream, onDone)
		} else {
			bl.udp = newUDPStream(bl.pc, &bl.stream, onDone)
		}
	}
	return bl, nil
}

func (bl *boundListener) serve() {
	go func() {
		defer close(bl.done)
		switch {
		case bl.http != nil && bl.tls:
			bl.http.ServeTLS(trackedListener{bl.ln}, "", "")
		case bl.http != nil:
			bl.http.Serve(trackedListener{bl.ln})
		case bl.h3 != nil:
			bl.h3.Serve(bl.pc)
		case bl.tcp != nil:
			bl.tcp.serve()
		case bl.udp != nil:
			bl.udp.serve()
		}
	}()
}

// closeUnserved releases a listener that was opened but never served.
func (bl *boundListener) closeUnserved() {
	if bl.ln != nil {
		bl.ln.Close()
	}
	if bl.pc != nil {
		bl.pc.Close()
	}
}

// shutdown stops accepting, lets active requests and sessions finish until
// ctx ends, then closes what is left.
func (bl *boundListener) shutdown(ctx context.Context) {
	switch {
	case bl.http != nil:
		if err := bl.http.Shutdown(ctx); err != nil {
			bl.http.Close()
		}
	case bl.h3 != nil:
		bl.h3.Shutdown(ctx)
		bl.pc.Close()
	case bl.tcp != nil:
		bl.tcp.shutdown(ctx)
	case bl.udp != nil:
		bl.udp.shutdown()
	}
	select {
	case <-bl.done:
	case <-time.After(time.Second):
	}
}

// baseTLS is the listener config; the per-server config comes from
// GetConfigForClient.
func (s *Server) baseTLS(tunnel bool) *tls.Config {
	get := s.getConfigForClient
	if tunnel {
		get = s.getTunnelConfigForClient
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS10,
		NextProtos:         alpnH2,
		GetConfigForClient: get,
	}
}

var errUnpublishedName = errors.New("tls: server name is not published through a tunnel")

// getTunnelConfigForClient is getConfigForClient for the tunnel ingress:
// names that are not published fail the handshake.
func (s *Server) getTunnelConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	rt := s.table.Load()
	if rt == nil {
		return nil, errors.New("not serving")
	}
	if c := rt.tunnelHTTPS.lookup(hostKey(toLowerASCII(hello.ServerName))).tlsConf; c != nil {
		return c, nil
	}
	return nil, errUnpublishedName
}

// getConfigForClient selects the HTTPS server for the SNI name exactly like
// requests are routed (exact, longest wildcard, default server). Unknown
// names get the default server's certificate or the placeholder.
func (s *Server) getConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	rt := s.table.Load()
	if rt == nil {
		return nil, errors.New("not serving")
	}
	return rt.https.lookup(hostKey(toLowerASCII(hello.ServerName))).tlsConf, nil
}
