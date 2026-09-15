package edge

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// listenerHandler serves requests of one HTTP, HTTPS or QUIC listener with
// the routing table current when the request arrived.
type listenerHandler struct {
	srv  *Server
	role *listenerRole
}

func (lh *listenerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := lh.srv
	rt := s.table.Load()
	if rt == nil {
		panic(http.ErrAbortHandler)
	}
	rs := getState()
	rs.srv, rs.rt, rs.role, rs.r, rs.start = s, rt, lh.role, r, time.Now()
	rs.rw.rw = w
	rs.fw.init(rs)
	// The transport may still read the body after the handler returned, so
	// the counter lives outside the pooled state.
	var body *countingBody
	if r.Body != nil && r.Body != http.NoBody {
		body = &countingBody{rc: r.Body}
		r.Body = body
	}
	s.metrics.writing.Add(1)
	defer func() {
		p := recover()
		if p != nil && p != http.ErrAbortHandler {
			s.errlog.logf(levelAlert, "panic serving %s %s: %v %s", r.Method, r.RequestURI, p, debug.Stack())
		}
		if p == nil {
			rs.fw.finish()
		}
		s.logRequest(rs, body)
		s.metrics.writing.Add(-1)
		putState(rs)
		if p != nil {
			panic(http.ErrAbortHandler)
		}
	}()
	s.serve(rs)
}

const acmePrefix = "/.well-known/acme-challenge/"

func (s *Server) serve(rs *reqState) {
	r := rs.r
	rs.requestID = newRequestID()
	rs.splitRemote()
	rs.host = requestHost(r.Host)
	set := &rs.rt.https
	if rs.role.scheme == "http" {
		set = &rs.rt.http
	}
	vs := set.lookup(hostKey(rs.host))
	rs.vs = vs
	w := &rs.fw
	if len(vs.headers) > 0 {
		h := w.Header()
		for _, kv := range vs.headers {
			h[kv.name] = kv.value
		}
	}
	rs.rawPath, rs.rawQuery = splitRequestURI(rs.requestURI())
	rs.hasArgs = rs.rawQuery != ""

	if rs.rt.blocklist.len() > 0 {
		if _, blocked := rs.rt.blocklist.lookup(rs.clientIP); blocked {
			writePage(w, http.StatusForbidden)
			return
		}
	}
	path, ok := normalizePath(rs.rawPath)
	if !ok {
		writePage(w, http.StatusBadRequest)
		return
	}
	rs.path = path

	switch vs.kind {
	case kindHost:
		s.serveHost(rs, vs.host)
	case kindHTTPSRedirect:
		if s.serveACME(rs) {
			return
		}
		host := rs.host
		if host == "" {
			host = vs.name
		}
		target := "https://" + host
		if p := rs.rt.httpsPort; p != 443 && p != 0 {
			target += ":" + strconv.Itoa(p)
		}
		writeRedirect(w, http.StatusMovedPermanently, target+rs.requestURI())
	case kindGroup:
		s.serveGroup(rs, vs)
	case kindDefault:
		s.serveDefault(rs, vs)
	}
}

func (s *Server) serveDefault(rs *reqState, vs *vserver) {
	if vs.action == "host" {
		s.serveHost(rs, vs.host)
		return
	}
	if s.serveACME(rs) {
		return
	}
	w := &rs.fw
	switch vs.action {
	case "404":
		writeBody(w, http.StatusNotFound, "text/html", pages[http.StatusNotFound])
	case "redirect":
		writeRedirect(w, http.StatusFound, vs.redirectTo.expand(rs))
	default: // close: nginx `return 444` drops the connection without a response
		rs.aborted = true
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) serveGroup(rs *reqState, vs *vserver) {
	if vs.acme && s.serveACME(rs) {
		return
	}
	g := vs.group
	w := &rs.fw
	for i := range g.paths {
		if p := &g.paths[i]; p.matches(rs.path) {
			writeRedirect(w, p.code, p.target(rs))
			return
		}
	}
	if wr := g.whole; wr != nil {
		target := wr.to.expand(rs)
		if wr.keep {
			target = strings.TrimRight(target, "/") + rs.requestURI()
		}
		writeRedirect(w, wr.code, target)
		return
	}
	writePage(w, http.StatusNotFound)
}

func (s *Server) serveHost(rs *reqState, h *hostRT) {
	r := rs.r
	w := &rs.fw
	if h.blockExploits && isExploit(rs.requestURI(), rs.rawQuery, r.Header.Get("User-Agent")) {
		writePage(w, http.StatusForbidden)
		return
	}
	if h.limiter != nil && !h.limiter.allow(rs.clientIP, rs.start) {
		writePage(w, http.StatusTooManyRequests)
		return
	}
	if h.maxBody > 0 && r.ContentLength > h.maxBody {
		writePage(w, http.StatusRequestEntityTooLarge)
		return
	}
	if rs.vs.acme && s.serveACME(rs) {
		return
	}
	for i := range h.pathRedirects {
		if p := &h.pathRedirects[i]; p.matches(rs.path) {
			writeRedirect(w, p.code, p.target(rs))
			return
		}
	}
	loc, slash := h.match(rs.path)
	if slash != nil {
		target := rs.role.scheme + "://" + rs.host
		if !(rs.role.scheme == "http" && rs.role.port == 80) && !(rs.role.scheme == "https" && rs.role.port == 443) {
			target += ":" + rs.role.portStr
		}
		target += escapeURI(slash.path)
		if rs.hasArgs {
			target += "?" + rs.rawQuery
		}
		writeRedirect(w, http.StatusMovedPermanently, target)
		return
	}
	if loc == nil {
		writePage(w, http.StatusNotFound)
		return
	}
	rs.loc = loc
	if !s.authorize(rs, h, loc) {
		return
	}
	if loc.deny {
		writePage(w, http.StatusForbidden)
		return
	}
	if h.maxBody > 0 && r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(rs.rw.rw, r.Body, h.maxBody)
	}
	s.proxy(rs, loc)
}

// serveACME answers /.well-known/acme-challenge/<token> from the webroot.
// It reports false when the path is not a challenge path.
func (s *Server) serveACME(rs *reqState) bool {
	token, ok := strings.CutPrefix(rs.path, acmePrefix)
	if !ok {
		return false
	}
	w := &rs.fw
	if rs.r.Method != http.MethodGet && rs.r.Method != http.MethodHead {
		writePage(w, http.StatusMethodNotAllowed)
		return true
	}
	root := rs.rt.acmeRoot
	if root == "" || !validToken(token) {
		writePage(w, http.StatusNotFound)
		return true
	}
	f, err := os.Open(filepath.Join(root, ".well-known", "acme-challenge", token))
	if err != nil {
		writePage(w, http.StatusNotFound)
		return true
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		writePage(w, http.StatusNotFound)
		return true
	}
	h := w.Header()
	h["Content-Type"] = []string{"text/plain"}
	h["Content-Length"] = []string{strconv.FormatInt(st.Size(), 10)}
	h["Last-Modified"] = []string{st.ModTime().UTC().Format(http.TimeFormat)}
	w.WriteHeader(http.StatusOK)
	if rs.r.Method == http.MethodGet {
		io.Copy(w, io.LimitReader(f, st.Size()))
	}
	return true
}

// validToken accepts ACME tokens (base64url), which rules out traversal.
func validToken(t string) bool {
	if t == "" || len(t) > 256 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
