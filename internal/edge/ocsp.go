package edge

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/ocsp"
)

// OCSP stapling (nginx ssl_stapling on + ssl_stapling_verify on): responses
// are fetched in the background, verified against the issuer and attached to
// the served certificate. Any failure simply leaves the handshake without a
// staple, like nginx.

type ocspState struct {
	next   time.Time // next fetch attempt
	expiry time.Time // current staple's NextUpdate
}

const (
	ocspRetry   = 5 * time.Minute
	ocspMaxWait = 24 * time.Hour
)

var errNoOCSP = errors.New("no OCSP responder or issuer")

// staple refreshes e's OCSP response when due.
func (e *certEntry) staple(ctx context.Context, client *http.Client, now time.Time) error {
	if !e.ref.OCSPStapling {
		return nil
	}
	e.mu.Lock()
	base, st := e.base, e.ocsp
	e.mu.Unlock()
	if now.Before(st.next) {
		return nil
	}
	raw, resp, err := fetchOCSP(ctx, client, base)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.base != base { // renewed meanwhile; the next poll fetches for the new one
		return nil
	}
	if err != nil {
		e.ocsp.next = now.Add(ocspRetry)
		if errors.Is(err, errNoOCSP) {
			e.ocsp.next = now.Add(ocspMaxWait)
		}
		if !e.ocsp.expiry.IsZero() && now.After(e.ocsp.expiry) {
			e.ocsp.expiry = time.Time{}
			e.cur.Store(base)
		}
		return err
	}
	c := *base
	c.OCSPStaple = raw
	e.cur.Store(&c)
	e.ocsp.expiry = resp.NextUpdate
	next := now.Add(time.Hour)
	if !resp.NextUpdate.IsZero() {
		next = resp.ThisUpdate.Add(resp.NextUpdate.Sub(resp.ThisUpdate) / 2)
	}
	if lo := now.Add(ocspRetry); next.Before(lo) {
		next = lo
	}
	if hi := now.Add(ocspMaxWait); next.After(hi) {
		next = hi
	}
	e.ocsp.next = next
	return nil
}

func fetchOCSP(ctx context.Context, client *http.Client, c *tls.Certificate) ([]byte, *ocsp.Response, error) {
	if c == nil || c.Leaf == nil || len(c.Leaf.OCSPServer) == 0 || len(c.Certificate) < 2 {
		return nil, nil, errNoOCSP
	}
	issuer, err := x509.ParseCertificate(c.Certificate[1])
	if err != nil {
		return nil, nil, errNoOCSP
	}
	req, err := ocsp.CreateRequest(c.Leaf, issuer, &ocsp.RequestOptions{Hash: crypto.SHA1})
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Leaf.OCSPServer[0], bytes.NewReader(req))
	if err != nil {
		return nil, nil, err
	}
	hreq.Header.Set("Content-Type", "application/ocsp-request")
	hreq.Header.Set("Accept", "application/ocsp-response")
	res, err := client.Do(hreq)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("OCSP responder %s: HTTP %d", c.Leaf.OCSPServer[0], res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	resp, err := ocsp.ParseResponseForCert(raw, c.Leaf, issuer)
	if err != nil {
		return nil, nil, err
	}
	if resp.Status != ocsp.Good {
		return nil, nil, fmt.Errorf("OCSP status %d for certificate serial %s", resp.Status, c.Leaf.SerialNumber)
	}
	return raw, resp, nil
}
