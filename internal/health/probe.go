package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

const probeTimeout = 5 * time.Second

func newClient(verify bool) *http.Client {
	dialer := &net.Dialer{Timeout: probeTimeout}
	return &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   probeTimeout,
			ResponseHeaderTimeout: probeTimeout,
			DisableKeepAlives:     true,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: !verify}, //nolint:gosec // upstreams are often self-signed; verification is per host
		},
		// Any response (including a redirect) proves the upstream is serving.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var (
	verifyClient   = newClient(true)
	insecureClient = newClient(false)
)

func upstreamAddr(host string, port int) string {
	if host == "" {
		return ""
	}
	if port <= 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// probeHTTP GETs the upstream root: any response < 500 is healthy, 5xx is
// degraded, connection errors and timeouts are down.
func probeHTTP(ctx context.Context, u model.Upstream, verify bool) core.HealthStatus {
	st := core.HealthStatus{Target: upstreamAddr(u.Host, u.Port), Status: core.HealthUnknown, CheckedAt: time.Now().UTC()}
	if u.Host == "" || u.Port <= 0 || u.Port > 65535 {
		st.Detail = "no upstream address"
		return st
	}
	scheme := "http"
	if strings.EqualFold(u.Scheme, "https") {
		scheme = "https"
	}
	path := u.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, scheme+"://"+upstreamAddr(u.Host, u.Port)+path, nil)
	if err != nil {
		st.Status = core.HealthDown
		st.Detail = "invalid upstream URL"
		return st
	}
	req.Header.Set("User-Agent", "Relay-HealthCheck/1.0")
	client := insecureClient
	if verify {
		client = verifyClient
	}
	start := time.Now()
	resp, err := client.Do(req)
	st.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		st.Status = core.HealthDown
		st.Detail = describeError(err)
		return st
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	st.HTTPStatus = resp.StatusCode
	st.Detail = strings.TrimSpace(fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	if resp.StatusCode >= 500 {
		st.Status = core.HealthDegraded
	} else {
		st.Status = core.HealthHealthy
	}
	return st
}

// probeTCP checks that a TCP connection can be established.
func probeTCP(ctx context.Context, host string, port int) core.HealthStatus {
	addr := upstreamAddr(host, port)
	st := core.HealthStatus{Target: addr, Status: core.HealthUnknown, CheckedAt: time.Now().UTC()}
	if host == "" || port <= 0 || port > 65535 {
		st.Detail = "no upstream address"
		return st
	}
	d := net.Dialer{Timeout: probeTimeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	st.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		st.Status = core.HealthDown
		st.Detail = describeError(err)
		return st
	}
	conn.Close()
	st.Status = core.HealthHealthy
	st.Detail = "TCP connect ok"
	return st
}

// describeError turns dial/HTTP errors into the short phrases used in the UI
// ("connection refused", "timed out after 5 s").
func describeError(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	var unknownAuth x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var verifyErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	msg := err.Error()
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "no such host"
		}
		return "DNS lookup failed"
	case errors.As(err, &unknownAuth), errors.As(err, &verifyErr):
		return "TLS certificate not trusted"
	case errors.As(err, &hostnameErr):
		return "TLS certificate name mismatch"
	case errors.As(err, &certInvalid):
		return "TLS certificate invalid"
	case errors.As(err, &recordErr), strings.Contains(msg, "server gave HTTP response to HTTPS client"):
		return "upstream is not HTTPS"
	case strings.Contains(msg, "malformed HTTP response"):
		return "upstream speaks TLS (use https)"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Sprintf("timed out after %d s", int(probeTimeout.Seconds()))
	case strings.Contains(msg, "tls:"):
		return "TLS handshake failed"
	case errors.Is(err, io.EOF), strings.Contains(msg, "EOF"):
		return "connection closed by upstream"
	}
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}
