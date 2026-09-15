package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// connectTimeout is nginx proxy_connect_timeout (60 s); it also bounds the
// upstream TLS handshake.
const connectTimeout = 60 * time.Second

// transportKey groups upstreams sharing TLS verification and idle timeouts.
// Transports survive reloads, so keep-alive pools do too.
type transportKey struct {
	verify     bool
	read, send time.Duration
}

type upstreamTransport struct {
	key transportKey
	tr  *http.Transport
	rp  *httputil.ReverseProxy
}

var proxyBuffers = bufferPool{}

type bufferPool struct{}

func (bufferPool) Get() []byte  { return *copyBufPool.Get().(*[]byte) }
func (bufferPool) Put(b []byte) { copyBufPool.Put(&b) }

func newUpstreamTransport(key transportKey) *upstreamTransport {
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return newIdleConn(c, key.read, key.send), nil
		},
		// nginx proxy_ssl_protocols defaults to TLSv1.2 TLSv1.3; SNI is the
		// upstream host (proxy_ssl_server_name on).
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !key.verify, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: key.read,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       key.read,
		DisableCompression:    true,
		// Upstreams speak HTTP/1.1 (proxy_http_version 1.1), which websocket
		// upgrades need.
		TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
		WriteBufferSize: 16 << 10,
		ReadBufferSize:  16 << 10,
	}
	t := &upstreamTransport{key: key, tr: tr}
	t.rp = &httputil.ReverseProxy{
		Rewrite:        rewriteUpstream,
		Transport:      tr,
		BufferPool:     proxyBuffers,
		ErrorLog:       log.New(io.Discard, "", 0),
		ErrorHandler:   proxyError,
		ModifyResponse: modifyResponse,
	}
	return t
}

type stateKey struct{}

func stateFrom(ctx context.Context) *reqState {
	rs, _ := ctx.Value(stateKey{}).(*reqState)
	return rs
}

// proxy forwards the request to the location's upstream, through the asset
// cache when the location caches and the path is a static asset.
func (s *Server) proxy(rs *reqState, loc *locationRT) {
	r := rs.r
	ctx := context.WithValue(r.Context(), stateKey{}, rs)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// Both hooks run on the RoundTrip goroutine (unlike the dial and
		// read-loop hooks), so they never touch rs after the handler returns.
		GetConn: func(string) { rs.up.start = time.Now() },
		GotConn: func(info httptrace.GotConnInfo) {
			rs.up.addr = info.Conn.RemoteAddr().String()
			if !info.Reused {
				rs.up.connect = time.Since(rs.up.start)
			}
			rs.up.connected = true
		},
	})
	req := r.WithContext(ctx)
	rs.up.used = true
	rs.up.start = time.Now()
	if loc.cache && isStaticAsset(rs.path) {
		rs.expires = true
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.cache.serve(rs, req, loc)
			return
		}
	}
	loc.up.transport.rp.ServeHTTP(&rs.fw, req)
	rs.up.response = time.Since(rs.up.start)
}

var staticExts = map[string]bool{
	"css": true, "js": true, "mjs": true, "map": true, "png": true, "jpg": true, "jpeg": true, "gif": true,
	"ico": true, "svg": true, "webp": true, "avif": true, "bmp": true, "woff": true, "woff2": true,
	"ttf": true, "otf": true, "eot": true, "mp4": true, "webm": true, "ogg": true, "mp3": true, "wav": true, "pdf": true,
}

// isStaticAsset implements the cache location regex
// (?i)\.(css|js|…|pdf)$ without a regex.
func isStaticAsset(path string) bool {
	i := strings.LastIndexByte(path, '.')
	if i < 0 || len(path)-i-1 > 5 || len(path)-i-1 == 0 {
		return false
	}
	var buf [5]byte
	ext := buf[:0]
	for _, c := range []byte(path[i+1:]) {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		ext = append(ext, c)
	}
	return staticExts[string(ext)]
}

func rewriteUpstream(pr *httputil.ProxyRequest) {
	rs := stateFrom(pr.In.Context())
	loc := rs.loc
	out := pr.Out
	u := out.URL
	u.Scheme, u.Host, u.User, u.Fragment = loc.up.scheme, loc.up.hostPort, nil, ""
	u.RawQuery, u.ForceQuery = rs.rawQuery, false
	target, rewritten := loc.rewrite(rs.path)
	if rewritten {
		rs.upstreamURI = target
		target = escapeURI(target)
	} else {
		target = rs.rawPath // the raw request URI goes upstream unchanged
	}
	if strings.HasPrefix(target, "//") {
		// Opaque would be read as a network path; fall back to Path/RawPath.
		u.Opaque, u.Path, u.RawPath = "", target, target
	} else {
		u.Opaque, u.Path, u.RawPath = target, "", ""
	}

	h := out.Header
	for k := range h {
		if strings.IndexByte(k, '_') >= 0 { // underscores_in_headers off
			delete(h, k)
		}
	}
	out.Host = rs.host
	if out.Host == "" {
		out.Host = loc.up.hostPort
	}
	h["X-Real-Ip"] = []string{rs.remoteIP}
	h["X-Forwarded-For"] = []string{forwardedFor(pr.In.Header, rs.remoteIP)}
	h["X-Forwarded-Proto"] = []string{rs.role.scheme}
	h["X-Forwarded-Host"] = []string{rs.host}
	h["X-Forwarded-Port"] = []string{rs.role.portStr}
	h["X-Request-Id"] = []string{rs.requestID}
	if !loc.websockets {
		delete(h, "Upgrade")
		delete(h, "Connection")
	}
	if rs.stripRemote {
		delete(h, "Remote-User")
		delete(h, "Remote-Groups")
		if rs.remoteUser != "" {
			h["Remote-User"] = []string{rs.remoteUser}
		}
		if rs.remoteGroups != "" {
			h["Remote-Groups"] = []string{rs.remoteGroups}
		}
	}
	for i := range loc.headers {
		hd := &loc.headers[i]
		v := hd.tmpl.expand(rs)
		switch {
		case hd.host && v != "":
			out.Host = v
		case v == "":
			delete(h, hd.name)
		default:
			h[hd.name] = []string{v}
		}
	}
	if rs.cacheFetch {
		// proxy_cache fetches full, unconditional GET responses.
		out.Method = http.MethodGet
		for _, k := range [...]string{"If-Modified-Since", "If-Unmodified-Since", "If-None-Match", "If-Match", "Range", "If-Range"} {
			delete(h, k)
		}
	}
}

func modifyResponse(res *http.Response) error {
	rs := stateFrom(res.Request.Context())
	if rs == nil {
		return nil
	}
	rs.up.status = res.StatusCode
	rs.up.header = time.Since(rs.up.start)
	rs.up.gotHeader = true
	if rs.cacheFetch {
		res.Body = &eofBody{ReadCloser: res.Body, rs: rs}
	}
	return nil
}

// eofBody records that an upstream body was read to the end, so truncated
// responses are never cached.
type eofBody struct {
	io.ReadCloser
	rs *reqState
}

func (b *eofBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.rs.up.bodyComplete = true
	}
	return n, err
}

func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	rs := stateFrom(r.Context())
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writePage(w, http.StatusRequestEntityTooLarge)
		return
	}
	if rs == nil {
		writePage(w, http.StatusBadGateway)
		return
	}
	if r.Context().Err() != nil && !isTimeout(err) {
		rs.clientGone = true // nginx 499
		return
	}
	code := http.StatusBadGateway
	if isTimeout(err) {
		code = http.StatusGatewayTimeout
	}
	if !rs.up.gotHeader {
		rs.up.status = code
	}
	if rs.up.addr == "" {
		rs.up.addr = rs.loc.up.hostPort
	}
	m := rs.vs.metrics
	var op *net.OpError
	switch {
	case code == http.StatusGatewayTimeout:
		m.upTimeout.Add(1)
	case errors.As(err, &op) && op.Op == "dial":
		m.upConnect.Add(1)
	default:
		m.upOther.Add(1)
	}
	if e := rs.srv.errlog; e.enabled(levelError) {
		e.logf(levelError, "upstream %s://%s: %v, client: %s, server: %s, request: \"%s %s %s\", host: %q",
			rs.loc.up.scheme, rs.loc.up.hostPort, err, rs.remoteIP, rs.vs.name, r.Method, rs.requestURI(), rs.r.Proto, rs.host)
	}
	writePage(w, code)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// idleConn enforces nginx proxy_read_timeout / proxy_send_timeout: the time
// between two successive reads or writes, including on upgraded websocket
// connections. Deadlines are refreshed at most every grain to keep the
// per-read cost low.
type idleConn struct {
	net.Conn
	read, send         time.Duration
	readGrain, sendGra time.Duration
	readSet, writeSet  atomic.Int64
}

func newIdleConn(c net.Conn, read, send time.Duration) *idleConn {
	grain := func(d time.Duration) time.Duration { return min(d/10, time.Second) }
	return &idleConn{Conn: c, read: read, send: send, readGrain: grain(read), sendGra: grain(send)}
}

func (c *idleConn) Read(p []byte) (int, error) {
	if c.read > 0 {
		now := time.Now()
		if n := now.UnixNano(); n-c.readSet.Load() >= int64(c.readGrain) {
			c.Conn.SetReadDeadline(now.Add(c.read))
			c.readSet.Store(n)
		}
	}
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	if c.send > 0 {
		now := time.Now()
		if n := now.UnixNano(); n-c.writeSet.Load() >= int64(c.sendGra) {
			c.Conn.SetWriteDeadline(now.Add(c.send))
			c.writeSet.Store(n)
		}
	}
	return c.Conn.Write(p)
}
