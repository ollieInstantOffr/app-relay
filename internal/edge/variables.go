package edge

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Header value templates with nginx variables ($host, ${http_x_y}, …),
// compiled once per config. Unknown variables are rejected like `nginx -t`.

type varKind uint8

const (
	vLiteral varKind = iota
	vCapture         // $1…$9: no regex locations here, always empty
	vHost
	vHTTPHost
	vHostname
	vRemoteAddr
	vRemotePort
	vRemoteUser
	vScheme
	vHTTPS
	vRequestURI
	vURI
	vArgs
	vIsArgs
	vServerPort
	vServerAddr
	vServerName
	vServerProtocol
	vRequestMethod
	vRequestID
	vProxyAddXFF
	vProxyHost
	vProxyPort
	vSSLProtocol
	vSSLServerName
	vMsec
	vTimeISO8601
	vTimeLocal
	vConnectionUpgrade
	vContentLength
	vContentType
	vHeader // $http_<name>
	vCookie // $cookie_<name>
	vArg    // $arg_<name>
)

var variables = map[string]varKind{
	"host": vHost, "http_host": vHTTPHost, "hostname": vHostname,
	"remote_addr": vRemoteAddr, "remote_port": vRemotePort, "remote_user": vRemoteUser,
	"scheme": vScheme, "https": vHTTPS,
	"request_uri": vRequestURI, "uri": vURI, "document_uri": vURI,
	"args": vArgs, "query_string": vArgs, "is_args": vIsArgs,
	"server_port": vServerPort, "server_addr": vServerAddr, "server_name": vServerName,
	"server_protocol": vServerProtocol, "request_method": vRequestMethod, "request_id": vRequestID,
	"proxy_add_x_forwarded_for": vProxyAddXFF, "proxy_host": vProxyHost, "proxy_port": vProxyPort,
	"ssl_protocol": vSSLProtocol, "ssl_server_name": vSSLServerName,
	"msec": vMsec, "time_iso8601": vTimeISO8601, "time_local": vTimeLocal,
	"connection_upgrade": vConnectionUpgrade,
	"content_length":     vContentLength, "content_type": vContentType,
}

type tmplPart struct {
	kind varKind
	text string // literal, or the header/cookie/argument name
}

type template struct {
	parts   []tmplPart
	literal string
	static  bool
}

func isVarChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func compileTemplate(s string) (*template, error) {
	t := &template{}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, tmplPart{kind: vLiteral, text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		if s[i] != '$' {
			lit.WriteByte(s[i])
			i++
			continue
		}
		i++
		var name string
		if i < len(s) && s[i] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("the closing bracket in %q variable is missing", s[i+1:])
			}
			name = s[i+1 : i+end]
			i += end + 1
		} else {
			j := i
			for j < len(s) && isVarChar(s[j]) {
				j++
			}
			name = s[i:j]
			i = j
		}
		if name == "" || strings.IndexFunc(name, func(r rune) bool { return r > 0x7f || !isVarChar(byte(r)) }) >= 0 {
			return nil, fmt.Errorf("invalid variable name in %q", s)
		}
		part, err := lookupVariable(name)
		if err != nil {
			return nil, err
		}
		flush()
		t.parts = append(t.parts, part)
	}
	flush()
	switch {
	case len(t.parts) == 0:
		t.static = true
	case len(t.parts) == 1 && t.parts[0].kind == vLiteral:
		t.static, t.literal = true, t.parts[0].text
	}
	return t, nil
}

func lookupVariable(name string) (tmplPart, error) {
	lower := strings.ToLower(name)
	if k, ok := variables[lower]; ok {
		return tmplPart{kind: k}, nil
	}
	if _, err := strconv.Atoi(name); err == nil {
		return tmplPart{kind: vCapture}, nil
	}
	for _, p := range []struct {
		prefix string
		kind   varKind
	}{{"http_", vHeader}, {"cookie_", vCookie}, {"arg_", vArg}} {
		if rest, ok := strings.CutPrefix(lower, p.prefix); ok && rest != "" {
			text := rest
			switch p.kind {
			case vHeader:
				text = http.CanonicalHeaderKey(strings.ReplaceAll(rest, "_", "-"))
			case vCookie, vArg:
				text = name[len(p.prefix):] // cookie and argument names keep their case
			}
			return tmplPart{kind: p.kind, text: text}, nil
		}
	}
	return tmplPart{}, fmt.Errorf("unknown %q variable", name)
}

func (t *template) expand(rs *reqState) string {
	if t.static {
		return t.literal
	}
	if len(t.parts) == 1 {
		return rs.variable(t.parts[0])
	}
	var b strings.Builder
	for _, p := range t.parts {
		b.WriteString(rs.variable(p))
	}
	return b.String()
}

var hostname = func() string {
	h, _ := os.Hostname()
	return h
}()

func (rs *reqState) variable(p tmplPart) string {
	r := rs.r
	switch p.kind {
	case vLiteral:
		return p.text
	case vHost:
		return rs.host
	case vHTTPHost:
		return r.Host
	case vHostname:
		return hostname
	case vRemoteAddr:
		return rs.remoteIP
	case vRemotePort:
		return rs.remotePort
	case vRemoteUser:
		return rs.remoteUserName()
	case vScheme:
		return rs.role.scheme
	case vHTTPS:
		if rs.role.scheme == "https" {
			return "on"
		}
	case vRequestURI:
		return rs.requestURI()
	case vURI:
		return rs.uri()
	case vArgs:
		return rs.rawQuery
	case vIsArgs:
		if rs.hasArgs {
			return "?"
		}
	case vServerPort:
		return rs.role.portStr
	case vServerAddr:
		if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			return hostAddr(a.String()).String()
		}
	case vServerName:
		if rs.vs != nil {
			return rs.vs.name
		}
	case vServerProtocol:
		return r.Proto
	case vRequestMethod:
		return r.Method
	case vRequestID:
		return rs.requestID
	case vProxyAddXFF:
		return forwardedFor(r.Header, rs.remoteIP)
	case vProxyHost:
		if rs.loc != nil {
			return rs.loc.up.proxyHost
		}
	case vProxyPort:
		if rs.loc != nil {
			return strconv.Itoa(rs.loc.up.port)
		}
	case vSSLProtocol:
		return rs.sslProtocol()
	case vSSLServerName:
		if r.TLS != nil {
			return r.TLS.ServerName
		}
	case vMsec:
		return string(appendMsec(nil, time.Now()))
	case vTimeISO8601:
		return time.Now().Format("2006-01-02T15:04:05-07:00")
	case vTimeLocal:
		return time.Now().Format("02/Jan/2006:15:04:05 -0700")
	case vConnectionUpgrade:
		if r.Header.Get("Upgrade") != "" {
			return "upgrade"
		}
		return "close"
	case vContentLength:
		return r.Header.Get("Content-Length")
	case vContentType:
		return r.Header.Get("Content-Type")
	case vHeader:
		return headerVariable(r.Header, p.text)
	case vCookie:
		if c, err := r.Cookie(p.text); err == nil {
			return c.Value
		}
	case vArg:
		return queryArg(rs.rawQuery, p.text)
	}
	return ""
}

// headerVariable joins repeated headers the way nginx does for the headers
// it knows to be lists, and returns the first value otherwise.
func headerVariable(h http.Header, name string) string {
	vs := h[name]
	switch {
	case len(vs) == 0:
		return ""
	case len(vs) == 1:
		return vs[0]
	case name == "Cookie":
		return strings.Join(vs, "; ")
	case name == "X-Forwarded-For":
		return strings.Join(vs, ", ")
	}
	return vs[0]
}

// forwardedFor is $proxy_add_x_forwarded_for.
func forwardedFor(h http.Header, remote string) string {
	vs := h["X-Forwarded-For"]
	if len(vs) == 0 {
		return remote
	}
	return strings.Join(vs, ", ") + ", " + remote
}

// queryArg returns the raw (still escaped) value of a query argument, like
// nginx $arg_<name>, matching the name case-insensitively.
func queryArg(raw, name string) string {
	for raw != "" {
		var kv string
		kv, raw, _ = strings.Cut(raw, "&")
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
