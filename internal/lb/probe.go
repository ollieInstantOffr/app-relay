package lb

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

// ProbeResult is the outcome of a "Run health check" request.
type ProbeResult struct {
	OK         bool      `json:"ok"`
	Status     string    `json:"status"` // UP | DOWN
	Check      string    `json:"check"`  // L4OK, L7STS, …
	Detail     string    `json:"detail"`
	LatencyMs  int64     `json:"latencyMs"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
}

// proxyV2Local is a PROXY protocol v2 header with the LOCAL command (what
// haproxy itself sends for health checks to send-proxy-v2 servers).
var proxyV2Local = []byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00")

// probeServer checks one server directly from Relay, mirroring the backend's
// health check type.
func probeServer(ctx context.Context, b *model.Backend, srv model.Server) ProbeResult {
	timeout := 5 * time.Second
	if d, err := parseHAProxyDuration(b.Timeouts.Connect); err == nil && d > 0 && d < timeout {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	addr := net.JoinHostPort(strings.Trim(srv.Address, "[]"), strconv.Itoa(srv.Port))
	start := time.Now()
	res := ProbeResult{CheckedAt: start}
	finish := func(ok bool, check, detail string) ProbeResult {
		res.OK, res.Check = ok, check
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Status = "DOWN"
		if ok {
			res.Status = "UP"
		}
		res.Detail = fmt.Sprintf("%s · %s · %d ms", check, detail, res.LatencyMs)
		return res
	}

	dial := func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		if b.SendProxy {
			if _, err := c.Write(proxyV2Local); err != nil {
				c.Close()
				return nil, err
			}
		}
		return c, nil
	}

	switch b.HealthCheck.Type {
	case "http":
		scheme := "http"
		if b.TLSReencrypt {
			scheme = "https"
		}
		tr := &http.Transport{
			DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !b.TLSVerify, ServerName: b.HealthCheck.Host}, //nolint:gosec // mirrors "ssl verify none"
			ResponseHeaderTimeout: timeout,
			DisableKeepAlives:     true,
		}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		method := b.HealthCheck.Method
		if method == "" {
			method = http.MethodGet
		}
		path := b.HealthCheck.Path
		if path == "" {
			path = "/"
		}
		req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+addr+path, nil)
		if err != nil {
			return finish(false, "L7RSP", err.Error())
		}
		if h := b.HealthCheck.Host; h != "" {
			req.Host = h
		}
		req.Header.Set("User-Agent", "Relay-Health-Check")
		resp, err := client.Do(req)
		if err != nil {
			check, msg := classifyNetErr(err, true)
			return finish(false, check, msg)
		}
		resp.Body.Close()
		res.HTTPStatus = resp.StatusCode
		if statusMatches(b.HealthCheck.ExpectStatus, resp.StatusCode) {
			return finish(true, "L7OK", resp.Status)
		}
		want := b.HealthCheck.ExpectStatus
		if want == "" {
			want = "2xx"
		}
		return finish(false, "L7STS", fmt.Sprintf("%s (expected %s)", resp.Status, want))
	case "redis":
		c, err := dial(ctx)
		if err != nil {
			check, msg := classifyNetErr(err, false)
			return finish(false, check, msg)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(timeout))
		if _, err := c.Write([]byte("PING\r\n")); err != nil {
			return finish(false, "L7RSP", err.Error())
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			check, msg := classifyNetErr(err, true)
			return finish(false, check, msg)
		}
		if strings.HasPrefix(line, "+PONG") {
			return finish(true, "L7OK", "PONG")
		}
		return finish(false, "L7RSP", "unexpected reply "+strconv.Quote(strings.TrimSpace(line)))
	default: // tcp, pgsql, mysql, none
		c, err := dial(ctx)
		if err != nil {
			check, msg := classifyNetErr(err, false)
			return finish(false, check, msg)
		}
		c.Close()
		if b.TLSReencrypt && b.HealthCheck.Type == "tcp" {
			tc, err := dial(ctx)
			if err == nil {
				t := tls.Client(tc, &tls.Config{InsecureSkipVerify: !b.TLSVerify}) //nolint:gosec
				t.SetDeadline(time.Now().Add(timeout))
				err = t.HandshakeContext(ctx)
				t.Close()
			}
			if err != nil {
				return finish(false, "L6RSP", "TLS handshake failed: "+err.Error())
			}
			return finish(true, "L6OK", "TLS handshake ok")
		}
		return finish(true, "L4OK", "connected")
	}
}

func classifyNetErr(err error, l7 bool) (string, string) {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "L4CON", "connection refused"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		if l7 {
			return "L7TOUT", "timeout"
		}
		return "L4TOUT", "timeout"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "L4CON", "host unreachable"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "L4CON", "cannot resolve " + dnsErr.Name
	}
	if l7 {
		return "L7RSP", err.Error()
	}
	return "L4CON", err.Error()
}

var expectListRe = regexp.MustCompile(`^[0-9,\-]+$`)

// statusMatches implements the ExpectStatus syntax: "", "200", "2xx", "200-399", "200,204".
func statusMatches(expect string, code int) bool {
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

// parseHAProxyDuration parses haproxy time values (default unit ms).
func parseHAProxyDuration(v string) (time.Duration, error) {
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
