// Package lbcheck runs HAProxy-compatible health checks against one server:
// tcp, http, pgsql, mysql and redis, optionally preceded by a PROXY protocol
// v2 LOCAL header (send-proxy-v2 servers) and a TLS handshake (ssl servers).
//
// Results carry HAProxy's check status codes (L4OK, L4CON, L7STS, …) and
// descriptions ("Layer4 connection problem", …) so they can be reported in
// `show stat` format. It is shared by Relay Balancer's health checker
// (internal/balancer) and the load balancer API's "Check now" probe
// (internal/lb).
package lbcheck

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Check types.
const (
	TypeTCP   = "tcp"
	TypeHTTP  = "http"
	TypePgSQL = "pgsql"
	TypeMySQL = "mysql"
	TypeRedis = "redis"
)

// HAProxy check status codes (show stat check_status).
const (
	SOCKERR = "SOCKERR"
	L4OK    = "L4OK"
	L4TOUT  = "L4TOUT"
	L4CON   = "L4CON"
	L6OK    = "L6OK"
	L6TOUT  = "L6TOUT"
	L6RSP   = "L6RSP"
	L7OK    = "L7OK"
	L7TOUT  = "L7TOUT"
	L7RSP   = "L7RSP"
	L7STS   = "L7STS"
)

// DefaultTimeout bounds a check whose Target has no Timeout.
const DefaultTimeout = 5 * time.Second

var descriptions = map[string]string{
	"UNK": "Unknown", "INI": "Initializing", SOCKERR: "Socket error",
	L4OK: "Layer4 check passed", L4TOUT: "Layer4 timeout", L4CON: "Layer4 connection problem",
	L6OK: "Layer6 check passed", L6TOUT: "Layer6 timeout", L6RSP: "Layer6 invalid response",
	L7OK: "Layer7 check passed", "L7OKC": "Layer7 check conditionally passed", L7TOUT: "Layer7 timeout",
	L7RSP: "Layer7 invalid response", L7STS: "Layer7 wrong status",
}

// Description returns HAProxy's check_desc for a status code ("" if unknown).
func Description(status string) string { return descriptions[status] }

// ProxyV2Local is a PROXY protocol v2 header with the LOCAL command, which
// HAProxy sends ahead of health checks to send-proxy-v2 servers.
var ProxyV2Local = []byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00")

// Target describes one check.
type Target struct {
	Address string // IP or host name (IPv6 with or without brackets)
	Port    int
	Type    string // tcp (default) | http | pgsql | mysql | redis

	// http: Method (GET), Path (/), Host (HTTP/1.1 with that Host header;
	// without it an HTTP/1.0 request with no Host header), Expect ("" = 2xx).
	Method, Path, Host, Expect string
	// UserAgent adds a User-Agent header to http checks ("" = none, like HAProxy).
	UserAgent string
	// User is the pgsql user ("" = "relay").
	User string

	SendProxy bool // write ProxyV2Local first
	TLS       bool // TLS handshake before the check
	// TLSVerify verifies the certificate chain against RootCAs (nil = system
	// roots), not the host name (HAProxy "verify required" without verifyhost).
	TLSVerify bool
	RootCAs   *x509.CertPool
	SNI       string // "" sends no SNI

	// Timeout bounds the whole check: connect, handshake and response
	// (0 = DefaultTimeout).
	Timeout time.Duration
	// Dial replaces net.Dialer (tests, pre-resolved addresses).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Result is the outcome of a check.
type Result struct {
	OK       bool
	Status   string // HAProxy check_status code
	Code     int    // HTTP status (http checks that got a response)
	Info     string // human detail ("connection refused", "200 OK", …)
	Duration time.Duration
}

// Description is HAProxy's check_desc for the result.
func (r Result) Description() string { return Description(r.Status) }

// Run performs one check.
func Run(ctx context.Context, t Target) Result {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, code, info := run(ctx, t, start.Add(timeout))
	return Result{OK: strings.HasSuffix(status, "OK"), Status: status, Code: code, Info: info, Duration: time.Since(start)}
}

func run(ctx context.Context, t Target, deadline time.Time) (string, int, string) {
	addr := net.JoinHostPort(strings.Trim(t.Address, "[]"), strconv.Itoa(t.Port))
	dial := t.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		status, info := classifyConnect(err)
		return status, 0, info
	}
	defer raw.Close()
	raw.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	var conn net.Conn = raw
	if t.SendProxy {
		if _, err := conn.Write(ProxyV2Local); err != nil {
			return L4CON, 0, err.Error()
		}
	}
	if t.TLS {
		tc := tls.Client(conn, TLSConfig(t.TLSVerify, t.RootCAs, t.SNI))
		if err := tc.HandshakeContext(ctx); err != nil {
			if isTimeout(err) {
				return L6TOUT, 0, "TLS handshake timeout"
			}
			return L6RSP, 0, "TLS handshake failed: " + err.Error()
		}
		conn = tc
	}
	switch t.Type {
	case TypeHTTP:
		return checkHTTP(conn, t)
	case TypeRedis:
		return checkRedis(conn)
	case TypePgSQL:
		return checkPgSQL(conn, t.User)
	case TypeMySQL:
		return checkMySQL(conn)
	}
	if t.TLS {
		return L6OK, 0, "TLS handshake ok"
	}
	return L4OK, 0, "connected"
}

// TLSConfig is the client configuration HAProxy uses for "ssl verify none"
// (verify false) and "ssl verify required ca-file …" (verify true): no ALPN,
// SNI only when sni is set, chain verification without host name check.
func TLSConfig(verify bool, roots *x509.CertPool, sni string) *tls.Config {
	cfg := &tls.Config{InsecureSkipVerify: true, ServerName: sni} //nolint:gosec // verification is done in VerifyConnection
	if verify {
		cfg.VerifyConnection = func(cs tls.ConnectionState) error { return VerifyChain(cs.PeerCertificates, roots) }
	}
	return cfg
}

// VerifyChain verifies a peer certificate chain against roots (nil = system
// roots) for server authentication, ignoring host names.
func VerifyChain(certs []*x509.Certificate, roots *x509.CertPool) error {
	if len(certs) == 0 {
		return errors.New("server presented no certificate")
	}
	opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, c := range certs[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := certs[0].Verify(opts)
	return err
}

func checkHTTP(conn net.Conn, t Target) (string, int, string) {
	method := strings.ToUpper(strings.TrimSpace(t.Method))
	if method == "" {
		method = http.MethodGet
	}
	path := t.Path
	if path == "" {
		path = "/"
	}
	var b strings.Builder
	if t.Host != "" {
		b.WriteString(method + " " + path + " HTTP/1.1\r\nHost: " + t.Host + "\r\nConnection: close\r\n")
	} else {
		b.WriteString(method + " " + path + " HTTP/1.0\r\n")
	}
	if t.UserAgent != "" {
		b.WriteString("User-Agent: " + t.UserAgent + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	resp.Body.Close()
	if StatusMatches(t.Expect, resp.StatusCode) {
		return L7OK, resp.StatusCode, resp.Status
	}
	want := strings.TrimSpace(t.Expect)
	if want == "" {
		want = "2xx"
	}
	return L7STS, resp.StatusCode, fmt.Sprintf("%s (expected %s)", resp.Status, want)
}

func checkRedis(conn net.Conn) (string, int, string) {
	if _, err := io.WriteString(conn, "PING\r\n"); err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		status, info := classifyRead(err)
		return status, 0, info
	}
	if strings.HasPrefix(line, "+PONG") {
		return L7OK, 0, "PONG"
	}
	return L7STS, 0, "unexpected reply " + strconv.Quote(strings.TrimSpace(line))
}

// checkPgSQL sends a StartupMessage and accepts any authentication request
// ('R'); an ErrorResponse ('E') fails with its message.
func checkPgSQL(conn net.Conn, user string) (string, int, string) {
	if user == "" {
		user = "relay"
	}
	body := []byte{0, 3, 0, 0} // protocol 3.0
	body = append(body, "user\x00"...)
	body = append(body, user...)
	body = append(body, 0, 0)
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	if _, err := conn.Write(append(msg, body...)); err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	br := bufio.NewReader(conn)
	typ, err := br.ReadByte()
	if err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	switch typ {
	case 'R':
		return L7OK, 0, "PostgreSQL server is ok"
	case 'E':
		var hdr [4]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return L7RSP, 0, "PostgreSQL error"
		}
		n := int(binary.BigEndian.Uint32(hdr[:])) - 4
		if n < 0 || n > 8192 {
			return L7RSP, 0, "PostgreSQL error"
		}
		payload := make([]byte, n)
		io.ReadFull(br, payload)
		for _, f := range strings.Split(string(payload), "\x00") {
			if strings.HasPrefix(f, "M") {
				return L7RSP, 0, "PostgreSQL error: " + f[1:]
			}
		}
		return L7RSP, 0, "PostgreSQL error"
	}
	return L7RSP, 0, fmt.Sprintf("unexpected PostgreSQL reply %q", typ)
}

// checkMySQL reads the server greeting (HAProxy mysql-check without a user):
// a protocol 10 handshake passes, an error packet fails with its message.
func checkMySQL(conn net.Conn) (string, int, string) {
	br := bufio.NewReader(conn)
	var hdr [4]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if n == 0 || n > 1<<16 {
		return L7RSP, 0, "invalid MySQL packet"
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		status, info := classifyRead(err)
		return status, 0, info
	}
	switch payload[0] {
	case 0x0a:
		version := string(payload[1:])
		if i := strings.IndexByte(version, 0); i >= 0 {
			version = version[:i]
		}
		return L7OK, 0, "MySQL server is ok (" + version + ")"
	case 0xff:
		if len(payload) < 3 {
			return L7RSP, 0, "MySQL error"
		}
		code := binary.LittleEndian.Uint16(payload[1:3])
		text := payload[3:]
		if len(text) >= 6 && text[0] == '#' {
			text = text[6:]
		}
		return L7RSP, 0, fmt.Sprintf("MySQL error %d: %s", code, text)
	}
	return L7RSP, 0, "unexpected MySQL greeting"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func classifyConnect(err error) (string, string) {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return L4CON, "connection refused"
	case isTimeout(err):
		return L4TOUT, "timeout"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return L4CON, "host unreachable"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return L4CON, "cannot resolve " + dnsErr.Name
	}
	return L4CON, err.Error()
}

func classifyRead(err error) (string, string) {
	switch {
	case isTimeout(err):
		return L7TOUT, "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return L7RSP, "connection closed"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return L7RSP, "connection reset"
	}
	return L7RSP, err.Error()
}

var expectRe = regexp.MustCompile(`^([1-5][0-9]{2}(-[1-5][0-9]{2})?(,[1-5][0-9]{2}(-[1-5][0-9]{2})?)*|[1-5]xx)$`)

// ValidExpect reports whether expect is "", a status ("200"), a class
// ("2xx") or a comma list of statuses and ranges ("200-299,404").
func ValidExpect(expect string) bool {
	e := strings.ToLower(strings.TrimSpace(expect))
	return e == "" || expectRe.MatchString(e)
}

var expectListRe = regexp.MustCompile(`^[0-9,\-]+$`)

// StatusMatches implements the Expect syntax: "" (2xx), "200", "2xx",
// "200-399", "200,204".
func StatusMatches(expect string, code int) bool {
	e := strings.ToLower(strings.TrimSpace(expect))
	if e == "" {
		e = "2xx"
	}
	if len(e) == 3 && strings.HasSuffix(e, "xx") {
		return strconv.Itoa(code)[:1] == e[:1]
	}
	if !expectListRe.MatchString(e) {
		return code >= 200 && code < 300
	}
	for _, part := range strings.Split(e, ",") {
		lo, hi := part, part
		if i := strings.Index(part, "-"); i > 0 {
			lo, hi = part[:i], part[i+1:]
		}
		l, err1 := strconv.Atoi(lo)
		h, err2 := strconv.Atoi(hi)
		if err1 == nil && err2 == nil && code >= l && code <= h {
			return true
		}
	}
	return false
}

// ParseDuration parses an HAProxy time value (default unit ms; us, ms, s, m,
// h, d).
func ParseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, errors.New("empty")
	}
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	n, err := strconv.ParseInt(v[:i], 10, 64)
	if err != nil {
		return 0, err
	}
	unit := map[string]time.Duration{"": time.Millisecond, "us": time.Microsecond, "ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	u, ok := unit[v[i:]]
	if !ok {
		return 0, fmt.Errorf("bad unit %q", v[i:])
	}
	return time.Duration(n) * u, nil
}
