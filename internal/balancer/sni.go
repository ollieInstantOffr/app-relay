package balancer

import "strings"

// parseClientHelloSNI inspects the first bytes of a connection. done is false
// while more data is needed to decide; once done, sni is the lowercase
// server_name of a TLS ClientHello ("" for non-TLS data or no SNI), like
// HAProxy's req.ssl_sni.
func parseClientHelloSNI(data []byte) (sni string, done bool) {
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
