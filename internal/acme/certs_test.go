package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func mkCert(t *testing.T, cn string, parent *testCA, isCA bool, key crypto.Signer, notAfter time.Time, dns ...string) *testCA {
	t.Helper()
	if key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		DNSNames:              dns,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	parentCert, parentKey := tpl, key
	if parent != nil {
		parentCert, parentKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parentCert, key.Public(), parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: c, key: key}
}

func pemOf(certs ...*testCA) []byte {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})...)
	}
	return out
}

func TestInspectPEMChainOrder(t *testing.T) {
	year := time.Now().Add(365 * 24 * time.Hour)
	root := mkCert(t, "ISRG Root X1", nil, true, nil, year)
	inter := mkCert(t, "R11", root, true, nil, year)
	leaf := mkCert(t, "*.home.lan", inter, false, nil, time.Now().Add(88*24*time.Hour), "*.home.lan", "home.lan")

	// intermediate first: ordering must put the leaf first
	info, err := InspectPEM(pemOf(inter, leaf))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(info.Chain, " → "); got != "*.home.lan → R11 → ISRG Root X1" {
		t.Errorf("chain = %s", got)
	}
	if info.Issuer != "R11" || info.KeyType != "ECDSA P-256" {
		t.Errorf("issuer %q key %q", info.Issuer, info.KeyType)
	}
	if len(info.Fingerprint) != 95 || strings.Count(info.Fingerprint, ":") != 31 || strings.ToUpper(info.Fingerprint) != info.Fingerprint {
		t.Errorf("fingerprint format: %s", info.Fingerprint)
	}
	if strings.Join(info.Domains, ",") != "*.home.lan,home.lan" {
		t.Errorf("domains %v", info.Domains)
	}

	// full bundle including the self-signed root does not repeat the root
	info, err = InspectPEM(pemOf(leaf, inter, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Chain) != 3 {
		t.Errorf("chain with root: %v", info.Chain)
	}

	if _, err := InspectPEM([]byte("garbage")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestValidateUpload(t *testing.T) {
	year := time.Now().Add(365 * 24 * time.Hour)
	ca := mkCert(t, "step-ca", nil, true, nil, year)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	leaf := mkCert(t, "internal.lan", ca, false, rsaKey, year, "internal.lan")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})

	up, err := ValidateUpload(pemOf(leaf, ca), keyPEM, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if up.Info.KeyType != "RSA 2048" || !strings.Contains(string(up.Key), "BEGIN PRIVATE KEY") {
		t.Errorf("normalised upload: %s %q", up.Info.KeyType, up.Key[:30])
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherPEM, _ := encodePrivateKey(other)
	_, err = ValidateUpload(pemOf(leaf), otherPEM, time.Now())
	var ve *model.ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Fields["privateKey"], "does not match") {
		t.Errorf("mismatched key: %v", err)
	}

	_, err = ValidateUpload(pemOf(leaf), keyPEM, year.Add(48*time.Hour))
	if !errors.As(err, &ve) || !strings.Contains(ve.Fields["certificate"], "expired") {
		t.Errorf("expired: %v", err)
	}

	_, err = ValidateUpload(keyPEM, keyPEM, time.Now())
	if !errors.As(err, &ve) || !strings.Contains(ve.Fields["certificate"], "private key") {
		t.Errorf("key in cert field: %v", err)
	}
}

func TestSelfSignedAndAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	chain, key, err := selfSigned("localhost", 365*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cp, kp := filepath.Join(dir, "_default", "fullchain.pem"), filepath.Join(dir, "_default", "privkey.pem")
	if err := writeCertFiles(cp, kp, chain, key); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{cp: 0o644, kp: 0o640} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", path, st.Mode().Perm(), mode)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(cp))
	if len(entries) != 2 {
		t.Errorf("temp files left behind: %v", entries)
	}
	data, _ := os.ReadFile(cp)
	info, err := InspectPEM(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Leaf.Subject.CommonName != "localhost" || info.KeyType != "ECDSA P-256" {
		t.Errorf("self-signed: %+v", info.Chain)
	}
	k, err := parsePrivateKey(key)
	if err != nil || !keyMatches(info.Leaf, k) {
		t.Errorf("self-signed key mismatch: %v", err)
	}
}

func TestHumanizeACMEError(t *testing.T) {
	err := errors.New("error: one or more domains had a problem:\nvault.home.lan: propagation: time limit exceeded: last error: NS ns1.example.net. did not return the expected TXT record")
	got := humanizeACMEError(model.ChallengeDNS01, []string{"vault.home.lan"}, err, 120*time.Second)
	if got != "DNS-01 for vault.home.lan: TXT record not visible after 120 s" {
		t.Errorf("dns timeout: %q", got)
	}
	err = errors.New("error: one or more domains had a problem:\n[git.home.lan] acme: error: 403 :: urn:ietf:params:acme:error:unauthorized :: 203.0.113.7: Invalid response from http://git.home.lan/.well-known/acme-challenge/x: 404\n")
	got = humanizeACMEError(model.ChallengeHTTP01, []string{"a.lan"}, err, 0)
	if got != "HTTP-01 for git.home.lan: 203.0.113.7: Invalid response from http://git.home.lan/.well-known/acme-challenge/x: 404" {
		t.Errorf("http unauthorized: %q", got)
	}
	err = errors.New("acme: error: 429 :: POST :: https://acme-v02.api.letsencrypt.org/acme/new-order :: urn:ietf:params:acme:error:rateLimited :: too many certificates (50) already issued for \"home.lan\"")
	got = humanizeACMEError(model.ChallengeDNS01, []string{"x.home.lan"}, err, 0)
	if !strings.HasPrefix(got, "Let's Encrypt rate limit reached: too many certificates") {
		t.Errorf("rate limit: %q", got)
	}
	got = humanizeACMEError(model.ChallengeDNS01, []string{"x.home.lan"}, errors.New("cloudflare: failed to find zone lan.: zone could not be found"), 0)
	if got != "DNS-01 for x.home.lan: cloudflare: failed to find zone lan.: zone could not be found" {
		t.Errorf("provider error: %q", got)
	}
}

func TestRateLimitCounting(t *testing.T) {
	now := time.Now()
	recs := []issuanceRecord{
		{At: now.Add(-time.Hour), Domains: []string{"*.home.lan", "home.lan"}},
		{At: now.Add(-6 * 24 * time.Hour), Domains: []string{"cloud.home.lan"}},
		{At: now.Add(-8 * 24 * time.Hour), Domains: []string{"old.home.lan"}},
		{At: now.Add(-time.Hour), Domains: []string{"a.example.co.uk"}},
	}
	if n := countIssuances(recs, "vault.home.lan", now); n != 2 {
		t.Errorf("home.lan count = %d", n)
	}
	if n := countIssuances(recs, "b.example.co.uk", now); n != 1 {
		t.Errorf("co.uk count = %d", n)
	}
	if registeredDomain("*.Home.LAN.") != "home.lan" {
		t.Errorf("registeredDomain = %s", registeredDomain("*.Home.LAN."))
	}
}

func TestRetryBackoff(t *testing.T) {
	if retryDelay(0) != 0 || retryDelay(1) != time.Hour || retryDelay(3) != 4*time.Hour || retryDelay(9) != 24*time.Hour {
		t.Error("retryDelay")
	}
	t0 := time.Now()
	h := []model.CertEvent{{Result: "ok", At: t0}, {Result: "retried", At: t0.Add(time.Hour)}, {Result: "failed", At: t0.Add(2 * time.Hour)}}
	n, last := consecutiveFailures(h)
	if n != 2 || !last.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("consecutiveFailures = %d %v", n, last)
	}
}

func TestDNSProviderTests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/cf/user/tokens/verify" && auth == "Bearer good":
			fmt.Fprint(w, `{"success":true,"errors":[],"result":{"status":"active"}}`)
		case r.URL.Path == "/cf/zones" && auth == "Bearer good":
			fmt.Fprint(w, `{"success":true,"errors":[],"result":[{"name":"home.lan"},{"name":"example.com"}]}`)
		case strings.HasPrefix(r.URL.Path, "/cf/"):
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`)
		case r.URL.Path == "/do/v2/domains" && auth == "Bearer good":
			fmt.Fprint(w, `{"domains":[{"name":"example.org"}]}`)
		case r.URL.Path == "/do/v2/domains":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"id":"Unauthorized","message":"Unable to authenticate you"}`)
		case r.URL.Path == "/duck/update":
			if r.URL.Query().Get("token") == "good" {
				fmt.Fprint(w, "OK\n")
			} else {
				fmt.Fprint(w, "KO")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cloudflareAPI, digitaloceanAPI, duckdnsAPI = srv.URL+"/cf", srv.URL+"/do", srv.URL+"/duck"
	ctx := t.Context()

	res := testDNSProvider(ctx, &model.DNSProvider{Type: "cloudflare", Credentials: map[string]string{"apiToken": "good"}})
	if res.Status != "ok" || strings.Join(res.Zones, ",") != "example.com,home.lan" {
		t.Errorf("cloudflare ok: %+v", res)
	}
	res = testDNSProvider(ctx, &model.DNSProvider{Type: "cloudflare", Credentials: map[string]string{"apiToken": "bad"}})
	if res.Status != "failed" || res.Error != "Invalid API Token · credentials rejected" {
		t.Errorf("cloudflare bad: %+v", res)
	}
	res = testDNSProvider(ctx, &model.DNSProvider{Type: "digitalocean", Credentials: map[string]string{"authToken": "bad"}})
	if res.Status != "failed" || !strings.Contains(res.Error, "credentials rejected") {
		t.Errorf("digitalocean bad: %+v", res)
	}
	res = testDNSProvider(ctx, &model.DNSProvider{Type: "duckdns", Credentials: map[string]string{"token": "good", "domain": "myhome"}})
	if res.Status != "ok" || res.Zones[0] != "myhome.duckdns.org" {
		t.Errorf("duckdns: %+v", res)
	}
	res = testDNSProvider(ctx, &model.DNSProvider{Type: "duckdns", Credentials: map[string]string{"token": "good"}})
	if res.Status != "unknown" {
		t.Errorf("duckdns without domain: %+v", res)
	}
}

func TestLegoProvidersFromCredentials(t *testing.T) {
	for _, p := range []model.DNSProvider{
		{Type: "cloudflare", Credentials: map[string]string{"apiToken": "t"}},
		{Type: "digitalocean", Credentials: map[string]string{"authToken": "t"}},
		{Type: "hetzner", Credentials: map[string]string{"apiToken": "t"}},
		{Type: "duckdns", Credentials: map[string]string{"token": "t"}},
		{Type: "route53", Credentials: map[string]string{"accessKeyId": "AKIA", "secretAccessKey": "s", "region": "eu-west-1"}},
	} {
		prov, timeout, err := legoDNSProvider(&p)
		if err != nil || prov == nil || timeout <= 0 {
			t.Errorf("%s: %v %v", p.Type, err, timeout)
		}
	}
}
