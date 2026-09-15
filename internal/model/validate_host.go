package model

// Validation and normalisation for proxy hosts, redirects and the default
// host settings (slice: hosts). Field paths match the UI form
// ("domains.0", "upstream.port", "locations.1.path", …).
//
// Helpers in this file are prefixed with "host"/"Host" because several slices
// add validate_*.go files to this package.

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var (
	hostDomainLabelRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostUpstreamLabelRe = regexp.MustCompile(`^[a-zA-Z0-9_]([a-zA-Z0-9_-]{0,62})$`)
	hostHeaderNameRe    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	hostSizeRe          = regexp.MustCompile(`^[0-9]+[kmg]?$`)
	hostCountryRe       = regexp.MustCompile(`^[A-Z]{2}$`)
	hostIPv4LikeRe      = regexp.MustCompile(`^[0-9.]+$`)
	hostForbiddenBlock  = regexp.MustCompile(`(?:^|[\s;{}])(server|http|stream|events|upstream(?:\s+[^\s{};]+)?)\s*\{`)
)

// Forward-auth providers.
const (
	ForwardAuthAuthelia    = "authelia"
	ForwardAuthAuthentik   = "authentik"
	ForwardAuthOAuth2Proxy = "oauth2-proxy"
	ForwardAuthCustom      = "custom"
)

// Default host actions.
const (
	DefaultHostClose    = "close"
	DefaultHost404      = "404"
	DefaultHostRedirect = "redirect"
	DefaultHostServe    = "host"
)

// ---------------------------------------------------------------- field checks

// HostNormalizeDomain trims, lowercases and strips a trailing dot.
func HostNormalizeDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

// HostDomainError returns a user-facing message when d is not a valid
// server name (exact or leading wildcard like *.home.lan), or "".
func HostDomainError(d string) string {
	if d == "" {
		return "Domain is required"
	}
	if len(d) > 253 {
		return "Domain is too long (max 253 characters)"
	}
	if strings.Contains(d, "://") || strings.ContainsAny(d, "/:") {
		return "Enter the domain only — no scheme, port or path"
	}
	if strings.ContainsAny(d, " \t\r\n") {
		return "Domain must not contain spaces"
	}
	if d != strings.ToLower(d) {
		return "Domain must be lowercase"
	}
	name := d
	wildcard := strings.HasPrefix(name, "*.")
	if wildcard {
		name = name[2:]
	}
	if strings.Contains(name, "*") {
		return "Wildcards are only allowed as the first label (*.home.lan)"
	}
	if name == "" {
		return "Wildcard needs a domain after *."
	}
	if !wildcard && net.ParseIP(name) != nil {
		return ""
	}
	for _, l := range strings.Split(name, ".") {
		if l == "" {
			return "Domain has an empty label (two dots in a row?)"
		}
		if len(l) > 63 {
			return "Each part of a domain must be at most 63 characters"
		}
		if !hostDomainLabelRe.MatchString(l) {
			return fmt.Sprintf("%s is not a valid domain name", d)
		}
	}
	return ""
}

// HostUpstreamHostError validates an upstream address: IPv4, IPv6 (optionally
// in brackets) or a hostname.
func HostUpstreamHostError(h string) string {
	if h == "" {
		return "Upstream host is required"
	}
	if strings.Contains(h, "://") {
		return "Enter the host only — pick the scheme separately"
	}
	if strings.ContainsAny(h, " \t\r\n/\"'{};") {
		return "Not a valid host or IP address"
	}
	if strings.HasPrefix(h, "[") || strings.HasSuffix(h, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
		if ip := net.ParseIP(inner); ip == nil || ip.To4() != nil {
			return "Not a valid IPv6 address"
		}
		return ""
	}
	if ip := net.ParseIP(h); ip != nil {
		return ""
	}
	if strings.Contains(h, ":") {
		return "Not a valid IPv6 address"
	}
	if hostIPv4LikeRe.MatchString(h) {
		return "Not a valid IPv4 address"
	}
	if len(h) > 253 {
		return "Host name is too long"
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || !hostUpstreamLabelRe.MatchString(l) || strings.HasSuffix(l, "-") {
			return "Not a valid host name or IP address"
		}
	}
	return ""
}

// HostURLError validates an absolute http(s) URL used in generated config
// (forward-auth, redirects). nginx variables like $request_uri are allowed.
func HostURLError(s string) string {
	if s == "" {
		return "URL is required"
	}
	if strings.ContainsAny(s, " \t\r\n\"'{};\\") {
		return "URL must not contain spaces, quotes, braces or semicolons"
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "Enter a full URL starting with http:// or https://"
	}
	return ""
}

// HostSnippetError sanity-checks a custom nginx snippet inserted into a server
// block: balanced braces and quotes, and no top-level context blocks. Full
// validation happens with nginx -t on apply.
func HostSnippetError(s string) string {
	if len(s) > 64<<10 {
		return "Snippet is too long (max 64 KB)"
	}
	var (
		depth    int
		quote    rune
		escaped  bool
		comment  bool
		stripped strings.Builder
	)
	for _, r := range s {
		switch {
		case comment:
			if r == '\n' {
				comment = false
				stripped.WriteRune('\n')
			} else {
				stripped.WriteRune(' ')
			}
			continue
		case quote != 0:
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == quote:
				quote = 0
			}
			stripped.WriteRune(' ')
			continue
		}
		switch r {
		case '#':
			comment = true
			stripped.WriteRune(' ')
			continue
		case '"', '\'':
			quote = r
			stripped.WriteRune(' ')
			continue
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return "Unbalanced braces: unexpected }"
			}
		}
		stripped.WriteRune(r)
	}
	if quote != 0 {
		return "Unterminated quoted string"
	}
	if depth > 0 {
		return "Unbalanced braces: missing }"
	}
	if m := hostForbiddenBlock.FindStringSubmatch(stripped.String()); m != nil {
		return fmt.Sprintf("%s { … } blocks aren't allowed here — the snippet is inserted inside this host's server block", strings.Fields(m[1])[0])
	}
	return ""
}

func hostPathError(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "Path must start with /"
	}
	if strings.ContainsAny(p, " \t\r\n\"'{};\\") {
		return "Path must not contain spaces, quotes, braces or semicolons"
	}
	return ""
}

func hostCheckUpstream(e Errs, prefix string, u Upstream) {
	if u.Scheme != "http" && u.Scheme != "https" {
		e.Add(prefix+".scheme", "Scheme must be http or https")
	}
	if msg := HostUpstreamHostError(u.Host); msg != "" {
		e.Add(prefix+".host", "%s", msg)
	}
	if u.Port < 1 || u.Port > 65535 {
		e.Add(prefix+".port", "Port must be between 1 and 65535")
	}
	if u.Path != "" {
		if msg := hostPathError(u.Path); msg != "" {
			e.Add(prefix+".path", "%s", msg)
		}
	}
}

func hostCheckDomains(e Errs, domains []string) {
	if len(domains) == 0 {
		e.Add("domains", "Add at least one domain")
		return
	}
	seen := map[string]bool{}
	for i, d := range domains {
		f := fmt.Sprintf("domains.%d", i)
		if msg := HostDomainError(d); msg != "" {
			e.Add(f, "%s", msg)
			continue
		}
		if seen[d] {
			e.Add(f, "%s is listed twice", d)
		}
		seen[d] = true
	}
}

func hostNormalizeDomains(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, d := range in {
		d = HostNormalizeDomain(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

func hostRandID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[b[i]&31]
	}
	return string(b)
}

// ---------------------------------------------------------------- proxy host

// Normalize trims and lowercases user input, fills defaults (hsts "inherit",
// source "manual", location ids) and ensures slices are non-nil. A host
// without a certificate cannot force HTTPS, so ForceHTTPS is cleared.
func (h *ProxyHost) Normalize() {
	h.Domains = hostNormalizeDomains(h.Domains)
	h.Upstream.Scheme = strings.ToLower(strings.TrimSpace(h.Upstream.Scheme))
	if h.Upstream.Scheme == "" {
		h.Upstream.Scheme = "http"
	}
	h.Upstream.Host = strings.TrimSpace(h.Upstream.Host)
	h.Upstream.Path = strings.TrimSpace(h.Upstream.Path)
	h.Upstream.BackendID = strings.TrimSpace(h.Upstream.BackendID)
	h.AccessListID = strings.TrimSpace(h.AccessListID)
	h.CertificateID = strings.TrimSpace(h.CertificateID)
	if h.CertificateID == "" {
		h.ForceHTTPS = false
	}
	h.HSTS = strings.ToLower(strings.TrimSpace(h.HSTS))
	if h.HSTS == "" {
		h.HSTS = "inherit"
	}
	h.CipherProfile = strings.ToLower(strings.TrimSpace(h.CipherProfile))
	if h.CipherProfile == "inherit" {
		h.CipherProfile = ""
	}
	if h.Locations == nil {
		h.Locations = []Location{}
	}
	for i := range h.Locations {
		l := &h.Locations[i]
		l.Path = strings.TrimSpace(l.Path)
		if l.ID == "" {
			l.ID = hostRandID()
		}
		l.Kind = strings.ToLower(strings.TrimSpace(l.Kind))
		if l.Kind == "" {
			l.Kind = LocationProxy
		}
		l.Upstream.Scheme = strings.ToLower(strings.TrimSpace(l.Upstream.Scheme))
		if l.Upstream.Scheme == "" {
			l.Upstream.Scheme = "http"
		}
		l.Upstream.Host = strings.TrimSpace(l.Upstream.Host)
		l.Upstream.Path = strings.TrimSpace(l.Upstream.Path)
		l.AccessListID = strings.TrimSpace(l.AccessListID)
		headers := make([]Header, 0, len(l.Headers))
		for _, hd := range l.Headers {
			hd.Name = strings.TrimSpace(hd.Name)
			if hd.Name == "" && strings.TrimSpace(hd.Value) == "" {
				continue
			}
			headers = append(headers, hd)
		}
		l.Headers = headers
	}
	h.ForwardAuth.Provider = strings.ToLower(strings.TrimSpace(h.ForwardAuth.Provider))
	h.ForwardAuth.VerifyURL = strings.TrimSpace(h.ForwardAuth.VerifyURL)
	h.ForwardAuth.SignInURL = strings.TrimSpace(h.ForwardAuth.SignInURL)
	h.RateLimit.ExemptAccessListID = strings.TrimSpace(h.RateLimit.ExemptAccessListID)
	countries := make([]string, 0, len(h.GeoBlock.AllowCountries))
	seen := map[string]bool{}
	for _, c := range h.GeoBlock.AllowCountries {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		countries = append(countries, c)
	}
	h.GeoBlock.AllowCountries = countries
	h.MaxBodySize = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(h.MaxBodySize), " ", ""))
	h.CustomNginx = strings.ReplaceAll(h.CustomNginx, "\r\n", "\n")
	if strings.TrimSpace(h.CustomNginx) == "" {
		h.CustomNginx = ""
	}
	if h.Source == "" {
		h.Source = SourceManual
	}
}

func (h *ProxyHost) Validate() error {
	e := Errs{}
	hostCheckDomains(e, h.Domains)
	hostCheckUpstream(e, "upstream", h.Upstream)

	switch h.HSTS {
	case "inherit", "on", "off":
	default:
		e.Add("hsts", "HSTS must be inherit, on or off")
	}
	switch h.CipherProfile {
	case "", "modern", "intermediate", "old":
	default:
		e.Add("cipherProfile", "Cipher profile must be inherit, modern, intermediate or old")
	}
	if h.ForceHTTPS && h.CertificateID == "" {
		e.Add("forceHttps", "Force HTTPS needs a certificate")
	}

	paths := map[string]int{}
	for i, l := range h.Locations {
		p := fmt.Sprintf("locations.%d", i)
		switch {
		case l.Path == "":
			e.Add(p+".path", "Path is required")
		case l.Path == "/":
			e.Add(p+".path", "/ is the host's default upstream — set it on the Details tab")
		default:
			if msg := hostPathError(l.Path); msg != "" {
				e.Add(p+".path", "%s", msg)
			} else if j, dup := paths[l.Path]; dup {
				e.Add(p+".path", "Duplicate path — location %d already uses %s", j+1, l.Path)
			} else {
				paths[l.Path] = i
			}
		}
		switch l.Kind {
		case LocationProxy:
			hostCheckUpstream(e, p+".upstream", l.Upstream)
		case LocationSame, LocationDeny:
		default:
			e.Add(p+".kind", "Pick proxy, same upstream or deny")
		}
		for j, hd := range l.Headers {
			hf := fmt.Sprintf("%s.headers.%d", p, j)
			if !hostHeaderNameRe.MatchString(hd.Name) {
				e.Add(hf+".name", "Header names may only contain letters, digits, - and _")
			}
			if strings.ContainsAny(hd.Value, "\r\n\"\\") {
				e.Add(hf+".value", "Header values must not contain quotes, backslashes or line breaks")
			}
		}
	}

	if fa := h.ForwardAuth; fa.Enabled {
		switch fa.Provider {
		case ForwardAuthAuthelia, ForwardAuthAuthentik, ForwardAuthOAuth2Proxy, ForwardAuthCustom:
		default:
			e.Add("forwardAuth.provider", "Pick a provider")
		}
		if msg := HostURLError(fa.VerifyURL); msg != "" {
			e.Add("forwardAuth.verifyUrl", "%s", msg)
		}
		if fa.SignInURL != "" {
			if msg := HostURLError(fa.SignInURL); msg != "" {
				e.Add("forwardAuth.signInUrl", "%s", msg)
			}
		}
	}
	if rl := h.RateLimit; rl.Enabled {
		if rl.RequestsPerSecond < 1 || rl.RequestsPerSecond > 100000 {
			e.Add("rateLimit.requestsPerSecond", "Between 1 and 100000 requests per second")
		}
		if rl.Burst < 0 || rl.Burst > 1000000 {
			e.Add("rateLimit.burst", "Burst must be between 0 and 1000000")
		}
	}
	if gb := h.GeoBlock; gb.Enabled {
		if len(gb.AllowCountries) == 0 {
			e.Add("geoBlock.allowCountries", "Add at least one country")
		}
		for i, c := range gb.AllowCountries {
			if !hostCountryRe.MatchString(c) {
				e.Add(fmt.Sprintf("geoBlock.allowCountries.%d", i), "%s is not a two-letter country code", c)
			}
		}
	}
	if h.MaxBodySize != "" && !hostSizeRe.MatchString(h.MaxBodySize) {
		e.Add("maxBodySize", "Use an nginx size like 10m, 1g, or 0 for unlimited")
	}
	if h.ProxyReadTimeout < 0 || h.ProxyReadTimeout > 86400 {
		e.Add("proxyReadTimeout", "Between 0 (default 60 s) and 86400 seconds")
	}
	if h.ProxySendTimeout < 0 || h.ProxySendTimeout > 86400 {
		e.Add("proxySendTimeout", "Between 0 (default 60 s) and 86400 seconds")
	}
	if msg := HostSnippetError(h.CustomNginx); msg != "" {
		e.Add("customNginx", "%s", msg)
	}
	switch h.Source {
	case SourceManual, SourceDocker, SourceExpose, SourceImport, SourceMCP:
	default:
		e.Add("source", "Unknown source %q", h.Source)
	}
	return e.Err()
}

// ---------------------------------------------------------------- redirect

// IsWholeDomain reports whether the redirect covers every path of its domains.
func (v *Redirect) IsWholeDomain() bool { return v.FromPath == "" || v.FromPath == "/" }

func (v *Redirect) Normalize() {
	v.Domains = hostNormalizeDomains(v.Domains)
	v.FromPath = strings.TrimSpace(v.FromPath)
	if v.FromPath == "/" {
		v.FromPath = ""
	}
	v.To = strings.TrimSpace(v.To)
	if v.Code == 0 {
		v.Code = 301
	}
	v.CertificateID = strings.TrimSpace(v.CertificateID)
	if v.CertificateID == "" {
		v.ForceHTTPS = false
	}
}

func (v *Redirect) Validate() error {
	e := Errs{}
	hostCheckDomains(e, v.Domains)
	if v.FromPath != "" {
		if msg := hostPathError(v.FromPath); msg != "" {
			e.Add("fromPath", "%s", msg)
		}
	}
	if msg := HostURLError(v.To); msg != "" {
		if v.To == "" {
			msg = "Enter where to redirect to"
		}
		e.Add("to", "%s", msg)
	} else if u, err := url.Parse(v.To); err == nil && v.IsWholeDomain() && (u.Path == "" || u.Path == "/") {
		target := strings.ToLower(u.Hostname())
		for _, d := range v.Domains {
			if d == target {
				e.Add("to", "This redirects %s to itself", d)
				break
			}
		}
	}
	switch v.Code {
	case 301, 302, 307, 308:
	default:
		e.Add("code", "Code must be 301, 302, 307 or 308")
	}
	if v.ForceHTTPS && v.CertificateID == "" {
		e.Add("forceHttps", "Force HTTPS needs a certificate")
	}
	return e.Err()
}

// ---------------------------------------------------------------- default host

func (d *DefaultHostSettings) Normalize() {
	d.Action = strings.ToLower(strings.TrimSpace(d.Action))
	if d.Action == "" {
		d.Action = DefaultHostClose
	}
	d.RedirectTo = strings.TrimSpace(d.RedirectTo)
	d.HostID = strings.TrimSpace(d.HostID)
	d.CertificateID = strings.TrimSpace(d.CertificateID)
}

func (d *DefaultHostSettings) Validate() error {
	e := Errs{}
	switch d.Action {
	case DefaultHostClose, DefaultHost404:
	case DefaultHostRedirect:
		if msg := HostURLError(d.RedirectTo); msg != "" {
			e.Add("redirectTo", "%s", msg)
		}
	case DefaultHostServe:
		if d.HostID == "" {
			e.Add("hostId", "Pick a proxy host")
		}
	default:
		e.Add("action", "Pick what unknown hosts get")
	}
	return e.Err()
}
