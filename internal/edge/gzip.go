package edge

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// filterWriter applies nginx's response header/body filters to everything a
// host answers: `expires 30d` on asset cache locations and gzip (gzip on,
// gzip_vary on, gzip_proxied any, gzip_comp_level 5, gzip_min_length 1024).
type filterWriter struct {
	rs       *reqState
	next     *respWriter
	gz       *gzip.Writer
	accepts  bool // client accepts gzip over HTTP/1.1+
	disabled bool // HEAD
	decided  bool
}

var gzipTypes = map[string]bool{
	"text/html": true, "text/plain": true, "text/css": true, "text/xml": true, "text/javascript": true,
	"application/javascript": true, "application/json": true, "application/xml": true,
	"application/rss+xml": true, "application/atom+xml": true, "image/svg+xml": true,
	"font/ttf": true, "font/otf": true,
}

const gzipMinLength = 1024

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, 5)
	return w
}}

func (f *filterWriter) init(rs *reqState) {
	r := rs.r
	*f = filterWriter{rs: rs, next: &rs.rw}
	f.disabled = r.Method == http.MethodHead
	f.accepts = !(r.ProtoMajor == 1 && r.ProtoMinor == 0) && acceptsGzip(r.Header["Accept-Encoding"])
}

// acceptsGzip reports whether gzip is listed without q=0.
func acceptsGzip(values []string) bool {
	for _, v := range values {
		for v != "" {
			var item string
			item, v, _ = strings.Cut(v, ",")
			name, params, _ := strings.Cut(item, ";")
			name = strings.TrimSpace(name)
			if !strings.EqualFold(name, "gzip") && !strings.EqualFold(name, "x-gzip") {
				continue
			}
			params = strings.ReplaceAll(params, " ", "")
			if q, ok := strings.CutPrefix(params, "q="); ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
					return false
				}
			}
			return true
		}
	}
	return false
}

func (f *filterWriter) Header() http.Header { return f.next.Header() }

func (f *filterWriter) WriteHeader(code int) {
	if f.decided {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		f.next.WriteHeader(code)
		return
	}
	f.decided = true
	h := f.next.Header()
	if f.rs.expires && expiresStatus(code) {
		h["Expires"] = []string{time.Now().Add(cacheValid).UTC().Format(http.TimeFormat)}
		h["Cache-Control"] = []string{"max-age=2592000"}
	}
	if f.compressible(code, h) {
		if !varyHas(h, "Accept-Encoding") {
			h.Add("Vary", "Accept-Encoding")
		}
		if f.accepts {
			delete(h, "Content-Length")
			h["Content-Encoding"] = []string{"gzip"}
			if etag := h.Get("Etag"); strings.HasPrefix(etag, `"`) {
				h["Etag"] = []string{"W/" + etag}
			}
			f.gz = gzipPool.Get().(*gzip.Writer)
			f.gz.Reset(f.next)
		}
	}
	f.next.WriteHeader(code)
}

func (f *filterWriter) compressible(code int, h http.Header) bool {
	if f.disabled || (code != http.StatusOK && code != http.StatusForbidden && code != http.StatusNotFound) {
		return false
	}
	if ce := h.Get("Content-Encoding"); ce != "" {
		return false
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n < gzipMinLength {
			return false
		}
	}
	ct, _, _ := strings.Cut(h.Get("Content-Type"), ";")
	return gzipTypes[toLowerASCII(strings.TrimSpace(ct))]
}

func varyHas(h http.Header, token string) bool {
	for _, v := range h["Vary"] {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func expiresStatus(code int) bool {
	switch code {
	case 200, 201, 204, 206, 301, 302, 303, 304, 307, 308:
		return true
	}
	return false
}

func (f *filterWriter) Write(p []byte) (int, error) {
	if !f.decided {
		f.WriteHeader(http.StatusOK)
	}
	if f.gz != nil {
		return f.gz.Write(p)
	}
	return f.next.Write(p)
}

func (f *filterWriter) Flush() {
	if f.gz != nil {
		f.gz.Flush()
	}
	f.next.Flush()
}

func (f *filterWriter) Unwrap() http.ResponseWriter { return f.next }

// finish completes a gzip stream; called once the handler is done.
func (f *filterWriter) finish() {
	if f.gz != nil {
		f.gz.Close()
		f.gz.Reset(nil)
		gzipPool.Put(f.gz)
		f.gz = nil
	}
}
