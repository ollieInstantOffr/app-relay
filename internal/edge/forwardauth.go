package edge

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// forwardAuthRT is nginx auth_request against VerifyURL.
type forwardAuthRT struct {
	verify     *url.URL
	signIn     string // SignInURL plus "?rd=" or "&rd="
	passUser   bool
	passGroups bool
	transport  *upstreamTransport
	timeout    time.Duration
}

// authResult is the verify endpoint's answer. For 401/403 the headers and a
// bounded body are kept to pass them to the client.
type authResult struct {
	status int
	header http.Header
	body   []byte
}

const maxAuthBody = 64 << 10

// hopHeaders are never forwarded (RFC 9110 §7.6.1, plus body framing that
// does not apply to a body-less subrequest).
var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Content-Length": true,
}

// check sends the subrequest: GET, no body, the client's headers, plus the
// X-Original-* / X-Forwarded-* headers of the nginx renderer.
func (fa *forwardAuthRT) check(rs *reqState) *authResult {
	r := rs.r
	ctx, cancel := context.WithTimeout(r.Context(), fa.timeout)
	defer cancel()
	hdr := make(http.Header, len(r.Header)+10)
	for k, vs := range r.Header {
		if hopHeaders[k] || strings.IndexByte(k, '_') >= 0 {
			continue
		}
		hdr[k] = vs
	}
	requestURI := rs.requestURI()
	hdr.Set("X-Original-URL", rs.role.scheme+"://"+r.Host+requestURI)
	hdr.Set("X-Original-Method", r.Method)
	hdr.Set("X-Forwarded-Method", r.Method)
	hdr.Set("X-Forwarded-Proto", rs.role.scheme)
	hdr.Set("X-Forwarded-Host", r.Host)
	hdr.Set("X-Forwarded-Uri", requestURI)
	hdr.Set("X-Forwarded-For", forwardedFor(r.Header, rs.remoteIP))
	hdr.Set("X-Real-IP", rs.remoteIP)
	req := (&http.Request{
		Method: http.MethodGet, URL: fa.verify, Header: hdr, Host: fa.verify.Host,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Body: http.NoBody,
	}).WithContext(ctx)

	res, err := fa.transport.tr.RoundTrip(req)
	if err != nil {
		if r.Context().Err() == nil {
			rs.srv.errlog.logf(levelError, "auth request to %s failed: %v, client: %s, host: %q", fa.verify.Redacted(), err, rs.remoteIP, rs.host)
		}
		return &authResult{status: http.StatusInternalServerError}
	}
	defer res.Body.Close()
	out := &authResult{status: res.StatusCode}
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		if fa.passUser {
			rs.remoteUser = res.Header.Get("Remote-User")
			rs.remoteEmail = res.Header.Get("Remote-Email")
			rs.remoteName = res.Header.Get("Remote-Name")
		}
		if fa.passGroups {
			rs.remoteGroups = res.Header.Get("Remote-Groups")
		}
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		out.header = make(http.Header, len(res.Header))
		for k, vs := range res.Header {
			if !hopHeaders[k] {
				out.header[k] = vs
			}
		}
		out.body, _ = io.ReadAll(io.LimitReader(res.Body, maxAuthBody))
	default:
		rs.srv.errlog.logf(levelError, "auth request unexpected status: %d, client: %s, host: %q", res.StatusCode, rs.remoteIP, rs.host)
		out.status = http.StatusInternalServerError
	}
	return out
}
