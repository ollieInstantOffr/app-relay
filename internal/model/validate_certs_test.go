package model

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func fieldErr(t *testing.T, err error, field string) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected validation error for %s, got %v", field, err)
	}
	msg, ok := ve.Fields[field]
	if !ok {
		t.Fatalf("expected error on %q, got %v", field, ve.Fields)
	}
	return msg
}

func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"short":                      "••••",
		"cf-token-abcdef12345678c1f": "••••8c1f",
	}
	for in, want := range cases {
		if got := MaskSecret(in); got != want {
			t.Errorf("MaskSecret(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsMaskedSecret("••••8c1f") || IsMaskedSecret("abc") {
		t.Error("IsMaskedSecret")
	}
}

func TestDNSProviderSecrets(t *testing.T) {
	prev := &DNSProvider{Name: "cf", Type: "cloudflare", Credentials: map[string]string{"apiToken": "secret-token-0000008c1f", "zoneToken": "zone-token-000000000abcd"}}

	next := &DNSProvider{Name: " cf ", Type: "cloudflare", Credentials: map[string]string{"apiToken": "••••8c1f", "zoneToken": "", "bogus": "x"}}
	if err := next.KeepSecrets(prev); err != nil {
		t.Fatal(err)
	}
	if next.Credentials["apiToken"] != "secret-token-0000008c1f" {
		t.Errorf("masked required secret not kept: %v", next.Credentials)
	}
	if _, ok := next.Credentials["zoneToken"]; ok {
		t.Errorf("cleared optional secret should be removed: %v", next.Credentials)
	}
	if _, ok := next.Credentials["bogus"]; ok {
		t.Error("unknown keys must be dropped")
	}
	if next.Name != "cf" {
		t.Errorf("name not trimmed: %q", next.Name)
	}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}

	// changing type never carries secrets over
	other := &DNSProvider{Name: "do", Type: "digitalocean", Credentials: map[string]string{"authToken": "••••8c1f"}}
	_ = other.KeepSecrets(prev)
	fieldErr(t, other.Validate(), "credentials.authToken")

	red := &DNSProvider{Type: "route53", Credentials: map[string]string{"accessKeyId": "AKIAEXAMPLE", "secretAccessKey": "abcdefghijklmnop1234", "region": "eu-west-1"}}
	red.Redact()
	if red.Credentials["secretAccessKey"] != "••••1234" || red.Credentials["region"] != "eu-west-1" || red.Credentials["accessKeyId"] != "AKIAEXAMPLE" {
		t.Errorf("redact: %v", red.Credentials)
	}

	h := &DNSProvider{Name: "hz", Type: "hetzner", Credentials: map[string]string{}}
	_ = h.KeepSecrets(nil)
	fieldErr(t, h.Validate(), "credentials.apiToken")
}

func TestCertRequestFields(t *testing.T) {
	if e := ValidateCertRequestFields([]string{"*.home.lan", "home.lan"}, ChallengeDNS01, "p1"); len(e) != 0 {
		t.Errorf("valid wildcard request rejected: %v", e)
	}
	e := ValidateCertRequestFields([]string{"*.home.lan"}, ChallengeHTTP01, "")
	if !strings.Contains(e["challenge"], "DNS-01") {
		t.Errorf("wildcard + HTTP-01 must require DNS-01: %v", e)
	}
	e = ValidateCertRequestFields([]string{"a.example.com"}, ChallengeDNS01, "")
	if e["dnsProviderId"] == "" {
		t.Errorf("DNS-01 needs a provider: %v", e)
	}
	e = ValidateCertRequestFields([]string{"a.example.com"}, ChallengeTLSALPN01, "")
	if !strings.Contains(e["challenge"], "unavailable") {
		t.Errorf("TLS-ALPN must be unavailable: %v", e)
	}
	e = ValidateCertRequestFields([]string{"bad_domain.com", "a.*.com", "1.2.3.4", "localhost"}, ChallengeHTTP01, "")
	for _, k := range []string{"domains.0", "domains.1", "domains.2", "domains.3"} {
		if e[k] == "" {
			t.Errorf("expected error on %s: %v", k, e)
		}
	}
	e = ValidateCertRequestFields(nil, ChallengeHTTP01, "")
	if e["domains"] == "" {
		t.Error("empty domains accepted")
	}
}

func TestCertificateValidate(t *testing.T) {
	c := &Certificate{Name: "x", Provider: CertCustom, Status: CertStatusValid, Domains: []string{"10.0.0.1"}}
	if err := c.Validate(); err != nil {
		t.Errorf("custom certificates skip ACME checks: %v", err)
	}
	c = &Certificate{Name: "x", Provider: CertLetsEncrypt, Status: "bogus", Domains: []string{"a.example.com"}, Challenge: ChallengeHTTP01}
	fieldErr(t, c.Validate(), "status")
}

func TestAccessListSecrets(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	prev := &AccessList{Name: "family", BasicAuth: BasicAuth{Enabled: true, Users: []BasicAuthUser{{Username: "jonas", PasswordHash: string(hash)}}}}
	next := &AccessList{
		Name:  "family",
		Rules: []IPRule{{Action: "allow", CIDR: "192.168.0.0/16"}, {Action: "deny", CIDR: "all"}},
		BasicAuth: BasicAuth{Enabled: true, Users: []BasicAuthUser{
			{Username: "jonas", PasswordHash: "$2y$client-supplied"},
			{Username: "mira", Password: "brisk-otter-4471"},
		}},
	}
	if err := next.KeepSecrets(prev); err != nil {
		t.Fatal(err)
	}
	if next.BasicAuth.Users[0].PasswordHash != string(hash) {
		t.Error("existing hash not kept / client hash trusted")
	}
	u := next.BasicAuth.Users[1]
	if u.Password != "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("brisk-otter-4471")) != nil {
		t.Error("new password not bcrypt-hashed")
	}
	for _, r := range next.Rules {
		if r.ID == "" {
			t.Error("rule id not assigned")
		}
	}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	next.Redact()
	for _, u := range next.BasicAuth.Users {
		if u.PasswordHash != "" {
			t.Error("redact must clear hashes")
		}
	}

	short := &AccessList{Name: "x", BasicAuth: BasicAuth{Users: []BasicAuthUser{{Username: "a", Password: "123"}}}}
	fieldErr(t, short.KeepSecrets(nil), "basicAuth.users.0.password")

	newUser := &AccessList{Name: "x", BasicAuth: BasicAuth{Enabled: true, Users: []BasicAuthUser{{Username: "nopass"}}}}
	_ = newUser.KeepSecrets(nil)
	fieldErr(t, newUser.Validate(), "basicAuth.users.0.password")
}

func TestAccessListRules(t *testing.T) {
	l := &AccessList{Name: "lan-only", Rules: []IPRule{{ID: "a", Action: "allow", CIDR: "192.168.1.5/24"}}}
	msg := fieldErr(t, l.Validate(), "rules.0.cidr")
	if !strings.Contains(msg, "192.168.1.0/24") {
		t.Errorf("host-bits message should suggest the network: %q", msg)
	}
	for _, ok := range []string{"all", "10.0.0.1", "10.0.0.0/8", "2001:db8::/32", "::1"} {
		if _, _, err := ParseAccessRule(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"10.0.0.300", "10.0.0.0/33", "any", ""} {
		if _, _, err := ParseAccessRule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	l = &AccessList{Name: "bad name!", Rules: []IPRule{{ID: "a", Action: "permit", CIDR: "all"}}, SatisfyAny: true}
	err := l.Validate()
	fieldErr(t, err, "name")
	fieldErr(t, err, "rules.0.action")
	fieldErr(t, err, "satisfyAny")
}

func TestParseStreamPorts(t *testing.T) {
	good := map[string][2]int{"25565": {25565, 25565}, "2456-2458": {2456, 2458}, " 53 ": {53, 53}, "1000-1099": {1000, 1099}}
	for in, want := range good {
		lo, hi, err := ParseStreamPorts(in)
		if err != nil || lo != want[0] || hi != want[1] {
			t.Errorf("ParseStreamPorts(%q) = %d,%d,%v", in, lo, hi, err)
		}
	}
	for _, bad := range []string{"", "0", "65536", "10-5", "1000-1100", "abc", "80-", "-80"} {
		if _, _, err := ParseStreamPorts(bad); err == nil {
			t.Errorf("ParseStreamPorts(%q) accepted", bad)
		}
	}
}

func TestStreamValidate(t *testing.T) {
	s := &Stream{Name: "Valheim", Protocol: "udp", ListenAddress: "0.0.0.0", ListenPorts: "2456-2458", ForwardHost: "10.0.0.61", ForwardPorts: "", IdleTimeout: "10m"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.ForwardPorts = "3000-3001"
	fieldErr(t, s.Validate(), "forwardPorts")
	s.ForwardPorts = "3000-3002"
	s.IdleTimeout = "10 minutes"
	fieldErr(t, s.Validate(), "idleTimeout")
	s.IdleTimeout = "1h30m"
	s.ListenAddress = "all"
	fieldErr(t, s.Validate(), "listenAddress")
	s.ListenAddress = "::"
	s.ForwardHost = "not a host"
	fieldErr(t, s.Validate(), "forwardHost")
	s.ForwardHost = "valheim-server"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.BackendID = "b1"
	fieldErr(t, s.Validate(), "protocol")
}

func TestTLSSettingsValidate(t *testing.T) {
	ts := TLSSettings{ACMEProvider: CertLetsEncrypt, Email: "jonas@example.com", PreferredChallenge: ChallengeDNS01, RenewDaysBefore: 30, CipherProfile: "intermediate", HSTS: HSTSSettings{Enabled: true, MaxAgeSeconds: 15768000, IncludeSubdomains: true}}
	if err := ts.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := ts
	bad.RenewDaysBefore = 61
	bad.Email = "not-an-email"
	bad.PreferredChallenge = ChallengeTLSALPN01
	bad.HSTS.Preload = true
	err := bad.Validate()
	for _, f := range []string{"renewDaysBefore", "email", "preferredChallenge", "hsts.preload"} {
		fieldErr(t, err, f)
	}
}
