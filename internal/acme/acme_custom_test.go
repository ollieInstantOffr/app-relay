package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-acme/lego/v4/lego"

	"github.com/instantoffr/relay/internal/model"
)

func TestServerForProviders(t *testing.T) {
	tls := model.TLSSettings{Email: "a@example.com", ACMEProvider: model.ACMEProviderCustom, ACMEDirectoryURL: "https://ca.internal/acme/dir", ACMECABundle: "PEM", EABKid: "kid", EABHMACKey: "key"}
	if got := certProviderFor(tls); got != model.CertACME {
		t.Fatalf("certProviderFor(custom) = %s", got)
	}
	srv, err := serverFor(model.CertACME, tls)
	if err != nil || srv.Directory != tls.ACMEDirectoryURL || srv.CABundle != "PEM" || srv.EABKid != "kid" || srv.EABHMAC != "key" || srv.Email != tls.Email {
		t.Fatalf("custom server: %+v %v", srv, err)
	}
	le, _ := serverFor(model.CertLetsEncrypt, tls)
	st, _ := serverFor(model.CertLetsEncryptStaging, tls)
	if le.Directory != lego.LEDirectoryProduction || st.Directory != lego.LEDirectoryStaging || le.EABKid != "" || le.CABundle != "" {
		t.Fatalf("LE servers must not inherit custom settings: %+v %+v", le, st)
	}
	tls.ACMEDirectoryURL = ""
	if _, err := serverFor(model.CertACME, tls); err == nil || !strings.Contains(err.Error(), "Settings → Default TLS") {
		t.Fatalf("unconfigured custom server: %v", err)
	}
	if _, err := serverFor(model.CertCustom, tls); err == nil {
		t.Fatal("uploaded certificates are not ACME")
	}
	for in, want := range map[string]string{model.CertLetsEncrypt: model.CertLetsEncrypt, model.CertLetsEncryptStaging: model.CertLetsEncryptStaging, "": model.CertLetsEncrypt} {
		if got := certProviderFor(model.TLSSettings{ACMEProvider: in}); got != want {
			t.Errorf("certProviderFor(%q) = %s", in, got)
		}
	}
}

func TestHumanizeUnresolvableDomain(t *testing.T) {
	// Pebble
	err := fmt.Errorf("error: one or more domains had a problem:\n[nores.acme.test] acme: error: 400 :: urn:ietf:params:acme:error:connection :: Get \"http://nores.acme.test:80/.well-known/acme-challenge/x\": could not resolve URL \"http://nores.acme.test:80/.well-known/acme-challenge/x\"\n")
	want := "HTTP-01 for nores.acme.test: nores.acme.test does not resolve for the ACME server — check its DNS A/AAAA record points at this machine"
	if got := humanizeACMEError(model.ChallengeHTTP01, []string{"nores.acme.test"}, err, 0); got != want {
		t.Errorf("pebble: %q", got)
	}
	// Let's Encrypt
	err = fmt.Errorf("error: one or more domains had a problem:\n[nx.example.com] acme: error: 400 :: urn:ietf:params:acme:error:dns :: DNS problem: NXDOMAIN looking up A for nx.example.com - check that a DNS record exists for this domain\n")
	if got := humanizeACMEError(model.ChallengeHTTP01, []string{"nx.example.com"}, err, 0); !strings.HasPrefix(got, "HTTP-01 for nx.example.com: nx.example.com does not resolve") {
		t.Errorf("letsencrypt: %q", got)
	}
	err = fmt.Errorf("acme: error: 403 :: urn:ietf:params:acme:error:externalAccountRequired :: External account binding required")
	if got := humanizeACMEError(model.ChallengeHTTP01, []string{"a.example"}, err, 0); !strings.Contains(got, "External Account Binding") {
		t.Errorf("eab: %q", got)
	}
}

func TestDNSResolversEnv(t *testing.T) {
	t.Setenv(EnvDNSResolvers, "")
	if r, custom := dnsResolvers(); custom || len(r) != 3 {
		t.Fatalf("default resolvers: %v %v", r, custom)
	}
	t.Setenv(EnvDNSResolvers, "10.0.0.53, 10.0.0.54:5353")
	r, custom := dnsResolvers()
	if !custom || strings.Join(r, ",") != "10.0.0.53:53,10.0.0.54:5353" {
		t.Fatalf("custom resolvers: %v", r)
	}
	if n := len(dnsChallengeOptions()); n != 3 {
		t.Fatalf("custom resolvers should add propagation options, got %d", n)
	}
}

// fakeACME is a minimal ACME server: directory, nonces and newAccount with
// optional External Account Binding verification.
type fakeACME struct {
	srv        *httptest.Server
	eabKid     string
	eabKey     []byte
	requireEAB bool
	gotKid     atomic.Value
	eabValid   atomic.Bool
}

func newFakeACME(t *testing.T, requireEAB bool) *fakeACME {
	f := &fakeACME{eabKid: "kid-1", eabKey: []byte("0123456789abcdef0123456789abcdef"), requireEAB: requireEAB}
	var nonce atomic.Int64
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce-%d", nonce.Add(1)))
		base := f.srv.URL
		switch r.URL.Path {
		case "/dir":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"newNonce": base + "/nonce", "newAccount": base + "/acct", "newOrder": base + "/order",
				"revokeCert": base + "/revoke", "keyChange": base + "/key-change",
				"meta": map[string]any{"externalAccountRequired": f.requireEAB},
			})
		case "/nonce":
			w.WriteHeader(http.StatusOK)
		case "/acct":
			var jws struct{ Protected, Payload, Signature string }
			_ = json.NewDecoder(r.Body).Decode(&jws)
			payload, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
			var acct struct {
				EAB *struct{ Protected, Payload, Signature string } `json:"externalAccountBinding"`
			}
			_ = json.Unmarshal(payload, &acct)
			if acct.EAB == nil && f.requireEAB {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"type":"urn:ietf:params:acme:error:externalAccountRequired","detail":"EAB required"}`)
				return
			}
			if acct.EAB != nil {
				ph, _ := base64.RawURLEncoding.DecodeString(acct.EAB.Protected)
				var hdr struct {
					Kid string `json:"kid"`
				}
				_ = json.Unmarshal(ph, &hdr)
				f.gotKid.Store(hdr.Kid)
				mac := hmac.New(sha256.New, f.eabKey)
				mac.Write([]byte(acct.EAB.Protected + "." + acct.EAB.Payload))
				sig, _ := base64.RawURLEncoding.DecodeString(acct.EAB.Signature)
				f.eabValid.Store(hmac.Equal(sig, mac.Sum(nil)))
			}
			w.Header().Set("Location", base+"/acct/1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"status":"valid"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeACME) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

func testUser(t *testing.T) *acmeUser {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &acmeUser{Email: "ops@example.com", key: k}
}

func TestCustomDirectoryCABundleAndEAB(t *testing.T) {
	f := newFakeACME(t, true)
	srv := acmeServer{Directory: f.srv.URL + "/dir"}

	// the directory's TLS certificate is untrusted without the CA bundle
	t.Setenv(EnvInsecureSkipVerify, "")
	if _, err := newLegoClient(srv, testUser(t), "test"); err == nil || !strings.Contains(err.Error(), "Could not reach the ACME directory") {
		t.Fatalf("expected TLS failure without bundle, got %v", err)
	}
	if _, err := acmeHTTPClient("not a pem"); err == nil {
		t.Fatal("invalid CA bundle must be rejected")
	}

	srv.CABundle = f.caPEM()
	c, err := newLegoClient(srv, testUser(t), "test")
	if err != nil {
		t.Fatalf("with CA bundle: %v", err)
	}
	if _, err := registerAccount(c, srv); err == nil || !strings.Contains(err.Error(), "External Account Binding") {
		t.Fatalf("EAB-required server without kid: %v", err)
	}

	// padded standard base64 is accepted and normalised
	srv.EABKid = f.eabKid
	srv.EABHMAC = base64.StdEncoding.EncodeToString(f.eabKey)
	reg, err := registerAccount(c, srv)
	if err != nil || reg == nil || reg.URI != f.srv.URL+"/acct/1" {
		t.Fatalf("EAB registration: %+v %v", reg, err)
	}
	if f.gotKid.Load() != f.eabKid || !f.eabValid.Load() {
		t.Fatalf("EAB binding: kid=%v valid=%v", f.gotKid.Load(), f.eabValid.Load())
	}

	srv.EABHMAC = "%%%"
	if _, err := registerAccount(c, srv); err == nil || !strings.Contains(err.Error(), "base64url") {
		t.Fatalf("bad HMAC key: %v", err)
	}
}

func TestInsecureSkipVerifyEnv(t *testing.T) {
	f := newFakeACME(t, false)
	t.Setenv(EnvInsecureSkipVerify, "1")
	c, err := newLegoClient(acmeServer{Directory: f.srv.URL + "/dir"}, testUser(t), "test")
	if err != nil {
		t.Fatalf("insecure skip verify: %v", err)
	}
	if _, err := registerAccount(c, acmeServer{}); err != nil {
		t.Fatalf("plain registration: %v", err)
	}
}

// sampleCredential returns a plausible value for a provider field so every
// provider can be constructed without network access.
func sampleCredential(key string) string {
	switch key {
	case "apiUrl", "endpoint":
		return "http://127.0.0.1:1"
	case "clientIp":
		return "203.0.113.7"
	case "tokens":
		return "example.com:key1"
	case "program":
		return "/bin/sh"
	case "customerNumber":
		return "12345"
	case "region":
		return "us-east-1"
	case "domain":
		return "myhome"
	case "mode":
		return ""
	}
	return "value-" + key
}

func TestEveryDNSProviderTypeConstructs(t *testing.T) {
	for _, pt := range model.DNSProviderTypes {
		if pt.Type == model.DNSProviderOther {
			continue
		}
		creds := map[string]string{}
		for _, f := range pt.Fields {
			if v := sampleCredential(f.Key); v != "" {
				creds[f.Key] = v
			}
		}
		p := &model.DNSProvider{Name: pt.Type, Type: pt.Type, Credentials: creds}
		if err := p.Validate(); err != nil {
			t.Errorf("%s: sample credentials invalid: %v", pt.Type, err)
		}
		prov, timeout, err := legoDNSProvider(p)
		if err != nil || prov == nil || timeout <= 0 {
			t.Errorf("%s: %v (timeout %v)", pt.Type, err, timeout)
		}
	}
}

func TestOtherProviderOptionsAndSchema(t *testing.T) {
	pt, ok := model.DNSProviderTypeByName(model.DNSProviderOther)
	if !ok || !pt.EnvPairs {
		t.Fatal("other type missing")
	}
	if len(pt.Fields[0].Options) != len(otherProviders) {
		t.Fatalf("options not filled: %d", len(pt.Fields[0].Options))
	}
	for code, p := range otherProviders {
		if _, typed := model.DNSProviderTypeByName(code); typed {
			t.Errorf("%s has a form and must not be listed under Other", code)
		}
		if prefix, ok := model.OtherProviderPrefix(code); !ok || prefix != p.prefix || !strings.HasSuffix(prefix, "_") {
			t.Errorf("%s: prefix %q", code, prefix)
		}
	}
}

func TestOtherProviderEnvIsScoped(t *testing.T) {
	t.Setenv("GANDI_API_KEY", "outer")
	os.Unsetenv("GANDI_TTL")
	prov, timeout, err := otherDNSProvider(map[string]string{"provider": "gandi", "GANDI_API_KEY": "inner", "GANDI_TTL": "600"})
	if err != nil || prov == nil || timeout <= 0 {
		t.Fatalf("gandi via env: %v", err)
	}
	if v := os.Getenv("GANDI_API_KEY"); v != "outer" {
		t.Fatalf("GANDI_API_KEY not restored: %q", v)
	}
	if _, set := os.LookupEnv("GANDI_TTL"); set {
		t.Fatal("GANDI_TTL must be unset again")
	}

	// missing required variable: lego's error names it
	os.Unsetenv("LUADNS_API_USERNAME")
	os.Unsetenv("LUADNS_API_TOKEN")
	if _, _, err := otherDNSProvider(map[string]string{"provider": "luadns", "LUADNS_API_USERNAME": "u"}); err == nil || !strings.Contains(err.Error(), "LUADNS_API_TOKEN") {
		t.Fatalf("missing variable: %v", err)
	}
	if _, set := os.LookupEnv("LUADNS_API_USERNAME"); set {
		t.Fatal("LUADNS_API_USERNAME leaked after a failed construction")
	}
	for _, creds := range []map[string]string{
		{"provider": "gandi", "LEGO_CA_CERTIFICATES": "/etc/passwd"},
		{"provider": "gandi", "GANDI_API_KEY_FILE": "/data/relay.db"},
		{"provider": "nope", "NOPE_KEY": "x"},
	} {
		if _, _, err := otherDNSProvider(creds); err == nil {
			t.Errorf("%v must be rejected", creds)
		}
	}
	res := testDNSProvider(t.Context(), &model.DNSProvider{Type: model.DNSProviderOther, Credentials: map[string]string{"provider": "gandi", "GANDI_API_KEY": "k"}})
	if res.Status != "unknown" || !strings.Contains(res.Error, "not verified") {
		t.Fatalf("other test result: %+v", res)
	}
}

// TestOtherProviderConcurrency constructs "Other" and typed providers in
// parallel: every construction must see only its own variables (run with -race).
func TestOtherProviderConcurrency(t *testing.T) {
	os.Unsetenv("LUADNS_API_USERNAME")
	os.Unsetenv("LUADNS_API_TOKEN")
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			// complete configuration: must always succeed
			if _, _, err := otherDNSProvider(map[string]string{"provider": "luadns", "LUADNS_API_USERNAME": fmt.Sprint("u", i), "LUADNS_API_TOKEN": "t"}); err != nil {
				errs <- fmt.Errorf("complete luadns %d: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			// incomplete configuration: must always fail, even while another
			// goroutine has LUADNS_API_TOKEN set
			if _, _, err := otherDNSProvider(map[string]string{"provider": "luadns", "LUADNS_API_USERNAME": fmt.Sprint("x", i)}); err == nil {
				errs <- fmt.Errorf("incomplete luadns %d saw another provider's token", i)
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, _, err := legoDNSProvider(&model.DNSProvider{Type: "dynu", Credentials: map[string]string{"apiKey": "k"}}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if _, set := os.LookupEnv("LUADNS_API_TOKEN"); set {
		t.Fatal("LUADNS_API_TOKEN leaked")
	}
}

func TestNewProviderCredentialTests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/gandi/domains" && r.Header.Get("Authorization") == "Bearer good":
			fmt.Fprint(w, `[{"fqdn":"example.com"},{"fqdn":"home.lan"}]`)
		case r.URL.Path == "/gandi/domains":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":401,"message":"The server could not verify that you are authorized"}`)
		case r.URL.Path == "/godaddy/v1/domains" && r.Header.Get("Authorization") == "sso-key k:good":
			fmt.Fprint(w, `[{"domain":"example.org"}]`)
		case r.URL.Path == "/godaddy/v1/domains":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":"ACCESS_DENIED","message":"Authenticated user is not allowed access"}`)
		case r.URL.Path == "/dynu/dns" && r.Header.Get("API-Key") == "good":
			fmt.Fprint(w, `{"statusCode":200,"domains":[{"name":"dyn.example"}]}`)
		case r.URL.Path == "/dynu/dns":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"statusCode":401,"message":"Invalid API key"}`)
		case r.URL.Path == "/cloudns/dns/login.json" && q.Get("auth-password") == "good":
			fmt.Fprint(w, `{"status":"Success","statusDescription":"Success login."}`)
		case r.URL.Path == "/cloudns/dns/login.json":
			fmt.Fprint(w, `{"status":"Failed","statusDescription":"Invalid authentication, incorrect auth-id or auth-password."}`)
		case r.URL.Path == "/cloudns/dns/list-zones.json":
			fmt.Fprint(w, `[{"name":"cloud.example","type":"master"}]`)
		case r.URL.Path == "/pdns/api/v1/servers/localhost/zones" && r.Header.Get("X-API-Key") == "good":
			fmt.Fprint(w, `[{"name":"internal.lan."}]`)
		case strings.HasPrefix(r.URL.Path, "/pdns/"):
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"Unauthorized"}`)
		case r.URL.Path == "/netcup":
			var body struct {
				Action string            `json:"action"`
				Param  map[string]string `json:"param"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Action == "login" && body.Param["apipassword"] == "good" {
				fmt.Fprint(w, `{"status":"success","responsedata":{"apisessionid":"s1"}}`)
			} else if body.Action == "logout" {
				fmt.Fprint(w, `{"status":"success"}`)
			} else {
				fmt.Fprint(w, `{"status":"error","shortmessage":"Api key missing","longmessage":"The API login failed."}`)
			}
		case r.URL.Path == "/namecheap" && q.Get("ApiKey") == "good" && q.Get("ClientIp") == "203.0.113.7":
			fmt.Fprint(w, `<?xml version="1.0"?><ApiResponse Status="OK"><CommandResponse><DomainGetListResult><Domain Name="nc.example"/></DomainGetListResult></CommandResponse></ApiResponse>`)
		case r.URL.Path == "/namecheap":
			fmt.Fprint(w, `<?xml version="1.0"?><ApiResponse Status="ERROR"><Errors><Error Number="1011150">Invalid request IP: 198.51.100.1</Error></Errors></ApiResponse>`)
		case r.URL.Path == "/getip":
			fmt.Fprint(w, "198.51.100.1")
		case r.URL.Path == "/hook":
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	gandiAPI, godaddyAPI, dynuAPI, cloudnsAPI = srv.URL+"/gandi", srv.URL+"/godaddy", srv.URL+"/dynu", srv.URL+"/cloudns"
	netcupAPI, namecheapAPI, namecheapIPAPI = srv.URL+"/netcup", srv.URL+"/namecheap", srv.URL+"/getip"
	ctx := t.Context()
	check := func(typ string, creds map[string]string, status, contains string) {
		t.Helper()
		res := testDNSProvider(ctx, &model.DNSProvider{Type: typ, Credentials: creds})
		if res.Status != status || !strings.Contains(strings.Join(res.Zones, ",")+" "+res.Error, contains) {
			t.Errorf("%s %v: %+v (want %s containing %q)", typ, creds, res, status, contains)
		}
	}
	check("gandiv5", map[string]string{"personalAccessToken": "good"}, "ok", "example.com,home.lan")
	check("gandiv5", map[string]string{"personalAccessToken": "bad"}, "failed", "credentials rejected")
	check("godaddy", map[string]string{"apiKey": "k", "apiSecret": "good"}, "ok", "example.org")
	check("godaddy", map[string]string{"apiKey": "k", "apiSecret": "bad"}, "failed", "10+ domains")
	check("dynu", map[string]string{"apiKey": "good"}, "ok", "dyn.example")
	check("dynu", map[string]string{"apiKey": "bad"}, "failed", "Invalid API key · credentials rejected")
	check("cloudns", map[string]string{"authId": "1", "authPassword": "good"}, "ok", "cloud.example")
	check("cloudns", map[string]string{"authId": "1", "authPassword": "bad"}, "failed", "incorrect auth-id")
	check("pdns", map[string]string{"apiUrl": srv.URL + "/pdns/", "apiKey": "good"}, "ok", "internal.lan")
	check("pdns", map[string]string{"apiUrl": srv.URL + "/pdns", "apiKey": "bad"}, "failed", "credentials rejected")
	check("netcup", map[string]string{"customerNumber": "1", "apiKey": "k", "apiPassword": "good"}, "ok", "")
	check("netcup", map[string]string{"customerNumber": "1", "apiKey": "k", "apiPassword": "bad"}, "failed", "The API login failed")
	check("namecheap", map[string]string{"apiUser": "u", "apiKey": "good", "clientIp": "203.0.113.7"}, "ok", "nc.example")
	check("namecheap", map[string]string{"apiUser": "u", "apiKey": "good"}, "failed", "whitelist 198.51.100.1")
	check("hurricane", map[string]string{"tokens": "example.com:k1,b.example.org:k2"}, "unknown", "b.example.org,example.com")
	check("httpreq", map[string]string{"endpoint": srv.URL + "/hook"}, "unknown", "HTTP 405")
	check("exec", map[string]string{"program": "/bin/sh"}, "unknown", "not verified")
	check("exec", map[string]string{"program": "/nonexistent/hook"}, "failed", "does not exist")
}
