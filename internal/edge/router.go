package edge

import "strings"

// serverSet selects the server block for a name on one listener, like nginx
// server_name matching: exact name, then the longest "*." wildcard, then the
// default server. The first server to claim a name keeps it.
type serverSet struct {
	exact map[string]*vserver
	wild  map[string]*vserver // "example.com" for *.example.com
	def   *vserver
}

func newServerSet() serverSet {
	return serverSet{exact: map[string]*vserver{}, wild: map[string]*vserver{}}
}

func (s *serverSet) addAll(names []string, vs *vserver) {
	for _, n := range names {
		n = hostKey(strings.ToLower(strings.TrimSpace(n)))
		if n == "" {
			continue
		}
		m := s.exact
		if rest, ok := strings.CutPrefix(n, "*."); ok {
			m, n = s.wild, rest
		}
		if _, taken := m[n]; !taken {
			m[n] = vs
		}
	}
}

// lookup takes a normalized name (see hostKey).
func (s *serverSet) lookup(name string) *vserver {
	if name != "" {
		if vs := s.exact[name]; vs != nil {
			return vs
		}
		if len(s.wild) > 0 {
			for i := strings.IndexByte(name, '.'); i > 0 && i < len(name)-1; {
				suffix := name[i+1:]
				if vs := s.wild[suffix]; vs != nil {
					return vs
				}
				j := strings.IndexByte(suffix, '.')
				if j < 0 {
					break
				}
				i += j + 1
			}
		}
	}
	return s.def
}

// requestHost returns $host: the Host header lowercased without port
// (IPv6 literals keep their brackets).
func requestHost(h string) string {
	if h == "" {
		return ""
	}
	if h[0] == '[' {
		if i := strings.IndexByte(h, ']'); i > 0 {
			h = h[:i+1]
		}
	} else if i := strings.LastIndexByte(h, ':'); i >= 0 && strings.IndexByte(h, ':') == i {
		h = h[:i]
	}
	return toLowerASCII(h)
}

// hostKey normalizes $host or a server name for matching: no brackets, no
// trailing dot.
func hostKey(h string) string {
	if len(h) > 1 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	return strings.TrimSuffix(h, ".")
}

func toLowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; c >= 'A' && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// splitRequestURI splits a raw request target into path and query. The
// absolute form ("http://host/path") yields just the path.
func splitRequestURI(uri string) (path, query string) {
	if uri != "" && uri[0] != '/' {
		if i := strings.Index(uri, "://"); i > 0 {
			rest := uri[i+3:]
			if j := strings.IndexAny(rest, "/?"); j >= 0 {
				uri = rest[j:]
				if uri[0] == '?' {
					uri = "/" + uri
				}
			} else {
				uri = "/"
			}
		}
	}
	path, query, _ = strings.Cut(uri, "?")
	return path, query
}

// normalizePath decodes a raw path, resolves "." and ".." segments and merges
// slashes (nginx $uri with merge_slashes on). ok is false for requests nginx
// rejects with 400: bad escapes, NUL bytes, or ".." above the root.
func normalizePath(raw string) (string, bool) {
	if raw == "" || raw[0] != '/' {
		return "", false
	}
	if !strings.Contains(raw, "%") && !strings.Contains(raw, "//") && !strings.Contains(raw, "/.") {
		return raw, true
	}
	dec := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '%' {
			if i+2 >= len(raw) || !isHex(raw[i+1]) || !isHex(raw[i+2]) {
				return "", false
			}
			c = unhex(raw[i+1])<<4 | unhex(raw[i+2])
			if c == 0 {
				return "", false
			}
			i += 2
		}
		dec = append(dec, c)
	}
	out := make([]byte, 0, len(dec))
	for i := 0; i < len(dec); {
		j := i + 1
		for j < len(dec) && dec[j] != '/' {
			j++
		}
		seg := dec[i+1 : j]
		last := j == len(dec)
		switch {
		case len(seg) == 0 || (len(seg) == 1 && seg[0] == '.'):
			if last {
				out = append(out, '/')
			}
		case len(seg) == 2 && seg[0] == '.' && seg[1] == '.':
			k := strings.LastIndexByte(string(out), '/')
			if k < 0 {
				return "", false
			}
			out = out[:k]
			if last {
				out = append(out, '/')
			}
		default:
			out = append(out, '/')
			out = append(out, seg...)
		}
		i = j
	}
	if len(out) == 0 {
		out = append(out, '/')
	}
	return string(out), true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// escapeURI escapes a decoded URI for an upstream request line or a Location
// header like nginx's ngx_escape_uri(NGX_ESCAPE_URI).
func escapeURI(s string) string {
	n := 0
	for i := 0; i < len(s); i++ {
		if uriEscape(s[i]) {
			n++
		}
	}
	if n == 0 {
		return s
	}
	b := make([]byte, 0, len(s)+2*n)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if uriEscape(c) {
			b = append(b, '%', "0123456789ABCDEF"[c>>4], "0123456789ABCDEF"[c&0xf])
		} else {
			b = append(b, c)
		}
	}
	return string(b)
}

func uriEscape(c byte) bool {
	return c <= 0x20 || c >= 0x7f || c == '#' || c == '%' || c == '?' || c == '"'
}

// match returns the longest prefix location for path, or the proxy location
// to 301-redirect to when path equals its path minus the trailing slash and
// no location matches path exactly (nginx auto_redirect).
func (h *hostRT) match(path string) (loc, slash *locationRT) {
	for _, l := range h.locations { // longest first
		if len(l.path) > len(path) {
			if slash == nil && !l.deny && len(l.path) == len(path)+1 && l.path[len(path)] == '/' && strings.HasPrefix(l.path, path) {
				slash = l
			}
			continue
		}
		if strings.HasPrefix(path, l.path) {
			if slash != nil && len(l.path) < len(path) {
				return nil, slash
			}
			return l, nil
		}
	}
	return nil, slash
}

// matches implements "^<from>(/.*)?$" on the decoded path.
func (p *pathRedirectRT) matches(path string) bool {
	switch {
	case path == p.from:
		return true
	case strings.HasPrefix(path, p.fromSlash):
		return strings.IndexByte(path[len(p.from):], '\n') < 0
	}
	return false
}

// target builds the Location for a matching request.
func (p *pathRedirectRT) target(rs *reqState) string {
	to := p.to.expand(rs)
	if !p.keep {
		return to
	}
	t := strings.TrimRight(to, "/") + escapeURI(rs.path[len(p.from):])
	if rs.hasArgs {
		t += "?" + rs.rawQuery
	}
	return t
}

// rewrite applies the location's upstream path rule to the decoded path. ok
// is false when the raw request URI goes upstream unchanged.
func (l *locationRT) rewrite(path string) (string, bool) {
	if l.strip && l.path != "/" {
		rest := strings.TrimPrefix(path[len(l.prefix):], "/")
		return l.up.base + "/" + rest, true
	}
	if l.up.base != "" {
		return l.up.base + path, true
	}
	return "", false
}
