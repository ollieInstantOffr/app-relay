package model

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func testCAPEM(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestTLSSettingsCustomACME(t *testing.T) {
	base := TLSSettings{ACMEProvider: ACMEProviderCustom, PreferredChallenge: ChallengeHTTP01, RenewDaysBefore: 30, CipherProfile: "intermediate",
		ACMEDirectoryURL: "https://ca.internal:9000/acme/acme/directory", ACMECABundle: testCAPEM(t),
		EABKid: "kid-1", EABHMACKey: "zWNDZM6eQGHWpSRTPal5eIUYFTu7EajVIoguysqZ9wG44nMEtx3MUAsUDkMTQ12W"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid custom settings: %v", err)
	}

	bad := base
	bad.ACMEDirectoryURL = "http://ca.internal/acme"
	bad.ACMECABundle = "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----"
	bad.EABHMACKey = ""
	err := bad.Validate()
	for _, f := range []string{"acmeDirectoryUrl", "acmeCaBundle", "eabHmacKey"} {
		fieldErr(t, err, f)
	}
	bad = base
	bad.EABKid, bad.EABHMACKey = "", "not*base64"
	err = bad.Validate()
	fieldErr(t, err, "eabKid")
	fieldErr(t, err, "eabHmacKey")

	// Let's Encrypt ignores leftover custom fields
	le := base
	le.ACMEProvider, le.EABHMACKey, le.ACMEDirectoryURL = CertLetsEncrypt, "", ""
	if err := le.Validate(); err != nil {
		t.Fatalf("LE with leftover custom fields: %v", err)
	}
	unknown := base
	unknown.ACMEProvider = "zerossl"
	fieldErr(t, unknown.Validate(), "acmeProvider")

	if k, err := DecodeEABHMAC("YWJjZA=="); err != nil || string(k) != "abcd" {
		t.Fatalf("padded std base64: %q %v", k, err)
	}
}

func TestTLSSettingsSecrets(t *testing.T) {
	stored := TLSSettings{ACMEProvider: ACMEProviderCustom, EABKid: "kid-1", EABHMACKey: "zWNDZM6eQGHWpSRTPal5eIUYFTu7EajV"}
	shown := stored
	shown.Redact()
	if shown.EABHMACKey == stored.EABHMACKey || !IsMaskedSecret(shown.EABHMACKey) || shown.EABKid != "kid-1" {
		t.Fatalf("redacted: %+v", shown)
	}
	// resending the mask keeps the key
	next := shown
	next.ACMEDirectoryURL = "  https://ca.internal/dir  "
	if err := next.KeepSecrets(&stored); err != nil {
		t.Fatal(err)
	}
	if next.EABHMACKey != stored.EABHMACKey || next.ACMEDirectoryURL != "https://ca.internal/dir" {
		t.Fatalf("keep secrets: %+v", next)
	}
	// a different key ID must not inherit the old HMAC key
	other := shown
	other.EABKid = "kid-2"
	_ = other.KeepSecrets(&stored)
	if other.EABHMACKey != "" {
		t.Fatalf("HMAC key leaked to a new key ID: %q", other.EABHMACKey)
	}
}

// withOtherOptions fills the "other" provider options like the acme package does.
func withOtherOptions(t *testing.T) {
	t.Helper()
	for i := range DNSProviderTypes {
		if DNSProviderTypes[i].Type == DNSProviderOther {
			saved := DNSProviderTypes[i].Fields[0].Options
			DNSProviderTypes[i].Fields[0].Options = []DNSFieldOption{{Value: "gandi", Label: "Gandi", EnvPrefix: "GANDI_"}}
			t.Cleanup(func() { DNSProviderTypes[i].Fields[0].Options = saved })
		}
	}
}

func TestOtherDNSProviderSecretsAndValidation(t *testing.T) {
	withOtherOptions(t)
	prev := &DNSProvider{Name: "gandi", Type: DNSProviderOther, Credentials: map[string]string{"provider": "gandi", "GANDI_API_KEY": "super-secret-key-1234"}}
	shown := *prev
	shown.Credentials = map[string]string{"provider": "gandi", "GANDI_API_KEY": "super-secret-key-1234"}
	shown.Redact()
	if shown.Credentials["provider"] != "gandi" || !IsMaskedSecret(shown.Credentials["GANDI_API_KEY"]) {
		t.Fatalf("redact other: %v", shown.Credentials)
	}

	next := &DNSProvider{Name: "gandi", Type: DNSProviderOther, Credentials: map[string]string{
		"provider": "gandi", "GANDI_API_KEY": shown.Credentials["GANDI_API_KEY"], "gandi_ttl": " 600 ", "GANDI_HTTP_TIMEOUT": "",
	}}
	if err := next.KeepSecrets(prev); err != nil {
		t.Fatal(err)
	}
	if next.Credentials["GANDI_API_KEY"] != "super-secret-key-1234" || next.Credentials["GANDI_TTL"] != "600" {
		t.Fatalf("keep secrets other: %v", next.Credentials)
	}
	if _, ok := next.Credentials["GANDI_HTTP_TIMEOUT"]; ok {
		t.Fatal("empty variables must be dropped")
	}
	if err := next.Validate(); err != nil {
		t.Fatalf("valid other provider: %v", err)
	}

	for key, want := range map[string]string{
		"LEGO_CA_CERTIFICATES": "credentials.LEGO_CA_CERTIFICATES",
		"GANDI_API_KEY_FILE":   "credentials.GANDI_API_KEY_FILE",
		"PATH":                 "credentials.PATH",
	} {
		p := &DNSProvider{Name: "x", Type: DNSProviderOther, Credentials: map[string]string{"provider": "gandi", key: "v"}}
		fieldErr(t, p.Validate(), want)
	}
	fieldErr(t, (&DNSProvider{Name: "x", Type: DNSProviderOther, Credentials: map[string]string{"provider": "unknown", "UNKNOWN_KEY": "v"}}).Validate(), "credentials.provider")
	fieldErr(t, (&DNSProvider{Name: "x", Type: DNSProviderOther, Credentials: map[string]string{"provider": "gandi"}}).Validate(), "credentials")
}

func TestNewDNSProviderTypeValidation(t *testing.T) {
	cases := []struct {
		typ   string
		creds map[string]string
		field string
	}{
		{"gandiv5", map[string]string{}, "credentials.personalAccessToken"},
		{"namecheap", map[string]string{"apiUser": "u", "apiKey": "k", "clientIp": "not-an-ip"}, "credentials.clientIp"},
		{"netcup", map[string]string{"customerNumber": "abc", "apiKey": "k", "apiPassword": "p"}, "credentials.customerNumber"},
		{"hurricane", map[string]string{"tokens": "example.com"}, "credentials.tokens"},
		{"pdns", map[string]string{"apiUrl": "pdns:8081", "apiKey": "k"}, "credentials.apiUrl"},
		{"httpreq", map[string]string{"endpoint": "https://hook.internal", "mode": "BOGUS"}, "credentials.mode"},
		{"exec", map[string]string{"program": "hook.sh --flag"}, "credentials.program"},
		{"cloudns", map[string]string{"authPassword": "p"}, "credentials.authId"},
	}
	for _, c := range cases {
		p := &DNSProvider{Name: c.typ, Type: c.typ, Credentials: c.creds}
		fieldErr(t, p.Validate(), c.field)
	}
	msg := fieldErr(t, (&DNSProvider{Name: "h", Type: "hetzner", Credentials: map[string]string{}}).Validate(), "credentials.apiToken")
	if msg != "Enter API token (Hetzner Console) or API key (legacy DNS Console)" {
		t.Errorf("require-one-of message: %q", msg)
	}
	if pt, _ := DNSProviderTypeByName("exec"); !pt.AdminOnly {
		t.Error("exec must be admin-only")
	}
	tokens, err := ParseDomainTokens(" Example.com:k1 , b.example.org:k2 ")
	if err != nil || tokens["example.com"] != "k1" || tokens["b.example.org"] != "k2" {
		t.Fatalf("ParseDomainTokens: %v %v", tokens, err)
	}
	if !IsACMEProvider(CertACME) || IsACMEProvider(CertCustom) {
		t.Error("IsACMEProvider")
	}
}
