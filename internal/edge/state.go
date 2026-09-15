package edge

import (
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// listenerRole describes the listener a request arrived on.
type listenerRole struct {
	kind    string // http | https | quic
	scheme  string // http | https
	port    int
	portStr string
	quic    bool
}

// reqState carries everything about one request through the pipeline and
// into the access log. It is pooled; nothing may keep it after the handler
// returns.
type reqState struct {
	srv  *Server
	rt   *runtime
	role *listenerRole
	r    *http.Request

	start time.Time
	rw    respWriter
	fw    filterWriter

	vs       *vserver
	host     string // $host
	path     string // $uri: decoded, dot segments resolved, slashes merged
	rawPath  string
	rawQuery string
	hasArgs  bool

	clientIP   netip.Addr
	remoteIP   string
	remotePort string
	requestID  string

	loc         *locationRT
	upstreamURI string // $uri after a location rewrite ("" = unchanged)
	up          upstreamInfo
	clientGone  bool // client closed before the upstream answered (499)

	stripRemote  bool
	remoteUser   string
	remoteGroups string
	remoteEmail  string
	remoteName   string

	aborted    bool // default server "close" (status 444)
	cacheFetch bool // leader fetch for the asset cache (GET, no validators)
	expires    bool // add nginx `expires 30d` headers
}

// upstreamInfo feeds $upstream_* log fields.
type upstreamInfo struct {
	used     bool
	addr     string
	status   int
	start    time.Time
	connect  time.Duration
	header   time.Duration
	response time.Duration

	connected, gotHeader bool
	bodyComplete         bool // upstream body read to EOF (asset cache)
}

var statePool = sync.Pool{New: func() any { return new(reqState) }}

func getState() *reqState { return statePool.Get().(*reqState) }

func putState(rs *reqState) {
	*rs = reqState{}
	statePool.Put(rs)
}

// newRequestID returns 32 lowercase hex characters like nginx $request_id.
func newRequestID() string {
	var b [32]byte
	a, c := rand.Uint64(), rand.Uint64()
	for i := range 16 {
		b[i] = hexDigits[a>>(60-4*i)&0xf]
		b[16+i] = hexDigits[c>>(60-4*i)&0xf]
	}
	return string(b[:])
}

// requestURI is $request_uri: the raw request target.
func (rs *reqState) requestURI() string {
	if u := rs.r.RequestURI; u != "" && u[0] == '/' {
		return u
	}
	if rs.r.URL != nil {
		return rs.r.URL.RequestURI()
	}
	return rs.r.RequestURI
}

// uri is $uri (the rewritten URI inside a rewriting location).
func (rs *reqState) uri() string {
	if rs.upstreamURI != "" {
		return rs.upstreamURI
	}
	return rs.path
}

// remoteUserName is $remote_user: the basic-auth user name as sent, whether
// or not it is valid.
func (rs *reqState) remoteUserName() string {
	u, _, _ := rs.r.BasicAuth()
	return u
}

func (rs *reqState) sslProtocol() string {
	if rs.r.TLS != nil {
		return tlsVersionName(rs.r.TLS.Version)
	}
	if rs.role.quic {
		return "TLSv1.3"
	}
	return ""
}

// splitRemote fills the client address fields from r.RemoteAddr.
func (rs *reqState) splitRemote() {
	ra := rs.r.RemoteAddr
	if ap, err := netip.ParseAddrPort(ra); err == nil {
		rs.clientIP = ap.Addr().Unmap().WithZone("")
		if i := strings.LastIndexByte(ra, ':'); i >= 0 {
			rs.remoteIP, rs.remotePort = strings.Trim(ra[:i], "[]"), ra[i+1:]
		}
		if ap.Addr().Is4In6() {
			rs.remoteIP = rs.clientIP.String()
		}
		return
	}
	host, port, err := net.SplitHostPort(ra)
	if err != nil {
		host = ra
	}
	rs.remoteIP, rs.remotePort = host, port
	rs.clientIP = hostAddr(host)
}

// ---------------------------------------------------------------- writer

// respWriter records the status and body bytes for the access log.
type respWriter struct {
	rw          http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *respWriter) Header() http.Header { return w.rw.Header() }

func (w *respWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.rw.WriteHeader(code) // informational (103 Early Hints)
		return
	}
	w.status, w.wroteHeader = code, true
	w.rw.WriteHeader(code)
}

func (w *respWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.rw.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *respWriter) Flush() {
	if f, ok := w.rw.(http.Flusher); ok {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach Hijack and deadlines.
func (w *respWriter) Unwrap() http.ResponseWriter { return w.rw }

// countingBody counts request body bytes for $request_length.
type countingBody struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }
