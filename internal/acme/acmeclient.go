package acme

// ACME server selection (Let's Encrypt or a custom directory such as step-ca,
// ZeroSSL or Pebble), the HTTP client used to reach it and DNS-01 propagation
// resolvers.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"

	"github.com/instantoffr/relay/internal/model"
)

// EnvInsecureSkipVerify=1 disables TLS verification of the ACME directory.
// TEST-ONLY (e2e stacks against Pebble); never set it in production — use
// the "CA bundle" field of a custom ACME server instead.
const EnvInsecureSkipVerify = "RELAY_ACME_INSECURE_SKIP_VERIFY"

// EnvDNSResolvers overrides the resolvers used to check DNS-01 propagation,
// e.g. "10.0.0.53:53,10.0.0.54". When set, propagation is checked through
// these resolvers only (not the zone's authoritative nameservers), which suits
// split-horizon/internal DNS and test stacks.
const EnvDNSResolvers = "RELAY_ACME_DNS_RESOLVERS"

var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

// acmeServer is the ACME directory and account parameters for an order.
type acmeServer struct {
	Directory string
	Email     string
	CABundle  string // extra PEM roots for the directory's TLS certificate
	EABKid    string
	EABHMAC   string // base64url HMAC key
}

// certProviderFor maps the TLS settings' ACME provider to the provider stored
// on newly requested certificates.
func certProviderFor(t model.TLSSettings) string {
	switch t.ACMEProvider {
	case model.CertLetsEncryptStaging:
		return model.CertLetsEncryptStaging
	case model.ACMEProviderCustom:
		return model.CertACME
	}
	return model.CertLetsEncrypt
}

// serverFor returns the ACME server issuing and renewing certificates of
// provider. Custom ACME certificates use the directory currently configured in
// the TLS settings.
func serverFor(provider string, t model.TLSSettings) (acmeServer, error) {
	srv := acmeServer{Email: t.Email}
	switch provider {
	case model.CertLetsEncrypt:
		srv.Directory = lego.LEDirectoryProduction
	case model.CertLetsEncryptStaging:
		srv.Directory = lego.LEDirectoryStaging
	case model.CertACME:
		srv.Directory = strings.TrimSpace(t.ACMEDirectoryURL)
		if srv.Directory == "" {
			return srv, errors.New("no custom ACME server is configured — set its directory URL in Settings → Default TLS")
		}
		srv.CABundle = t.ACMECABundle
		srv.EABKid, srv.EABHMAC = t.EABKid, t.EABHMACKey
	default:
		return srv, fmt.Errorf("%s certificates are not issued via ACME", providerLabel(provider))
	}
	return srv, nil
}

// directoryHost is the host[:port] of a directory URL ("ca.internal:9000").
func directoryHost(dir string) string {
	if u, err := url.Parse(dir); err == nil && u.Host != "" {
		return u.Host
	}
	return dir
}

// acmeHTTPClient mirrors lego's default client, trusting the system roots plus
// caBundle (and nothing is read from LEGO_* environment variables).
func acmeHTTPClient(caBundle string) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if strings.TrimSpace(caBundle) != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(caBundle)) {
			return nil, errors.New("the custom ACME CA bundle contains no PEM certificates")
		}
		tlsCfg.RootCAs = pool
	}
	if os.Getenv(EnvInsecureSkipVerify) == "1" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // test-only escape hatch (documented)
	}
	return &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSClientConfig:       tlsCfg,
		},
	}, nil
}

// dnsResolvers returns the DNS-01 propagation resolvers and whether they were
// overridden with EnvDNSResolvers.
func dnsResolvers() ([]string, bool) {
	raw := strings.TrimSpace(os.Getenv(EnvDNSResolvers))
	if raw == "" {
		return publicResolvers, false
	}
	var out []string
	for _, r := range strings.FieldsFunc(raw, func(c rune) bool { return c == ',' || c == ' ' }) {
		if _, _, err := net.SplitHostPort(r); err != nil {
			r = net.JoinHostPort(r, "53")
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return publicResolvers, false
	}
	return out, true
}

// dnsChallengeOptions configures DNS-01 propagation checks.
func dnsChallengeOptions() []dns01.ChallengeOption {
	resolvers, custom := dnsResolvers()
	opts := []dns01.ChallengeOption{dns01.AddRecursiveNameservers(resolvers)}
	if custom {
		opts = append(opts, dns01.DisableAuthoritativeNssPropagationRequirement(), dns01.RecursiveNSsPropagationRequirement())
	}
	return opts
}
