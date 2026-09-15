package balancer

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"golang.org/x/net/http/httpguts"
)

// compressTypes are the content types Relay renders into
// `compression type` (matched as prefixes of Content-Type, like HAProxy).
var compressTypes = []string{"text/html", "text/plain", "text/css", "application/json", "application/javascript"}

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// httpTxn is one HTTP request/response exchange.
type httpTxn struct {
	session
	fc         *feConn
	status     int
	bytesOut   int64
	tr         time.Duration
	term       [4]byte
	abort      bool // abort the client connection after logging
	cliAbrt    bool
	srvAbrt    bool
	compressed bool
}

type httpHandler struct{ bl *boundListener }

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fe := h.bl.fe.Load()
	s := h.bl.srv
	now := time.Now()
	t := &httpTxn{status: -1, tr: -1, term: [4]byte{'-', '-', '-', '-'}}
	t.start, t.fe, t.tw, t.tc = now, fe, -1, -1
	if fc, ok := r.Context().Value(connCtxKey{}).(*feConn); ok {
		t.fc, t.client, t.dst = fc, fc.src, fc.dst
	} else if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		t.client = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	fe.st.c.request(now)
	s.cumReq.Add(1)
	if r.Body != nil && r.Body != http.NoBody {
		cb := &countingBody{rc: &idleBody{rc: r.Body, ctl: http.NewResponseController(w), timeout: fe.clientTimeout}}
		r.Body = cb
		defer func() { fe.st.c.bin.Add(cb.n.Load()) }()
	}

	be := fe.routeHTTP(r, t.client.Addr())
	if be == nil {
		t.term[0], t.term[1] = 'S', 'C'
		h.writeError(w, t, http.StatusServiceUnavailable)
		h.logRequest(r, t)
		return
	}
	t.be = be
	be.st.c.sessionStart(now)
	be.st.c.request(now)
	h.proxy(w, r, t)
	total := time.Since(t.start)
	be.st.c.timers(t.tw, t.tc, t.tr, total)
	if t.srv != nil {
		t.srv.st.c.timers(t.tw, t.tc, t.tr, total)
	}
	h.logRequest(r, t)
	t.release()
	be.st.c.sessionEnd()
	if t.abort {
		panic(http.ErrAbortHandler)
	}
}

// countingBody counts request body bytes.
type countingBody struct {
	rc     io.ReadCloser
	n      atomic.Int64
	failed atomic.Bool // a read failed (not EOF): the client side broke
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n.Add(int64(n))
	if err != nil && err != io.EOF {
		b.failed.Store(true)
	}
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }

// idleBody applies the client timeout to request body reads, like HAProxy's
// "timeout client": the client must keep sending within the timeout. Once the
// body is done the deadline is cleared: waiting for the server's response is
// governed by the server timeout.
type idleBody struct {
	rc       io.ReadCloser
	ctl      *http.ResponseController
	timeout  time.Duration
	last     time.Time
	timedOut atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.timeout > 0 {
		if now := time.Now(); now.Sub(b.last) > b.timeout/16 {
			b.ctl.SetReadDeadline(now.Add(b.timeout))
			b.last = now
		}
	}
	n, err := b.rc.Read(p)
	if err != nil {
		if isTimeout(err) {
			b.timedOut.Store(true)
		}
		if !b.last.IsZero() {
			b.ctl.SetReadDeadline(time.Time{})
		}
	}
	return n, err
}

func (b *idleBody) Close() error { return b.rc.Close() }

// clientBodyTimedOut reports whether reading r's body hit the client timeout.
func clientBodyTimedOut(r *http.Request) bool {
	if cb, ok := r.Body.(*countingBody); ok {
		if ib, ok := cb.rc.(*idleBody); ok {
			return ib.timedOut.Load()
		}
	}
	return false
}

// noCloseBody hides Close from Request.Write: the server owns the body.
type noCloseBody struct{ r io.Reader }

func (b noCloseBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (noCloseBody) Close() error                 { return nil }

func (h *httpHandler) proxy(w http.ResponseWriter, r *http.Request, t *httpTxn) {
	fe, be := t.fe, t.be
	ctx := r.Context()
	now := t.start

	// Persistence.
	var persist *server
	if st := be.sticky; st != nil {
		switch st.Mode {
		case spec.StickyInsert, spec.StickyPrefix:
			val, found := takeCookie(r.Header, st.Cookie, st.Mode == spec.StickyPrefix)
			t.term[2], t.term[3] = 'N', 'N'
			if found {
				srv := be.srvByCookie[val]
				switch {
				case srv == nil:
					t.term[2] = 'I'
				case !srv.st.usablePersist():
					t.term[2] = 'D'
				default:
					t.term[2] = 'V'
					persist = srv
				}
			}
		case spec.StickySource:
			if tbl := be.st.stick.Load(); tbl != nil {
				if name, ok := tbl.get(t.client.Addr(), now); ok {
					if srv := be.srvByName[name]; srv != nil && srv.st.usablePersist() {
						persist = srv
					}
				}
			}
		}
	}

	// X-Forwarded-For.
	if fe.forwardFor || be.forwardFor {
		ip := t.client.Addr()
		add := ip.IsValid()
		for _, p := range fe.ffExcept {
			if p.Contains(ip) {
				add = false
				break
			}
		}
		if add {
			v := ip.String()
			if prev := r.Header["X-Forwarded-For"]; len(prev) > 0 {
				v = strings.Join(prev, ", ") + ", " + v
			}
			r.Header["X-Forwarded-For"] = []string{v}
		}
	}

	upgrade := isUpgradeRequest(r.Header)
	removeHopHeaders(r.Header, upgrade)
	r.Header.Del("Expect")
	if _, ok := r.Header["User-Agent"]; !ok {
		r.Header["User-Agent"] = []string{""} // no Go default User-Agent
	}
	out := &http.Request{
		Method: r.Method, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		URL:    &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery, ForceQuery: r.URL.ForceQuery},
		Header: r.Header, Host: r.Host,
	}
	var body *countingBody
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		body = &countingBody{rc: r.Body}
		out.Body, out.ContentLength = noCloseBody{body}, r.ContentLength
	}

	pc := pickCtx{client: t.client.Addr()}
	if be.algo == spec.AlgoURI {
		pc.path = rawPath(r)
	}
	if persist != nil {
		t.tw = 0
		t.setServer(persist, false)
	} else if !t.assign(ctx, &pc) {
		switch {
		case ctx.Err() != nil:
			t.term[0], t.term[1] = 'C', 'Q'
			be.st.c.cliAbrt.Add(1)
			return
		default:
			t.term[0], t.term[1] = 'S', 'C'
		}
		h.writeError(w, t, http.StatusServiceUnavailable)
		return
	}

	idle := func(srv *server) *sconn {
		if be.sendProxy {
			if t.fc != nil {
				return t.fc.takeParked(srv)
			}
			return nil
		}
		return srv.pool.get()
	}
	var resp *http.Response
	var sc *sconn
	var wch chan error
	var stop func() bool
	for staleRetry := false; ; {
		var reused bool
		var err error
		sc, reused, err = t.connect(ctx, &pc, idle)
		if err != nil {
			if ctx.Err() != nil {
				t.term[0], t.term[1] = 'C', 'C'
				be.st.c.cliAbrt.Add(1)
				t.srv.st.c.cliAbrt.Add(1)
				return
			}
			t.term[0], t.term[1] = 'S', 'C'
			if isTimeout(err) {
				t.term[0] = 's'
			}
			h.writeError(w, t, http.StatusServiceUnavailable)
			return
		}
		if be.sticky != nil && be.sticky.Mode == spec.StickySource {
			if tbl := be.st.stick.Load(); tbl != nil {
				tbl.put(t.client.Addr(), t.srv.name, now)
			}
		}
		sc.buffers()
		sent := time.Now()
		stop = context.AfterFunc(ctx, sc.raw.kick)
		resp, wch, err = roundTrip(sc, out, body, w)
		if err == nil {
			t.tr = time.Since(sent)
			break
		}
		stop()
		sc.close()
		if reused && !staleRetry && body == nil && ctx.Err() == nil && isClosedByPeer(err) {
			staleRetry = true // the server closed an idle connection: try a new one
			continue
		}
		switch {
		case clientBodyTimedOut(r):
			t.term[0], t.term[1] = 'c', 'D'
			be.st.c.cliAbrt.Add(1)
			t.srv.st.c.cliAbrt.Add(1)
			h.writeError(w, t, http.StatusRequestTimeout)
			return
		case ctx.Err() != nil:
			t.term[0], t.term[1] = 'C', 'H'
			be.st.c.cliAbrt.Add(1)
			t.srv.st.c.cliAbrt.Add(1)
		case isTimeout(err):
			t.term[0], t.term[1] = 's', 'H'
			be.st.c.eresp.Add(1)
			t.srv.st.c.eresp.Add(1)
			h.writeError(w, t, http.StatusGatewayTimeout)
		default:
			t.term[0], t.term[1] = 'S', 'H'
			be.st.c.eresp.Add(1)
			t.srv.st.c.eresp.Add(1)
			h.writeError(w, t, http.StatusBadGateway)
		}
		return
	}
	srv := t.srv
	defer func() {
		if body != nil {
			n := body.n.Load()
			be.st.c.bin.Add(n)
			srv.st.c.bin.Add(n)
		}
	}()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		stop()
		h.tunnel(w, t, sc, resp, upgrade)
		return
	}

	// Response headers.
	removeHopHeaders(resp.Header, false)
	if st := be.sticky; st != nil && (st.Mode == spec.StickyInsert || st.Mode == spec.StickyPrefix) {
		h.responseCookies(resp.Header, t, st, persist)
	}
	gz := h.compressor(r, resp, t)
	dst := w.Header()
	for k, v := range resp.Header {
		dst[k] = v
	}
	if _, ok := resp.Header["Date"]; !ok {
		dst["Date"] = nil
	}
	if _, ok := resp.Header["Content-Type"]; !ok {
		dst["Content-Type"] = nil
	}
	if gz != nil {
		delete(dst, "Content-Length")
		dst["Content-Encoding"] = []string{"gzip"}
		dst.Add("Vary", "Accept-Encoding")
		if et := dst.Get("Etag"); et != "" && !strings.HasPrefix(et, "W/") {
			dst.Set("Etag", "W/"+et)
		}
	} else if resp.ContentLength >= 0 && resp.Header.Get("Content-Length") == "" && r.Method != http.MethodHead && bodyAllowed(resp.StatusCode) {
		dst["Content-Length"] = []string{strconv.FormatInt(resp.ContentLength, 10)}
	}
	if len(resp.Trailer) > 0 {
		keys := make([]string, 0, len(resp.Trailer))
		for k := range resp.Trailer {
			keys = append(keys, k)
		}
		dst["Trailer"] = []string{strings.Join(keys, ", ")}
	}
	t.status = resp.StatusCode
	t.bytesOut += headerSize(resp.StatusCode, dst)
	w.WriteHeader(resp.StatusCode)
	fe.st.c.status(resp.StatusCode)
	be.st.c.status(resp.StatusCode)
	srv.st.c.status(resp.StatusCode)

	complete := h.copyBody(ctx, w, t, resp, gz)
	if complete {
		for k, v := range resp.Trailer {
			dst[k] = v
		}
	}
	writerOK := true
	if wch != nil {
		select {
		case err := <-wch:
			writerOK = err == nil
		default:
			writerOK = false
		}
	}
	if stop() && complete && writerOK && !resp.Close {
		sc.raw.rearm()
		if be.sendProxy {
			if t.fc != nil {
				t.fc.park(sc)
			} else {
				sc.close()
			}
		} else {
			srv.pool.put(sc)
		}
	} else {
		sc.close()
	}
}

// roundTrip writes the request and reads the final response (forwarding
// informational 1xx responses except 100). Request bodies are written
// concurrently so an early response is seen.
func roundTrip(sc *sconn, out *http.Request, body *countingBody, w http.ResponseWriter) (*http.Response, chan error, error) {
	var wch chan error
	if body == nil {
		err := out.Write(sc.bw)
		if err == nil {
			err = sc.bw.Flush()
		}
		if err != nil {
			return nil, nil, err
		}
	} else {
		wch = make(chan error, 1)
		go func() {
			err := out.Write(sc.bw)
			if err == nil {
				err = sc.bw.Flush()
			}
			if err != nil && body.failed.Load() {
				// The client stopped sending the body: don't wait for the
				// server to give up on it too.
				sc.raw.kick()
			}
			wch <- err
		}()
	}
	for {
		resp, err := http.ReadResponse(sc.br, out)
		if err != nil {
			if wch != nil {
				select {
				case werr := <-wch:
					if werr != nil && !isTimeout(err) {
						err = werr
					}
				default:
				}
			}
			return nil, nil, err
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
			if resp.StatusCode != http.StatusContinue {
				hdr := w.Header()
				for k, v := range resp.Header {
					hdr[k] = v
				}
				w.WriteHeader(resp.StatusCode)
				clear(hdr)
			}
			continue
		}
		return resp, wch, nil
	}
}

func (h *httpHandler) copyBody(ctx context.Context, w http.ResponseWriter, t *httpTxn, resp *http.Response, gz *gzip.Writer) bool {
	fe, be, srv := t.fe, t.be, t.srv
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	rc := http.NewResponseController(w)
	var cw *countWriter
	if gz != nil {
		cw = &countWriter{w: w}
		gz.Reset(cw)
		defer func() {
			gzipPool.Put(gz)
			fe.st.c.compOut.Add(cw.n)
			be.st.c.compOut.Add(cw.n)
			t.bytesOut += cw.n
		}()
	}
	var sent int64
	defer func() {
		out := sent
		if cw != nil {
			out = cw.n
		}
		fe.st.c.bout.Add(out)
		be.st.c.bout.Add(out)
		srv.st.c.bout.Add(out)
	}()
	timeout := fe.clientTimeout
	var lastDeadline time.Time
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			now := time.Now()
			if now.Sub(lastDeadline) > timeout/16 {
				rc.SetWriteDeadline(now.Add(timeout))
				lastDeadline = now
			}
			var werr error
			if gz != nil {
				_, werr = gz.Write(buf[:n])
				fe.st.c.compIn.Add(int64(n))
				be.st.c.compIn.Add(int64(n))
			} else {
				var nw int
				nw, werr = w.Write(buf[:n])
				sent += int64(nw)
				t.bytesOut += int64(nw)
			}
			if werr != nil {
				t.term[0], t.term[1] = 'C', 'D'
				be.st.c.cliAbrt.Add(1)
				srv.st.c.cliAbrt.Add(1)
				return false
			}
			if err == nil && n < len(buf) {
				if gz != nil {
					gz.Flush()
				}
				rc.Flush()
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			if gz != nil {
				gz.Close()
			}
			return true
		}
		if ctx.Err() != nil {
			t.term[0], t.term[1] = 'C', 'D'
			be.st.c.cliAbrt.Add(1)
			srv.st.c.cliAbrt.Add(1)
			return false
		}
		t.term[0], t.term[1] = 'S', 'D'
		if isTimeout(err) {
			t.term[0] = 's'
		}
		be.st.c.srvAbrt.Add(1)
		srv.st.c.srvAbrt.Add(1)
		t.abort = true
		return false
	}
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// tunnel completes a protocol switch (WebSocket) and copies both directions.
func (h *httpHandler) tunnel(w http.ResponseWriter, t *httpTxn, sc *sconn, resp *http.Response, requested bool) {
	be, fe, srv := t.be, t.fe, t.srv
	if !requested {
		sc.close()
		t.term[0], t.term[1] = 'S', 'H'
		be.st.c.eresp.Add(1)
		srv.st.c.eresp.Add(1)
		h.writeError(w, t, http.StatusBadGateway)
		return
	}
	hdr := w.Header()
	for k, v := range resp.Header {
		hdr[k] = v
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		sc.close()
		t.term[0], t.term[1] = 'P', 'H'
		h.writeError(w, t, http.StatusBadGateway)
		return
	}
	defer conn.Close()
	defer sc.close()
	t.status = http.StatusSwitchingProtocols
	fe.st.c.status(t.status)
	be.st.c.status(t.status)
	srv.st.c.status(t.status)
	resp.Header, resp.Body, resp.ContentLength = hdr, nil, 0
	if err := resp.Write(brw); err != nil || brw.Flush() != nil {
		t.term[0], t.term[1] = 'C', 'D'
		return
	}
	t.bytesOut += headerSize(http.StatusSwitchingProtocols, hdr)
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		sc.conn.Write(b)
	}
	if n := sc.br.Buffered(); n > 0 {
		b, _ := sc.br.Peek(n)
		conn.Write(b)
	}
	sc.raw.off.Store(true)
	sc.raw.Conn.SetDeadline(time.Time{})
	conn.SetDeadline(time.Time{})
	res := pipe(
		pipeEnd{conn: conn, r: conn, timeout: fe.clientTimeout, closeW: func() { closeWrite(conn) }},
		pipeEnd{conn: sc.conn, r: sc.conn, timeout: be.serverTimeout, closeW: sc.closeWrite},
		func(n int) { fe.st.c.bin.Add(int64(n)); be.st.c.bin.Add(int64(n)); srv.st.c.bin.Add(int64(n)) },
		func(n int) { fe.st.c.bout.Add(int64(n)); be.st.c.bout.Add(int64(n)); srv.st.c.bout.Add(int64(n)) },
	)
	t.bytesOut += res.down
	if res.term != '-' {
		t.term[0], t.term[1] = res.term, 'D'
	}
}

func closeWrite(c any) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// writeError answers with HAProxy's built-in error page.
func (h *httpHandler) writeError(w http.ResponseWriter, t *httpTxn, code int) {
	body := errorPage(code)
	hdr := w.Header()
	clear(hdr)
	hdr["Content-Length"] = []string{strconv.Itoa(len(body))}
	hdr["Cache-Control"] = []string{"no-cache"}
	hdr["Content-Type"] = []string{"text/html"}
	hdr["Connection"] = []string{"close"}
	hdr["Date"] = nil
	w.WriteHeader(code)
	w.Write(body)
	t.status = code
	t.bytesOut = headerSize(code, hdr) + int64(len(body))
	t.fe.st.c.status(code)
	t.fe.st.c.bout.Add(t.bytesOut)
	if t.be != nil {
		t.be.st.c.status(code)
		t.be.st.c.bout.Add(t.bytesOut)
	}
}

func headerSize(code int, h http.Header) int64 {
	n := len("HTTP/1.1 000 ") + len(http.StatusText(code)) + 4
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return int64(n)
}

func bodyAllowed(code int) bool {
	return !(code >= 100 && code < 200) && code != http.StatusNoContent && code != http.StatusNotModified
}

// compressor returns a pooled gzip writer when the response must be
// compressed (HAProxy `compression algo gzip` rules).
func (h *httpHandler) compressor(r *http.Request, resp *http.Response, t *httpTxn) *gzip.Writer {
	fe, be := t.fe, t.be
	if !fe.compression {
		return nil
	}
	skip := func() *gzip.Writer {
		fe.st.c.compByp.Add(1)
		be.st.c.compByp.Add(1)
		return nil
	}
	if r.Method == http.MethodHead || !r.ProtoAtLeast(1, 1) || !resp.ProtoAtLeast(1, 1) || resp.ContentLength == 0 {
		return nil
	}
	switch resp.StatusCode {
	case 200, 201, 202, 203:
	default:
		return nil
	}
	if !acceptsGzip(r.Header["Accept-Encoding"]) {
		return nil
	}
	if len(resp.Header["Content-Encoding"]) > 0 {
		return skip()
	}
	for _, v := range resp.Header["Cache-Control"] {
		if httpguts.HeaderValuesContainsToken([]string{v}, "no-transform") {
			return skip()
		}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	ok := false
	for _, typ := range compressTypes {
		if strings.HasPrefix(ct, typ) {
			ok = true
			break
		}
	}
	if !ok {
		return nil
	}
	fe.st.c.compRsp.Add(1)
	be.st.c.compRsp.Add(1)
	t.compressed = true
	return gzipPool.Get().(*gzip.Writer)
}

// acceptsGzip reports whether Accept-Encoding allows gzip (q > 0).
func acceptsGzip(values []string) bool {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			q := 1.0
			for p := range strings.SplitSeq(params, ";") {
				if k, val, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						q = f
					}
				}
			}
			return q > 0
		}
	}
	return false
}

var hopHeaders = []string{"Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding"}

func isUpgradeRequest(h http.Header) bool {
	return len(h["Upgrade"]) > 0 && httpguts.HeaderValuesContainsToken(h["Connection"], "upgrade")
}

// removeHopHeaders strips hop-by-hop headers; keepUpgrade keeps
// "Connection: Upgrade" and Upgrade for protocol switches.
func removeHopHeaders(h http.Header, keepUpgrade bool) {
	if conn := h["Connection"]; len(conn) > 0 {
		for _, v := range conn {
			for tok := range strings.SplitSeq(v, ",") {
				if tok = strings.TrimSpace(tok); tok != "" && !strings.EqualFold(tok, "upgrade") {
					h.Del(tok)
				}
			}
		}
		delete(h, "Connection")
	}
	for _, k := range hopHeaders {
		delete(h, k)
	}
	if keepUpgrade {
		h["Connection"] = []string{"Upgrade"}
	} else {
		delete(h, "Upgrade")
	}
}

// takeCookie finds the persistence cookie in the request. Insert mode
// removes it (indirect); prefix mode rewrites "<name>=<srv>~<value>" to
// "<name>=<value>" and returns srv.
func takeCookie(h http.Header, name string, prefix bool) (string, bool) {
	lines := h["Cookie"]
	for i, line := range lines {
		if !strings.Contains(line, name) {
			continue
		}
		parts := strings.Split(line, ";")
		for j, part := range parts {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || strings.TrimSpace(k) != name {
				continue
			}
			v = strings.TrimSpace(v)
			if prefix {
				srv, rest, ok := strings.Cut(v, "~")
				if !ok {
					return "", true
				}
				parts[j] = " " + name + "=" + rest
				if j == 0 {
					parts[j] = parts[j][1:]
				}
				lines[i] = strings.Join(parts, ";")
				return srv, true
			}
			parts = append(parts[:j], parts[j+1:]...)
			for n := range parts {
				parts[n] = strings.TrimSpace(parts[n])
			}
			if joined := strings.Join(parts, "; "); joined != "" {
				lines[i] = joined
			} else {
				lines = append(lines[:i], lines[i+1:]...)
				if len(lines) == 0 {
					delete(h, "Cookie")
				} else {
					h["Cookie"] = lines
				}
			}
			return v, true
		}
	}
	return "", false
}

// responseCookies applies insert (indirect nocache) and prefix (nocache)
// persistence to the response.
func (h *httpHandler) responseCookies(hdr http.Header, t *httpTxn, st *spec.Sticky, persist *server) {
	srv := t.srv
	name := st.Cookie
	if st.Mode == spec.StickyPrefix {
		if srv.cookie == "" {
			return
		}
		sc := hdr["Set-Cookie"]
		for i, v := range sc {
			k, val, ok := strings.Cut(v, "=")
			if ok && strings.TrimSpace(k) == name {
				sc[i] = name + "=" + srv.cookie + "~" + strings.TrimLeft(val, " ")
				t.term[3] = 'R'
			}
		}
		if t.term[3] == 'R' {
			hdr.Add("Cache-Control", "private")
		}
		return
	}
	// indirect: the server never sees nor sets the persistence cookie.
	if sc := hdr["Set-Cookie"]; len(sc) > 0 {
		kept := sc[:0]
		for _, v := range sc {
			if k, _, ok := strings.Cut(v, "="); ok && strings.TrimSpace(k) == name {
				t.term[3] = 'D'
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(hdr, "Set-Cookie")
		} else {
			hdr["Set-Cookie"] = kept
		}
	}
	if srv.cookie == "" || (persist == srv && t.term[2] == 'V') {
		return
	}
	hdr.Add("Set-Cookie", name+"="+srv.cookie+"; path=/")
	hdr.Add("Cache-Control", "private")
	t.term[3] = 'I'
}
