// Package sniff inspects the first bytes of a connection without consuming or
// decrypting it: the TLS ClientHello server name and the HTTP/1 Host header.
// Relay Balancer uses it for SNI routing and the tunnel gateway for routing
// :443 and :80.
package sniff

import (
	"bytes"
	"strings"
)

// ClientHelloSNI inspects the first bytes of a connection. done is false
// while more data is needed to decide; once done, sni is the lowercase
// server_name of a TLS ClientHello ("" for non-TLS data or no SNI), like
// HAProxy's req.ssl_sni.
func ClientHelloSNI(data []byte) (sni string, done bool) {
	if len(data) == 0 {
		return "", false
	}
	if data[0] != 0x16 {
		return "", true
	}
	var hs []byte
	p := data
	for {
		if len(p) < 5 {
			return "", false
		}
		if p[0] != 0x16 {
			return "", true
		}
		rl := int(p[3])<<8 | int(p[4])
		if rl == 0 || rl > 18432 {
			return "", true
		}
		if len(p) < 5+rl {
			return "", false
		}
		if hs == nil && len(p) >= 5+rl {
			hs = p[5 : 5+rl : 5+rl]
		} else {
			hs = append(hs, p[5:5+rl]...)
		}
		p = p[5+rl:]
		if len(hs) >= 4 {
			if hs[0] != 1 { // client_hello
				return "", true
			}
			hl := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
			if hl > 1<<16 {
				return "", true
			}
			if len(hs) >= 4+hl {
				return helloServerName(hs[4 : 4+hl]), true
			}
		}
	}
}

// helloServerName extracts the host_name from a ClientHello body.
func helloServerName(b []byte) string {
	skip := func(n int) bool {
		if len(b) < n {
			return false
		}
		b = b[n:]
		return true
	}
	vec := func(lenBytes int) ([]byte, bool) {
		if len(b) < lenBytes {
			return nil, false
		}
		n := 0
		for i := range lenBytes {
			n = n<<8 | int(b[i])
		}
		if len(b) < lenBytes+n {
			return nil, false
		}
		v := b[lenBytes : lenBytes+n]
		b = b[lenBytes+n:]
		return v, true
	}
	if !skip(2 + 32) { // version, random
		return ""
	}
	if _, ok := vec(1); !ok { // session id
		return ""
	}
	if _, ok := vec(2); !ok { // cipher suites
		return ""
	}
	if _, ok := vec(1); !ok { // compression methods
		return ""
	}
	exts, ok := vec(2)
	if !ok {
		return ""
	}
	b = exts
	for len(b) >= 4 {
		typ := int(b[0])<<8 | int(b[1])
		b = b[2:]
		data, ok := vec(2)
		if !ok {
			return ""
		}
		if typ != 0 { // server_name
			continue
		}
		b = data
		list, ok := vec(2)
		if !ok {
			return ""
		}
		b = list
		for len(b) >= 3 {
			nameType := b[0]
			b = b[1:]
			name, ok := vec(2)
			if !ok {
				return ""
			}
			if nameType == 0 {
				return strings.ToLower(string(name))
			}
		}
		return ""
	}
	return ""
}

// HTTPHost inspects the start of a plain HTTP/1 connection. done is false
// while more data is needed. Once done, host is the lowercase Host header
// without port; it is "" when the data is not an HTTP/1 request, the request
// line or headers are malformed, there is no Host header, or the client
// speaks HTTP/2 prior knowledge ("PRI * HTTP/2.0").
func HTTPHost(data []byte) (host string, done bool) {
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		// Reject early when the request line can't be HTTP/1.
		if line, _, ok := bytes.Cut(data, []byte("\r\n")); ok && !requestLine(line) {
			return "", true
		}
		if len(data) > 0 && !tokenPrefix(data) {
			return "", true
		}
		return "", false
	}
	lines := bytes.Split(data[:end], []byte("\r\n"))
	if !requestLine(lines[0]) {
		return "", true
	}
	for _, l := range lines[1:] {
		name, value, ok := bytes.Cut(l, []byte(":"))
		if !ok {
			return "", true
		}
		if !strings.EqualFold(string(name), "host") {
			continue
		}
		return normalizeHost(string(bytes.TrimSpace(value))), true
	}
	return "", true
}

// requestLine reports whether l looks like "METHOD target HTTP/1.x".
func requestLine(l []byte) bool {
	f := bytes.Split(l, []byte(" "))
	if len(f) != 3 || len(f[0]) == 0 || len(f[1]) == 0 || !tokenPrefix(f[0]) {
		return false
	}
	if string(f[0]) == "PRI" {
		return false
	}
	return bytes.HasPrefix(f[2], []byte("HTTP/1."))
}

// tokenPrefix reports whether b starts with an uppercase method token.
func tokenPrefix(b []byte) bool {
	for i, c := range b {
		if c == ' ' && i > 0 {
			return true
		}
		if c < 'A' || c > 'Z' {
			return false
		}
		if i > 16 {
			return false
		}
	}
	return true
}

// normalizeHost lowercases h and strips a port and a trailing dot.
func normalizeHost(h string) string {
	h = strings.ToLower(h)
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i > 0 {
			return h[1:i]
		}
		return ""
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}
