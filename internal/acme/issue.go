package acme

// ACME accounts, issuance with lego, revocation and error humanisation.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/http/webroot"
	"github.com/go-acme/lego/v4/registration"
	"golang.org/x/net/publicsuffix"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func providerLabel(provider string) string {
	switch provider {
	case model.CertLetsEncrypt:
		return "Let's Encrypt"
	case model.CertLetsEncryptStaging:
		return "Let's Encrypt staging"
	case model.CertACME:
		return "Custom ACME"
	case model.CertCustom:
		return "Custom"
	case model.CertSelfSigned:
		return "Self-signed"
	}
	return provider
}

func challengeLabel(ch string) string {
	switch ch {
	case model.ChallengeDNS01:
		return "DNS-01"
	case model.ChallengeHTTP01:
		return "HTTP-01"
	case model.ChallengeTLSALPN01:
		return "TLS-ALPN-01"
	}
	return strings.ToUpper(ch)
}

// ---------------------------------------------------------------- accounts

type acmeUser struct {
	Email        string                 `json:"email"`
	Directory    string                 `json:"directory"`
	KeyPEM       string                 `json:"keyPem"`
	Registration *registration.Resource `json:"registration,omitempty"`
	key          crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.Email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.Registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

func accountKVKey(dir, email string) string {
	sum := sha256.Sum256([]byte(dir + "\n" + strings.ToLower(email)))
	return "acme:account:" + hex.EncodeToString(sum[:8])
}

var accountMu sync.Mutex

// loadAccount returns the stored account for kvKey, or nil when absent.
func (s *Service) loadAccount(ctx context.Context, kvKey string) (*acmeUser, error) {
	raw, err := s.app.Store.GetKV(ctx, kvKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u acmeUser
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("stored ACME account is corrupt: %w", err)
	}
	k, err := parsePrivateKey([]byte(u.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("stored ACME account key: %w", err)
	}
	u.key = k
	return &u, nil
}

func (s *Service) saveAccount(ctx context.Context, kvKey string, u *acmeUser) error {
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return s.app.Store.PutKV(ctx, kvKey, raw)
}

// client returns a lego client for the ACME server (directory, email),
// creating and registering the account on first use (with External Account
// Binding when configured).
func (s *Service) client(ctx context.Context, srv acmeServer) (*lego.Client, string, error) {
	accountMu.Lock()
	defer accountMu.Unlock()
	kvKey := accountKVKey(srv.Directory, srv.Email)
	user, err := s.loadAccount(ctx, kvKey)
	if err != nil {
		return nil, "", err
	}
	if user == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, "", err
		}
		keyPEM, err := encodePrivateKey(key)
		if err != nil {
			return nil, "", err
		}
		user = &acmeUser{Email: srv.Email, Directory: srv.Directory, KeyPEM: string(keyPEM), key: key}
	}
	c, err := newLegoClient(srv, user, s.app.Config.Version)
	if err != nil {
		return nil, "", err
	}
	if user.Registration == nil {
		reg, err := registerAccount(c, srv)
		if err != nil {
			return nil, "", err
		}
		user.Registration = reg
		if err := s.saveAccount(ctx, kvKey, user); err != nil {
			return nil, "", err
		}
	}
	return c, kvKey, nil
}

// newLegoClient builds a lego client for srv (fetches the directory).
func newLegoClient(srv acmeServer, user registration.User, version string) (*lego.Client, error) {
	cfg := lego.NewConfig(user)
	cfg.CADirURL = srv.Directory
	cfg.UserAgent = "relay/" + version
	cfg.Certificate.KeyType = certcrypto.EC256
	cfg.Certificate.Timeout = 60 * time.Second
	hc, err := acmeHTTPClient(srv.CABundle)
	if err != nil {
		return nil, err
	}
	cfg.HTTPClient = hc
	c, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("Could not reach the ACME directory %s: %w", srv.Directory, err)
	}
	return c, nil
}

// registerAccount registers a new ACME account, with External Account
// Binding when a key ID is configured.
func registerAccount(c *lego.Client, srv acmeServer) (*registration.Resource, error) {
	var reg *registration.Resource
	var err error
	switch {
	case srv.EABKid != "":
		key, derr := model.DecodeEABHMAC(srv.EABHMAC)
		if derr != nil || len(key) == 0 {
			return nil, errors.New("the EAB HMAC key is not valid base64url")
		}
		reg, err = c.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
			TermsOfServiceAgreed: true, Kid: srv.EABKid, HmacEncoded: base64.RawURLEncoding.EncodeToString(key),
		})
	case c.GetExternalAccountRequired():
		return nil, errors.New("the ACME server requires External Account Binding — add the EAB key ID and HMAC key in Settings → Default TLS")
	default:
		reg, err = c.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	}
	if err != nil {
		return nil, fmt.Errorf("ACME account registration failed: %w", err)
	}
	return reg, nil
}

// ---------------------------------------------------------------- issuance

type issueOutcome struct {
	Resource    *certificate.Resource
	Info        *CertInfo
	AccountKey  string
	Propagation time.Duration
	Via         string // "DNS-01 via Cloudflare"
}

// obtain runs one ACME order for cert against the ACME server srv.
func (s *Service) obtain(ctx context.Context, cert *model.Certificate, srv acmeServer) (*issueOutcome, error) {
	out := &issueOutcome{Via: challengeLabel(cert.Challenge)}
	client, kvKey, err := s.client(ctx, srv)
	if err != nil {
		return out, err
	}
	out.AccountKey = kvKey
	switch cert.Challenge {
	case model.ChallengeHTTP01:
		if err := s.httpPreflight(ctx); err != nil {
			return out, err
		}
		if err := os.MkdirAll(s.env.ACMEWebroot, 0o755); err != nil {
			return out, err
		}
		prov, err := webroot.NewHTTPProvider(s.env.ACMEWebroot)
		if err != nil {
			return out, err
		}
		if err := client.Challenge.SetHTTP01Provider(prov); err != nil {
			return out, err
		}
	case model.ChallengeDNS01:
		dp, err := s.app.Store.DNSProviders().Get(ctx, cert.DNSProviderID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return out, errors.New("the DNS provider was deleted — pick another one")
			}
			return out, err
		}
		out.Via = "DNS-01 via " + dnsTypeLabel(dp.Type)
		prov, timeout, err := legoDNSProvider(dp)
		if err != nil {
			return out, fmt.Errorf("%s credentials: %w", dnsTypeLabel(dp.Type), err)
		}
		out.Propagation = timeout
		if err := client.Challenge.SetDNS01Provider(prov, dnsChallengeOptions()...); err != nil {
			return out, err
		}
	default:
		return out, fmt.Errorf("challenge %s is not available", challengeLabel(cert.Challenge))
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: cert.Domains, Bundle: true})
	if err != nil {
		return out, err
	}
	if len(res.PrivateKey) == 0 || len(res.Certificate) == 0 {
		return out, errors.New("the ACME server returned an empty certificate")
	}
	info, err := InspectPEM(res.Certificate)
	if err != nil {
		return out, err
	}
	out.Resource = res
	out.Info = info
	return out, nil
}

// httpPreflight checks nginx is serving :80 before starting an HTTP-01 order.
func (s *Service) httpPreflight(ctx context.Context) error {
	if s.app.Nginx == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.app.Nginx.Status(cctx)
	if err != nil {
		return errors.New("nginx isn't reachable, so the ACME server can't fetch the challenge from port 80")
	}
	if !st.Running {
		return errors.New("nginx is not running, so the ACME server can't fetch the challenge from port 80")
	}
	return nil
}

func dnsTypeLabel(t string) string {
	if pt, ok := model.DNSProviderTypeByName(t); ok {
		return pt.Label
	}
	return t
}

// revoke revokes the certificate at its ACME server using the issuing account.
func (s *Service) revoke(ctx context.Context, cert *model.Certificate) error {
	fullchain, _ := s.env.CertPaths(cert.ID)
	pemBytes, err := os.ReadFile(fullchain)
	if err != nil {
		return fmt.Errorf("certificate files are missing: %w", err)
	}
	tls, err := store.LoadSettings[model.TLSSettings](ctx, s.app.Store, model.SettingsTLS)
	if err != nil {
		return err
	}
	srv, srvErr := serverFor(cert.Provider, tls)
	if raw, err := s.app.Store.GetKV(ctx, "acme:certacct:"+cert.ID); err == nil {
		if u, err := s.loadAccount(ctx, string(raw)); err == nil && u != nil {
			if u.Directory != srv.Directory {
				srv.CABundle = "" // the configured bundle belongs to another server
			}
			srv.Directory, srv.Email, srvErr = u.Directory, u.Email, nil
		}
	}
	if srvErr != nil {
		return srvErr
	}
	client, _, err := s.client(ctx, srv)
	if err != nil {
		return err
	}
	return client.Certificate.Revoke(pemBytes)
}

// ---------------------------------------------------------------- errors

var (
	domainPrefixRe = regexp.MustCompile(`^\[?([A-Za-z0-9*][A-Za-z0-9.*-]*\.[A-Za-z0-9-]+)\]?:?\s+(.*)$`)
	acmeTypeRe     = regexp.MustCompile(`urn:ietf:params:acme:error:([A-Za-z]+)`)
)

// humanizeACMEError turns a lego error into one readable line:
// "DNS-01 for vault.home.lan: TXT record not visible after 120 s".
func humanizeACMEError(challenge string, domains []string, err error, propagation time.Duration) string {
	label := challengeLabel(challenge)
	msg := strings.TrimSpace(err.Error())
	msg = strings.TrimPrefix(msg, "error: one or more domains had a problem:")
	msg = strings.TrimSpace(msg)
	line := msg
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		line = strings.TrimSpace(msg[:i])
	}
	domain := ""
	if len(domains) > 0 {
		domain = domains[0]
	}
	if m := domainPrefixRe.FindStringSubmatch(line); m != nil && !strings.HasPrefix(line, "acme:") {
		domain, line = m[1], m[2]
	}
	subject := label
	if domain != "" {
		subject = label + " for " + domain
	}
	var out string
	lower := strings.ToLower(line)
	switch {
	case challenge == model.ChallengeHTTP01 && (strings.Contains(lower, "could not resolve") || strings.Contains(lower, "nxdomain") || strings.Contains(lower, "no valid a records") || strings.Contains(lower, "no valid ip addresses")):
		out = subject + ": " + domain + " does not resolve for the ACME server — check its DNS A/AAAA record points at this machine"
	case strings.Contains(line, "time limit exceeded"):
		secs := int(propagation.Seconds())
		if challenge == model.ChallengeDNS01 {
			if secs > 0 {
				out = fmt.Sprintf("%s: TXT record not visible after %d s", subject, secs)
			} else {
				out = subject + ": TXT record not visible in time"
			}
		} else {
			out = subject + ": timed out waiting for validation"
		}
	case acmeTypeRe.MatchString(line):
		typ := acmeTypeRe.FindStringSubmatch(line)[1]
		detail := line
		if i := strings.LastIndex(line, " :: "); i >= 0 {
			detail = strings.TrimSpace(line[i+4:])
		}
		switch typ {
		case "rateLimited":
			if strings.Contains(msg, "letsencrypt.org") {
				out = "Let's Encrypt rate limit reached: " + detail
			} else {
				out = "ACME server rate limit reached: " + detail
			}
		case "rejectedIdentifier":
			out = subject + ": the ACME server won't issue for this name · " + detail
		case "externalAccountRequired":
			out = "The ACME server requires External Account Binding — add the EAB key ID and HMAC key in Settings → Default TLS"
		default:
			out = subject + ": " + detail
		}
	default:
		line = strings.TrimPrefix(line, "acme: error: ")
		line = strings.TrimPrefix(line, "acme: ")
		out = subject + ": " + line
	}
	r := []rune(out)
	if len(r) > 400 {
		out = string(r[:399]) + "…"
	}
	return out
}

// ---------------------------------------------------------------- rate limits

type issuanceRecord struct {
	At      time.Time `json:"at"`
	Domains []string  `json:"domains"`
}

const issuancesKey = "acme:issuances"

var issuancesMu sync.Mutex

func registeredDomain(d string) string {
	d = strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(d, ".")), "*.")
	if r, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return r
	}
	return d
}

func (s *Service) loadIssuances(ctx context.Context) []issuanceRecord {
	var recs []issuanceRecord
	if raw, err := s.app.Store.GetKV(ctx, issuancesKey); err == nil {
		_ = json.Unmarshal(raw, &recs)
	}
	return recs
}

// recordIssuance remembers a production issuance for rate-limit estimates.
func (s *Service) recordIssuance(ctx context.Context, domains []string, at time.Time) {
	issuancesMu.Lock()
	defer issuancesMu.Unlock()
	recs := s.loadIssuances(ctx)
	kept := recs[:0]
	for _, r := range recs {
		if at.Sub(r.At) < 8*24*time.Hour {
			kept = append(kept, r)
		}
	}
	kept = append(kept, issuanceRecord{At: at, Domains: domains})
	raw, _ := json.Marshal(kept)
	if err := s.app.Store.PutKV(ctx, issuancesKey, raw); err != nil {
		s.app.Log.Warn("acme: record issuance", "err", err)
	}
}

// countIssuances counts certificates issued in the last 7 days that contain a
// name under the same registered domain.
func countIssuances(recs []issuanceRecord, domain string, now time.Time) int {
	reg := registeredDomain(domain)
	n := 0
	for _, r := range recs {
		if now.Sub(r.At) > 7*24*time.Hour {
			continue
		}
		for _, d := range r.Domains {
			if registeredDomain(d) == reg {
				n++
				break
			}
		}
	}
	return n
}
