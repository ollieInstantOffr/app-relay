package model

// Validation and secret handling for the certs slice: certificates, DNS
// providers, access lists, streams and TLS settings.

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------- DNS provider types

// DNSFieldOption is one choice of a select field.
type DNSFieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// EnvPrefix (type "other"): lego environment variables must start with it.
	EnvPrefix string `json:"envPrefix,omitempty"`
	DocsURL   string `json:"docsUrl,omitempty"`
}

// DNSProviderField describes one credential field of a DNS provider type.
type DNSProviderField struct {
	Key         string           `json:"key"`
	Label       string           `json:"label"`
	Secret      bool             `json:"secret"`
	Required    bool             `json:"required"`
	Placeholder string           `json:"placeholder,omitempty"`
	Hint        string           `json:"hint,omitempty"`
	Options     []DNSFieldOption `json:"options,omitempty"` // renders a select
}

// DNSProviderType is the schema the UI uses to render the provider form.
type DNSProviderType struct {
	Type    string             `json:"type"`
	Label   string             `json:"label"`
	DocsURL string             `json:"docsUrl"`
	Fields  []DNSProviderField `json:"fields"`
	// RequireOneOf lists field keys of which at least one must be set.
	RequireOneOf []string `json:"requireOneOf,omitempty"`
	// Note is shown under the form (what Test can and can't verify, caveats).
	Note string `json:"note,omitempty"`
	// AdminOnly types can only be created or changed by admins.
	AdminOnly bool `json:"adminOnly,omitempty"`
	// EnvPairs types additionally take lego environment variables as
	// credentials (keys NAME, values secret).
	EnvPairs bool `json:"envPairs,omitempty"`
}

const (
	// DNSProviderOther configures any supported lego provider by its
	// environment variables.
	DNSProviderOther = "other"
	// DNSOtherProviderKey is the credential holding the lego provider code.
	DNSOtherProviderKey = "provider"
	maxEnvPairs         = 40
)

var DNSProviderTypes = []DNSProviderType{
	{
		Type: "cloudflare", Label: "Cloudflare", DocsURL: "https://developers.cloudflare.com/fundamentals/api/get-started/create-token/",
		Fields: []DNSProviderField{
			{Key: "apiToken", Label: "API token", Secret: true, Required: true, Hint: "Permissions: Zone · DNS · Edit and Zone · Zone · Read"},
			{Key: "zoneToken", Label: "Zone token (optional)", Secret: true, Hint: "Separate Zone · Read token, if you split permissions"},
		},
	},
	{
		Type: "route53", Label: "Route 53", DocsURL: "https://go-acme.github.io/lego/dns/route53/",
		Fields: []DNSProviderField{
			{Key: "accessKeyId", Label: "Access key ID", Required: true, Placeholder: "AKIA…"},
			{Key: "secretAccessKey", Label: "Secret access key", Secret: true, Required: true},
			{Key: "region", Label: "Region", Placeholder: "us-east-1"},
			{Key: "hostedZoneId", Label: "Hosted zone ID (optional)", Placeholder: "Z0123456789ABC", Hint: "Detected from the domain when empty"},
		},
	},
	{
		Type: "digitalocean", Label: "DigitalOcean", DocsURL: "https://docs.digitalocean.com/reference/api/create-personal-access-token/",
		Fields: []DNSProviderField{
			{Key: "authToken", Label: "Personal access token", Secret: true, Required: true, Hint: "Scope: domain read + update"},
		},
	},
	{
		Type: "hetzner", Label: "Hetzner", DocsURL: "https://go-acme.github.io/lego/dns/hetzner/",
		Fields: []DNSProviderField{
			{Key: "apiToken", Label: "API token (Hetzner Console)", Secret: true, Hint: "Project → Security → API tokens · Read & Write"},
			{Key: "apiKey", Label: "API key (legacy DNS Console)", Secret: true, Hint: "Only for zones still on dns.hetzner.com"},
		},
		RequireOneOf: []string{"apiToken", "apiKey"},
	},
	{
		Type: "duckdns", Label: "DuckDNS", DocsURL: "https://www.duckdns.org/spec.jsp",
		Fields: []DNSProviderField{
			{Key: "token", Label: "Token", Secret: true, Required: true},
			{Key: "domain", Label: "Subdomain (for testing)", Placeholder: "myhome", Hint: "Used by Test to verify the token"},
		},
	},
	{
		Type: "gandiv5", Label: "Gandi LiveDNS", DocsURL: "https://docs.gandi.net/en/managing_an_organization/organizations/personal_access_token.html",
		Fields: []DNSProviderField{
			{Key: "personalAccessToken", Label: "Personal access token", Secret: true, Hint: "Scope: Domains · Manage domain name technical configurations"},
			{Key: "apiKey", Label: "API key (deprecated)", Secret: true, Hint: "Only for accounts without personal access tokens"},
		},
		RequireOneOf: []string{"personalAccessToken", "apiKey"},
		Note:         "Gandi publishes records slowly — issuance can take up to 20 minutes.",
	},
	{
		Type: "godaddy", Label: "GoDaddy", DocsURL: "https://developer.godaddy.com/keys",
		Fields: []DNSProviderField{
			{Key: "apiKey", Label: "API key", Required: true},
			{Key: "apiSecret", Label: "API secret", Secret: true, Required: true},
		},
		Note: "GoDaddy only grants DNS API access to accounts with 10 or more domains (or Discount Domain Club).",
	},
	{
		Type: "namecheap", Label: "Namecheap", DocsURL: "https://www.namecheap.com/support/api/intro/",
		Fields: []DNSProviderField{
			{Key: "apiUser", Label: "API user", Required: true, Placeholder: "username"},
			{Key: "apiKey", Label: "API key", Secret: true, Required: true},
			{Key: "clientIp", Label: "Whitelisted client IP (optional)", Placeholder: "203.0.113.7", Hint: "The public IP on the API whitelist · detected automatically when empty"},
		},
	},
	{
		Type: "dynu", Label: "Dynu", DocsURL: "https://www.dynu.com/en-US/ControlPanel/APICredentials",
		Fields: []DNSProviderField{
			{Key: "apiKey", Label: "API key", Secret: true, Required: true},
		},
	},
	{
		Type: "netcup", Label: "netcup", DocsURL: "https://helpcenter.netcup.com/en/wiki/general/our-api",
		Fields: []DNSProviderField{
			{Key: "customerNumber", Label: "Customer number", Required: true, Placeholder: "12345"},
			{Key: "apiKey", Label: "API key", Secret: true, Required: true},
			{Key: "apiPassword", Label: "API password", Secret: true, Required: true},
		},
		Note: "netcup DNS changes can take up to 15 minutes to become visible.",
	},
	{
		Type: "cloudns", Label: "ClouDNS", DocsURL: "https://www.cloudns.net/wiki/article/42/",
		Fields: []DNSProviderField{
			{Key: "authId", Label: "Auth ID", Placeholder: "1234"},
			{Key: "subAuthId", Label: "Sub auth ID", Placeholder: "5678", Hint: "Use instead of Auth ID for a sub-user"},
			{Key: "authPassword", Label: "Auth password", Secret: true, Required: true},
		},
		RequireOneOf: []string{"authId", "subAuthId"},
	},
	{
		Type: "hurricane", Label: "Hurricane Electric (he.net)", DocsURL: "https://dns.he.net/docs.html",
		Fields: []DNSProviderField{
			{Key: "tokens", Label: "Dynamic TXT keys", Secret: true, Required: true, Placeholder: "example.com:key1,home.example.org:key2", Hint: "domain:key pairs, comma-separated · one per _acme-challenge TXT record"},
		},
		Note: "Create a dynamic TXT record _acme-challenge.<domain> in dns.he.net first. Hurricane Electric has no read-only API, so Test only checks the format.",
	},
	{
		Type: "pdns", Label: "PowerDNS", DocsURL: "https://doc.powerdns.com/authoritative/http-api/",
		Fields: []DNSProviderField{
			{Key: "apiUrl", Label: "API URL", Required: true, Placeholder: "http://pdns.internal:8081"},
			{Key: "apiKey", Label: "API key", Secret: true, Required: true, Hint: "api-key from pdns.conf"},
			{Key: "serverName", Label: "Server ID (optional)", Placeholder: "localhost"},
		},
	},
	{
		Type: "httpreq", Label: "HTTP request (httpreq)", DocsURL: "https://go-acme.github.io/lego/dns/httpreq/",
		Fields: []DNSProviderField{
			{Key: "endpoint", Label: "Endpoint URL", Required: true, Placeholder: "https://dns-hook.internal/acme", Hint: "Relay POSTs JSON to <endpoint>/present and <endpoint>/cleanup"},
			{Key: "mode", Label: "Mode", Options: []DNSFieldOption{{Value: "", Label: "Default · fqdn + value"}, {Value: "RAW", Label: "RAW · domain + token + keyAuth"}}},
			{Key: "username", Label: "Basic auth user (optional)"},
			{Key: "password", Label: "Basic auth password (optional)", Secret: true},
		},
		Note: "Test only checks that the endpoint answers — it can't call present/cleanup without changing DNS.",
	},
	{
		Type: "exec", Label: "Run a program (exec)", DocsURL: "https://go-acme.github.io/lego/dns/exec/",
		Fields: []DNSProviderField{
			{Key: "program", Label: "Program", Required: true, Placeholder: "/data/hooks/dns01.sh", Hint: "Absolute path inside the relay container · called as <program> present|cleanup <fqdn> <value>"},
			{Key: "mode", Label: "Mode", Options: []DNSFieldOption{{Value: "", Label: "Default · fqdn + value"}, {Value: "RAW", Label: "RAW · domain + token + keyAuth"}}},
		},
		AdminOnly: true,
		Note:      "Admins only: the program runs inside the relay container with Relay's privileges. Test checks that it exists and is executable.",
	},
	{
		Type: DNSProviderOther, Label: "Other (any lego provider)", DocsURL: "https://go-acme.github.io/lego/dns/",
		Fields: []DNSProviderField{
			// Options are filled by the acme package from its provider registry.
			{Key: DNSOtherProviderKey, Label: "lego provider", Required: true},
		},
		EnvPairs: true,
		Note:     "Add the provider's lego environment variables (see its guide). They are only set while the provider is created and are masked like other secrets. Test checks that the configuration is complete, not that DNS changes work.",
	},
}

func DNSProviderTypeByName(t string) (DNSProviderType, bool) {
	for _, pt := range DNSProviderTypes {
		if pt.Type == t {
			return pt, true
		}
	}
	return DNSProviderType{}, false
}

const secretMask = "••••"

// MaskSecret keeps the last 4 characters of long secrets: "••••8c1f".
func MaskSecret(v string) string {
	if v == "" {
		return ""
	}
	r := []rune(v)
	if len(r) < 12 {
		return secretMask
	}
	return secretMask + string(r[len(r)-4:])
}

// IsMaskedSecret reports whether v is a value produced by MaskSecret.
func IsMaskedSecret(v string) bool { return strings.HasPrefix(v, secretMask) }

func (pt DNSProviderType) field(key string) (DNSProviderField, bool) {
	for _, f := range pt.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return DNSProviderField{}, false
}

func (d *DNSProvider) Redact() {
	pt, ok := DNSProviderTypeByName(d.Type)
	creds := map[string]string{}
	for k, v := range d.Credentials {
		f, declared := pt.field(k)
		switch {
		case !ok, !declared: // unknown type or lego environment variable
			creds[k] = MaskSecret(v)
		case f.Secret:
			creds[k] = MaskSecret(v)
		default:
			creds[k] = v
		}
	}
	d.Credentials = creds
	if d.Zones == nil {
		d.Zones = []string{}
	}
}

// KeepSecrets drops unknown credential keys, trims values and carries over
// secrets the client did not resend (empty for required fields, or masked).
// EnvPairs types keep environment variables (upper-cased keys; masked values
// are carried over, empty ones removed).
func (d *DNSProvider) KeepSecrets(prev any) error {
	p, _ := prev.(*DNSProvider)
	d.Name = strings.TrimSpace(d.Name)
	pt, ok := DNSProviderTypeByName(d.Type)
	if !ok {
		return nil // Validate reports the type
	}
	sameType := p != nil && p.Type == d.Type
	next := map[string]string{}
	for _, f := range pt.Fields {
		v := strings.TrimSpace(d.Credentials[f.Key])
		if f.Secret && (IsMaskedSecret(v) || (v == "" && f.Required)) {
			v = ""
			if sameType {
				v = p.Credentials[f.Key]
			}
		}
		if v != "" {
			next[f.Key] = v
		}
	}
	if pt.EnvPairs {
		for k, v := range d.Credentials {
			if _, declared := pt.field(k); declared {
				continue
			}
			k, v = strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v)
			if IsMaskedSecret(v) {
				v = ""
				if sameType {
					v = p.Credentials[k]
				}
			}
			if k != "" && v != "" {
				next[k] = v
			}
		}
	}
	d.Credentials = next
	if d.Zones == nil {
		d.Zones = []string{}
	}
	return nil
}

var (
	envKeyRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,79}$`)
	awsRegionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
	netcupCustRe = regexp.MustCompile(`^\d{1,12}$`)
)

// ValidLegoEnvKey checks a lego environment variable for the "other" DNS
// provider: it must belong to the provider's namespace (prefix) and must not
// be a *_FILE indirection (lego would read an arbitrary file).
func ValidLegoEnvKey(key, prefix string) error {
	switch {
	case !envKeyRe.MatchString(key):
		return errors.New("Use an environment variable name like GANDI_API_KEY")
	case prefix != "" && !strings.HasPrefix(key, prefix):
		return fmt.Errorf("Variables for this provider start with %s", prefix)
	case strings.HasSuffix(key, "_FILE"):
		return errors.New("*_FILE variables are not supported — paste the value instead")
	}
	return nil
}

// OtherProviderPrefix returns the environment prefix of a lego provider code
// offered by the "other" type ("" when unknown).
func OtherProviderPrefix(code string) (string, bool) {
	pt, _ := DNSProviderTypeByName(DNSProviderOther)
	f, _ := pt.field(DNSOtherProviderKey)
	for _, o := range f.Options {
		if o.Value == code {
			return o.EnvPrefix, true
		}
	}
	return "", false
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (d *DNSProvider) Validate() error {
	e := Errs{}
	if d.Name == "" {
		e.Add("name", "Name is required")
	} else if len(d.Name) > 64 {
		e.Add("name", "At most 64 characters")
	}
	pt, ok := DNSProviderTypeByName(d.Type)
	if !ok {
		e.Add("type", "Unsupported DNS provider %q", d.Type)
		return e.Err()
	}
	c := d.Credentials
	for _, f := range pt.Fields {
		v := c[f.Key]
		if f.Required && v == "" {
			e.Add("credentials."+f.Key, "%s is required", f.Label)
			continue
		}
		if len(v) > 8192 {
			e.Add("credentials."+f.Key, "At most 8192 characters")
		}
		if v != "" && len(f.Options) > 0 {
			found := false
			for _, o := range f.Options {
				found = found || o.Value == v
			}
			if !found {
				e.Add("credentials."+f.Key, "Pick one of the listed options")
			}
		}
	}
	if len(pt.RequireOneOf) > 0 {
		found := false
		labels := []string{}
		for _, k := range pt.RequireOneOf {
			if c[k] != "" {
				found = true
			}
			if f, ok := pt.field(k); ok {
				labels = append(labels, f.Label)
			}
		}
		if !found {
			e.Add("credentials."+pt.RequireOneOf[0], "Enter %s", strings.Join(labels, " or "))
		}
	}
	switch d.Type {
	case "route53":
		if c["region"] != "" && !awsRegionRe.MatchString(c["region"]) {
			e.Add("credentials.region", "Not an AWS region (e.g. us-east-1)")
		}
	case "duckdns":
		if c["domain"] != "" && strings.Contains(strings.TrimSuffix(c["domain"], ".duckdns.org"), ".") {
			e.Add("credentials.domain", "Enter just the subdomain, e.g. myhome")
		}
	case "namecheap":
		if c["clientIp"] != "" && net.ParseIP(c["clientIp"]) == nil {
			e.Add("credentials.clientIp", "Not a valid IP address")
		}
	case "netcup":
		if c["customerNumber"] != "" && !netcupCustRe.MatchString(c["customerNumber"]) {
			e.Add("credentials.customerNumber", "Digits only")
		}
	case "hurricane":
		if c["tokens"] != "" {
			if _, err := ParseDomainTokens(c["tokens"]); err != nil {
				e.Add("credentials.tokens", "%s", err.Error())
			}
		}
	case "pdns":
		if c["apiUrl"] != "" && !validHTTPURL(c["apiUrl"]) {
			e.Add("credentials.apiUrl", "Use an http:// or https:// URL")
		}
	case "httpreq":
		if c["endpoint"] != "" && !validHTTPURL(c["endpoint"]) {
			e.Add("credentials.endpoint", "Use an http:// or https:// URL")
		}
	case "exec":
		if p := c["program"]; p != "" && (!strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\n")) {
			e.Add("credentials.program", "Use an absolute path without arguments, e.g. /data/hooks/dns01.sh")
		}
	case DNSProviderOther:
		code := c[DNSOtherProviderKey]
		prefix, known := OtherProviderPrefix(code)
		if code != "" && !known {
			e.Add("credentials."+DNSOtherProviderKey, "Pick a provider from the list")
		}
		n := 0
		for k, v := range c {
			if k == DNSOtherProviderKey {
				continue
			}
			n++
			if err := ValidLegoEnvKey(k, prefix); err != nil {
				e.Add("credentials."+k, "%s", err.Error())
			} else if len(v) > 8192 {
				e.Add("credentials."+k, "At most 8192 characters")
			}
		}
		if n == 0 && code != "" {
			e.Add("credentials", "Add the provider's environment variables, e.g. %sAPI_KEY", firstNonEmptyStr(prefix, "PROVIDER_"))
		}
		if n > maxEnvPairs {
			e.Add("credentials", "At most %d variables", maxEnvPairs)
		}
	}
	switch d.Status {
	case "", "ok", "failed", "unknown":
	default:
		e.Add("status", "invalid status")
	}
	return e.Err()
}

// ---------------------------------------------------------------- certificates

var certLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidCertDomain reports whether d is a hostname acceptable in an ACME order
// (optionally a leading "*." wildcard).
func ValidCertDomain(d string) bool {
	d = strings.TrimSuffix(strings.ToLower(d), ".")
	d = strings.TrimPrefix(d, "*.")
	if d == "" || len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if !certLabelRe.MatchString(l) {
			return false
		}
	}
	// the TLD must not be all digits (that would be an IP address)
	parts := strings.Split(d, ".")
	if _, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
		return false
	}
	return true
}

func IsWildcardDomain(d string) bool { return strings.HasPrefix(d, "*.") }

const (
	// ACMEProviderCustom (TLSSettings.ACMEProvider) issues from a custom ACME
	// directory (step-ca, ZeroSSL, an internal CA).
	ACMEProviderCustom = "custom"
	// CertACME is the provider of certificates issued by a custom ACME server.
	CertACME = "acme"
)

// IsACMEProvider reports whether a certificate provider is issued via ACME.
func IsACMEProvider(p string) bool {
	return p == CertLetsEncrypt || p == CertLetsEncryptStaging || p == CertACME
}

// ParseDomainTokens parses "example.com:key1,home.example.org:key2"
// (Hurricane Electric dynamic TXT keys).
func ParseDomainTokens(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		d, tok, ok := strings.Cut(pair, ":")
		d, tok = strings.TrimSpace(d), strings.TrimSpace(tok)
		if !ok || tok == "" || !ValidCertDomain(d) || IsWildcardDomain(d) {
			return nil, fmt.Errorf("Use domain:key pairs, e.g. example.com:%s", "key1")
		}
		out[strings.ToLower(d)] = tok
	}
	if len(out) == 0 {
		return nil, errors.New("Add at least one domain:key pair")
	}
	return out, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *Certificate) Validate() error {
	e := Errs{}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		e.Add("name", "Name is required")
	} else if len(c.Name) > 253 {
		e.Add("name", "At most 253 characters")
	}
	switch c.Provider {
	case CertLetsEncrypt, CertLetsEncryptStaging, CertACME, CertCustom, CertSelfSigned:
	default:
		e.Add("provider", "Unknown provider %q", c.Provider)
	}
	switch c.Status {
	case CertStatusValid, CertStatusPending, CertStatusFailed, CertStatusExpired:
	default:
		e.Add("status", "Unknown status %q", c.Status)
	}
	if IsACMEProvider(c.Provider) {
		mergeCertErrs(e, ValidateCertRequestFields(c.Domains, c.Challenge, c.DNSProviderID))
	}
	return e.Err()
}

// ValidateCertRequestFields validates the ACME order fields of a certificate
// request (domains, challenge, DNS provider).
func ValidateCertRequestFields(domains []string, challenge, dnsProviderID string) Errs {
	e := Errs{}
	if len(domains) == 0 {
		e.Add("domains", "Add at least one domain")
	}
	if len(domains) > 100 {
		e.Add("domains", "Let's Encrypt allows at most 100 names per certificate")
	}
	seen := map[string]bool{}
	wildcard := ""
	for i, d := range domains {
		switch {
		case !ValidCertDomain(d):
			e.Add(fmt.Sprintf("domains.%d", i), "%q is not a valid domain name", d)
		case seen[strings.ToLower(d)]:
			e.Add(fmt.Sprintf("domains.%d", i), "%s is listed twice", d)
		case strings.Count(d, "*") > 1 || (strings.Contains(d, "*") && !IsWildcardDomain(d)):
			e.Add(fmt.Sprintf("domains.%d", i), "Wildcards are only allowed as the first label (*.example.com)")
		}
		seen[strings.ToLower(d)] = true
		if IsWildcardDomain(d) && wildcard == "" {
			wildcard = d
		}
	}
	switch challenge {
	case ChallengeDNS01:
		if dnsProviderID == "" {
			e.Add("dnsProviderId", "Pick a DNS provider for the DNS-01 challenge")
		}
	case ChallengeHTTP01:
		if wildcard != "" {
			e.Add("challenge", "Wildcard domains (%s) need the DNS-01 challenge", wildcard)
		}
	case ChallengeTLSALPN01:
		e.Add("challenge", "TLS-ALPN-01 is unavailable while nginx owns port 443 — use HTTP-01 or DNS-01")
	case "":
		e.Add("challenge", "Pick a challenge")
	default:
		e.Add("challenge", "Unknown challenge %q", challenge)
	}
	return e
}

func mergeCertErrs(e, o Errs) {
	for k, v := range o {
		if _, ok := e[k]; !ok {
			e[k] = v
		}
	}
}

// ---------------------------------------------------------------- access lists

var (
	accessListNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	basicAuthUserRe  = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,64}$`)
)

const MinBasicAuthPassword = 8

// ParseAccessRule parses an IP rule address: "all", an IP or a CIDR.
// It returns an error with a helpful message when host bits are set.
func ParseAccessRule(cidr string) (netip.Prefix, bool, error) {
	if cidr == "all" {
		return netip.Prefix{}, true, nil
	}
	if strings.Contains(cidr, "/") {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return netip.Prefix{}, false, errors.New("Not a valid IP address or CIDR")
		}
		if p.Masked() != p {
			return netip.Prefix{}, false, fmt.Errorf("Host bits are set — did you mean %s?", p.Masked())
		}
		return p, false, nil
	}
	a, err := netip.ParseAddr(cidr)
	if err != nil {
		return netip.Prefix{}, false, errors.New("Not a valid IP address or CIDR")
	}
	return netip.PrefixFrom(a, a.BitLen()), false, nil
}

func (a *AccessList) Redact() {
	if a.Rules == nil {
		a.Rules = []IPRule{}
	}
	if a.BasicAuth.Users == nil {
		a.BasicAuth.Users = []BasicAuthUser{}
	}
	for i := range a.BasicAuth.Users {
		a.BasicAuth.Users[i].PasswordHash = ""
		a.BasicAuth.Users[i].Password = ""
	}
}

// KeepSecrets hashes new plaintext passwords (bcrypt) and keeps the stored
// hash of users that were sent without one. Client-supplied hashes are ignored.
func (a *AccessList) KeepSecrets(prev any) error {
	p, _ := prev.(*AccessList)
	a.Name = strings.TrimSpace(a.Name)
	a.Description = strings.TrimSpace(a.Description)
	if a.Rules == nil {
		a.Rules = []IPRule{}
	}
	for i := range a.Rules {
		a.Rules[i].CIDR = strings.TrimSpace(a.Rules[i].CIDR)
		a.Rules[i].Note = strings.TrimSpace(a.Rules[i].Note)
		if a.Rules[i].ID == "" {
			a.Rules[i].ID = "r" + randSuffix()
		}
	}
	if a.BasicAuth.Users == nil {
		a.BasicAuth.Users = []BasicAuthUser{}
	}
	a.BasicAuth.Realm = strings.TrimSpace(a.BasicAuth.Realm)
	e := Errs{}
	for i := range a.BasicAuth.Users {
		u := &a.BasicAuth.Users[i]
		u.Username = strings.TrimSpace(u.Username)
		u.PasswordHash = ""
		var old *BasicAuthUser
		if p != nil {
			for j := range p.BasicAuth.Users {
				if p.BasicAuth.Users[j].Username == u.Username {
					old = &p.BasicAuth.Users[j]
				}
			}
		}
		if u.Password != "" {
			if len(u.Password) < MinBasicAuthPassword {
				e.Add(fmt.Sprintf("basicAuth.users.%d.password", i), "At least %d characters", MinBasicAuthPassword)
				u.Password = ""
				continue
			}
			if len(u.Password) > 72 {
				e.Add(fmt.Sprintf("basicAuth.users.%d.password", i), "At most 72 characters")
				u.Password = ""
				continue
			}
			h, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			u.PasswordHash = string(h)
			u.Password = ""
		} else if old != nil {
			u.PasswordHash = old.PasswordHash
		}
		if old != nil && u.LastUsedAt == nil {
			u.LastUsedAt = old.LastUsedAt
		}
	}
	return e.Err()
}

func (a *AccessList) Validate() error {
	e := Errs{}
	if a.Name == "" {
		e.Add("name", "Name is required")
	} else if !accessListNameRe.MatchString(a.Name) {
		e.Add("name", "Letters, digits, space, dot, dash and underscore · max 64")
	}
	if len(a.Description) > 280 {
		e.Add("description", "At most 280 characters")
	}
	ids := map[string]bool{}
	for i, r := range a.Rules {
		if r.Action != "allow" && r.Action != "deny" {
			e.Add(fmt.Sprintf("rules.%d.action", i), "Pick allow or deny")
		}
		if r.CIDR == "" {
			e.Add(fmt.Sprintf("rules.%d.cidr", i), "Enter an IP, CIDR or \"all\"")
		} else if _, _, err := ParseAccessRule(r.CIDR); err != nil {
			e.Add(fmt.Sprintf("rules.%d.cidr", i), "%s", err.Error())
		}
		if len(r.Note) > 120 {
			e.Add(fmt.Sprintf("rules.%d.note", i), "At most 120 characters")
		}
		if ids[r.ID] {
			e.Add(fmt.Sprintf("rules.%d.id", i), "duplicate rule id")
		}
		ids[r.ID] = true
	}
	if len(a.Rules) > 500 {
		e.Add("rules", "At most 500 rules")
	}
	ba := a.BasicAuth
	if len(ba.Realm) > 64 {
		e.Add("basicAuth.realm", "At most 64 characters")
	}
	for _, r := range ba.Realm {
		if r == '"' || r == '\\' || unicode.IsControl(r) {
			e.Add("basicAuth.realm", "Quotes, backslashes and control characters are not allowed")
			break
		}
	}
	names := map[string]bool{}
	for i, u := range ba.Users {
		field := fmt.Sprintf("basicAuth.users.%d", i)
		switch {
		case !basicAuthUserRe.MatchString(u.Username):
			e.Add(field+".username", "Letters, digits and . _ @ + - only · max 64")
		case names[strings.ToLower(u.Username)]:
			e.Add(field+".username", "%s is listed twice", u.Username)
		}
		names[strings.ToLower(u.Username)] = true
		if u.PasswordHash == "" {
			e.Add(field+".password", "Set a password")
		}
	}
	if ba.Enabled && len(ba.Users) == 0 {
		e.Add("basicAuth.users", "Add at least one user, or turn basic authentication off")
	}
	if a.SatisfyAny && !ba.Enabled {
		e.Add("satisfyAny", "Satisfy any needs basic authentication")
	}
	return e.Err()
}

// ---------------------------------------------------------------- streams

// ParseStreamPorts parses "N" or "N-M" (1–65535, at most 100 ports).
func ParseStreamPorts(s string) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, errors.New("Enter a port or a range like 2456-2458")
	}
	a, b, isRange := strings.Cut(s, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi = lo
	var err2 error
	if isRange {
		hi, err2 = strconv.Atoi(strings.TrimSpace(b))
	}
	if err1 != nil || err2 != nil {
		return 0, 0, errors.New("Enter a port or a range like 2456-2458")
	}
	if lo < 1 || hi > 65535 || lo > 65535 || hi < 1 {
		return 0, 0, errors.New("Ports must be between 1 and 65535")
	}
	if hi < lo {
		return 0, 0, errors.New("Range end must not be below its start")
	}
	if hi-lo+1 > 100 {
		return 0, 0, errors.New("At most 100 ports per stream")
	}
	return lo, hi, nil
}

var (
	nginxTimeRe = regexp.MustCompile(`^(\d+(ms|s|m|h|d|w|M|y)?)+$`)
	hostnameRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
)

// ValidNginxTime reports whether v is an nginx time value ("10m", "1h30m", "500ms").
func ValidNginxTime(v string) bool { return nginxTimeRe.MatchString(v) }

func (s *Stream) Validate() error {
	e := Errs{}
	s.Name = strings.TrimSpace(s.Name)
	s.ListenAddress = strings.TrimSpace(s.ListenAddress)
	s.ForwardHost = strings.TrimSpace(s.ForwardHost)
	s.ListenPorts = strings.ReplaceAll(s.ListenPorts, " ", "")
	s.ForwardPorts = strings.ReplaceAll(s.ForwardPorts, " ", "")
	s.IdleTimeout = strings.TrimSpace(s.IdleTimeout)
	if s.Name == "" {
		e.Add("name", "Name is required")
	} else if len(s.Name) > 64 {
		e.Add("name", "At most 64 characters")
	}
	switch s.Protocol {
	case "tcp", "udp", "both":
	default:
		e.Add("protocol", "Pick TCP, UDP or both")
	}
	if s.ListenAddress == "" {
		e.Add("listenAddress", "Pick a listen address")
	} else if net.ParseIP(s.ListenAddress) == nil {
		e.Add("listenAddress", "Not a valid IP address")
	}
	lo, hi, err := ParseStreamPorts(s.ListenPorts)
	if err != nil {
		e.Add("listenPorts", "%s", err.Error())
	}
	if s.BackendID == "" {
		if s.ForwardHost == "" {
			e.Add("forwardHost", "Enter a host or IP to forward to")
		} else if net.ParseIP(s.ForwardHost) == nil && (!hostnameRe.MatchString(s.ForwardHost) || len(s.ForwardHost) > 253) {
			e.Add("forwardHost", "Not a valid IP address or hostname")
		}
		if s.ForwardPorts != "" {
			flo, fhi, ferr := ParseStreamPorts(s.ForwardPorts)
			switch {
			case ferr != nil:
				e.Add("forwardPorts", "%s", ferr.Error())
			case err == nil && fhi-flo != hi-lo:
				e.Add("forwardPorts", "Forward range must have the same size as the listen range (%d ports)", hi-lo+1)
			}
		}
	} else if s.Protocol == "udp" || s.Protocol == "both" {
		e.Add("protocol", "Streams through a load-balancer backend are TCP only")
	}
	if s.IdleTimeout != "" && !ValidNginxTime(s.IdleTimeout) {
		e.Add("idleTimeout", "Use an nginx time like 10m, 30s or 1h")
	}
	return e.Err()
}

// ---------------------------------------------------------------- TLS settings

// Redact masks the EAB HMAC key.
func (t *TLSSettings) Redact() { t.EABHMACKey = MaskSecret(t.EABHMACKey) }

// KeepSecrets trims the custom ACME fields and keeps the stored EAB HMAC key
// when the client resends the mask for the same key ID.
func (t *TLSSettings) KeepSecrets(prev any) error {
	p, _ := prev.(*TLSSettings)
	t.ACMEDirectoryURL = strings.TrimSpace(t.ACMEDirectoryURL)
	t.ACMECABundle = strings.TrimSpace(t.ACMECABundle)
	t.EABKid = strings.TrimSpace(t.EABKid)
	t.EABHMACKey = strings.TrimSpace(t.EABHMACKey)
	if IsMaskedSecret(t.EABHMACKey) {
		t.EABHMACKey = ""
		if p != nil && p.EABKid == t.EABKid {
			t.EABHMACKey = p.EABHMACKey
		}
	}
	return nil
}

// DecodeEABHMAC decodes an EAB HMAC key given as base64url (ZeroSSL, step-ca)
// or standard base64, padded or not.
func DecodeEABHMAC(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}

func validateCustomACME(t *TLSSettings, e Errs) {
	if t.ACMEDirectoryURL == "" {
		e.Add("acmeDirectoryUrl", "Enter the ACME directory URL")
	} else if u, err := url.Parse(t.ACMEDirectoryURL); err != nil || u.Scheme != "https" || u.Host == "" || len(t.ACMEDirectoryURL) > 2048 {
		e.Add("acmeDirectoryUrl", "Use an https:// directory URL, e.g. https://ca.internal/acme/acme/directory")
	}
	if t.ACMECABundle != "" {
		n := 0
		rest := []byte(t.ACMECABundle)
		for len(rest) > 0 && len(t.ACMECABundle) <= 256<<10 {
			var block *pem.Block
			if block, rest = pem.Decode(rest); block == nil {
				break
			}
			if block.Type == "CERTIFICATE" {
				if _, err := x509.ParseCertificate(block.Bytes); err == nil {
					n++
				}
			}
		}
		if n == 0 {
			e.Add("acmeCaBundle", "Paste one or more PEM certificates (-----BEGIN CERTIFICATE-----)")
		}
	}
	switch {
	case t.EABKid == "" && t.EABHMACKey != "":
		e.Add("eabKid", "Enter the EAB key ID that belongs to this HMAC key")
	case t.EABKid != "" && t.EABHMACKey == "":
		e.Add("eabHmacKey", "Enter the EAB HMAC key")
	case len(t.EABKid) > 256:
		e.Add("eabKid", "At most 256 characters")
	}
	if t.EABHMACKey != "" {
		if k, err := DecodeEABHMAC(t.EABHMACKey); err != nil || len(k) == 0 || len(t.EABHMACKey) > 1024 {
			e.Add("eabHmacKey", "The HMAC key must be base64url-encoded")
		}
	}
}

func (t *TLSSettings) Validate() error {
	e := Errs{}
	switch t.ACMEProvider {
	case CertLetsEncrypt, CertLetsEncryptStaging:
	case ACMEProviderCustom:
		validateCustomACME(t, e)
	default:
		e.Add("acmeProvider", "Pick Let's Encrypt production, staging or a custom ACME server")
	}
	t.Email = strings.TrimSpace(t.Email)
	if t.Email != "" {
		if a, err := mail.ParseAddress(t.Email); err != nil || a.Address != t.Email || !strings.Contains(t.Email, ".") {
			e.Add("email", "Not a valid email address")
		}
	}
	switch t.PreferredChallenge {
	case ChallengeHTTP01, ChallengeDNS01:
	case ChallengeTLSALPN01:
		e.Add("preferredChallenge", "TLS-ALPN-01 is unavailable while nginx owns port 443")
	default:
		e.Add("preferredChallenge", "Pick HTTP-01 or DNS-01")
	}
	if t.RenewDaysBefore < 1 || t.RenewDaysBefore > 60 {
		e.Add("renewDaysBefore", "Between 1 and 60 days")
	}
	switch t.CipherProfile {
	case "modern", "intermediate", "old":
	default:
		e.Add("cipherProfile", "Pick modern, intermediate or old")
	}
	if t.HSTS.Enabled {
		if t.HSTS.MaxAgeSeconds < 300 || t.HSTS.MaxAgeSeconds > 63072000 {
			e.Add("hsts.maxAgeSeconds", "Between 5 minutes and 2 years")
		}
		if t.HSTS.Preload && (!t.HSTS.IncludeSubdomains || t.HSTS.MaxAgeSeconds < 31536000) {
			e.Add("hsts.preload", "Preload needs includeSubDomains and a max-age of at least 1 year")
		}
	}
	return e.Err()
}

// ---------------------------------------------------------------- helpers

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
