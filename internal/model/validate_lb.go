package model

// Validation for the load balancer slice: backends, frontends and HAProxy
// settings. Cross-entity checks (unique names, references, port clashes)
// live in the lb package's BeforeSave/BeforeDelete hooks.

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

var (
	lbNameRe     = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
	lbDurationRe = regexp.MustCompile(`^[0-9]+(us|ms|s|m|h|d)?$`)
	lbCookieRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	lbHeaderRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	lbHostnameRe = regexp.MustCompile(`^([a-zA-Z0-9_]([a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?)(\.[a-zA-Z0-9_]([a-zA-Z0-9_-]{0,61}[a-zA-Z0-9_])?)*$`)
	lbExpectRe   = regexp.MustCompile(`^([1-5][0-9]{2}(-[1-5][0-9]{2})?(,[1-5][0-9]{2}(-[1-5][0-9]{2})?)*|[1-5]xx)$`)
	lbDomainRe   = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// LBAlgorithms lists supported balance algorithms.
var LBAlgorithms = []string{"roundrobin", "leastconn", "source", "uri", "random", "first", "static-rr"}

// ValidLBName reports whether s is a valid haproxy proxy/server name.
func ValidLBName(s string) bool { return lbNameRe.MatchString(s) }

// ValidDuration reports whether s is a haproxy time value ("5s", "300ms").
func ValidDuration(s string) bool { return lbDurationRe.MatchString(s) }

// ValidUpstreamHost reports whether s is an IP address or DNS name.
func ValidUpstreamHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	if net.ParseIP(strings.Trim(s, "[]")) != nil {
		return true
	}
	return lbHostnameRe.MatchString(s)
}

// SplitBind parses "addr:port" ("127.0.0.1:10080", "[::]:443", "*:80", ":80").
// addr is "" for all interfaces.
func SplitBind(bind string) (addr string, port int, err error) {
	host, p, err := net.SplitHostPort(strings.TrimSpace(bind))
	if err != nil {
		return "", 0, fmt.Errorf("use address:port, e.g. 127.0.0.1:10080")
	}
	port, err = strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port must be 1–65535")
	}
	if host == "*" {
		host = ""
	}
	if host != "" && net.ParseIP(host) == nil {
		return "", 0, fmt.Errorf("bind address must be an IP address (or * for all interfaces)")
	}
	return host, port, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (b *Backend) Validate() error {
	e := Errs{}
	if !lbNameRe.MatchString(b.Name) {
		e.Add("name", "Use 1–64 letters, digits, dots, dashes or underscores")
	}
	if b.Mode != "http" && b.Mode != "tcp" {
		e.Add("mode", "Mode must be http or tcp")
	}
	if !contains(LBAlgorithms, b.Algorithm) {
		e.Add("algorithm", "Unknown balancing algorithm")
	} else if b.Algorithm == "uri" && b.Mode != "http" {
		e.Add("algorithm", "URI hashing needs HTTP mode")
	}
	names := map[string]int{}
	for i, s := range b.Servers {
		p := fmt.Sprintf("servers.%d.", i)
		if s.Name != "" {
			if !lbNameRe.MatchString(s.Name) {
				e.Add(p+"name", "Use letters, digits, dots, dashes or underscores")
			} else if j, dup := names[strings.ToLower(s.Name)]; dup {
				e.Add(p+"name", "Duplicate server name (also used by server %d)", j+1)
			} else {
				names[strings.ToLower(s.Name)] = i
			}
		}
		if !ValidUpstreamHost(strings.TrimSpace(s.Address)) {
			e.Add(p+"address", "Not a valid IP address or hostname")
		}
		if s.Port < 1 || s.Port > 65535 {
			e.Add(p+"port", "Port must be 1–65535")
		}
		if s.Weight < 1 || s.Weight > 256 {
			e.Add(p+"weight", "Weight must be 1–256")
		}
		if s.Role != ServerActive && s.Role != ServerBackup {
			e.Add(p+"role", "Role must be active or backup")
		}
		if s.State != "" && s.State != ServerStateReady && s.State != ServerStateDrain && s.State != ServerStateMaint {
			e.Add(p+"state", "State must be ready, drain or maint")
		}
	}
	hc := b.HealthCheck
	switch hc.Type {
	case "none", "tcp", "pgsql", "mysql", "redis":
	case "http":
		if !contains([]string{"GET", "HEAD", "POST", "OPTIONS"}, strings.ToUpper(hc.Method)) {
			e.Add("healthCheck.method", "Method must be GET, HEAD, POST or OPTIONS")
		}
		if !strings.HasPrefix(hc.Path, "/") || strings.ContainsAny(hc.Path, " \t\r\n'") {
			e.Add("healthCheck.path", "Path must start with / and contain no spaces or quotes")
		}
		if hc.ExpectStatus != "" && !lbExpectRe.MatchString(strings.ToLower(hc.ExpectStatus)) {
			e.Add("healthCheck.expectStatus", "Use a status like 200, 2xx or 200-399")
		}
		if hc.Host != "" && !ValidUpstreamHost(hc.Host) {
			e.Add("healthCheck.host", "Not a valid hostname")
		}
	default:
		e.Add("healthCheck.type", "Unknown health check type")
	}
	if hc.Interval != "" && !lbDurationRe.MatchString(hc.Interval) {
		e.Add("healthCheck.interval", "Use a duration like 2s or 500ms")
	}
	if hc.Rise < 0 || hc.Rise > 100 {
		e.Add("healthCheck.rise", "Rise must be 1–100")
	}
	if hc.Fall < 0 || hc.Fall > 100 {
		e.Add("healthCheck.fall", "Fall must be 1–100")
	}
	if b.Sticky.Enabled {
		switch b.Sticky.Mode {
		case "insert", "prefix":
			if b.Mode != "http" {
				e.Add("sticky.mode", "Cookie stickiness needs HTTP mode; use source IP for TCP")
			}
			if !lbCookieRe.MatchString(b.Sticky.CookieName) {
				e.Add("sticky.cookieName", "Use letters, digits, dashes or underscores")
			}
		case "source":
		default:
			e.Add("sticky.mode", "Mode must be insert, prefix or source")
		}
	}
	if b.Retries < 0 || b.Retries > 100 {
		e.Add("retries", "Retries must be 0–100")
	}
	for field, v := range map[string]string{"timeouts.connect": b.Timeouts.Connect, "timeouts.server": b.Timeouts.Server, "timeouts.queue": b.Timeouts.Queue} {
		if v != "" && !lbDurationRe.MatchString(v) {
			e.Add(field, "Use a duration like 5s or 30m")
		}
	}
	if b.TLSVerify && !b.TLSReencrypt {
		e.Add("tlsVerify", "Verification requires TLS re-encryption")
	}
	return e.Err()
}

func (f *Frontend) Validate() error {
	e := Errs{}
	if !lbNameRe.MatchString(f.Name) {
		e.Add("name", "Use 1–64 letters, digits, dots, dashes or underscores")
	} else if strings.EqualFold(f.Name, "stats") {
		e.Add("name", "stats is reserved for the built-in stats endpoint")
	}
	if f.Mode != "http" && f.Mode != "tcp" {
		e.Add("mode", "Mode must be http or tcp")
	}
	if _, _, err := SplitBind(f.Bind); err != nil {
		e.Add("bind", "%s", err.Error())
	}
	if f.DefaultBackendID == "" && len(f.Rules) == 0 {
		e.Add("defaultBackendId", "Pick a default backend or add a rule")
	}
	if f.Compression && f.Mode == "tcp" {
		e.Add("compression", "Compression needs HTTP mode")
	}
	for i, r := range f.Rules {
		p := fmt.Sprintf("rules.%d.", i)
		if r.BackendID == "" {
			e.Add(p+"backendId", "Pick a backend")
		}
		if len(r.Conditions) == 0 {
			e.Add(p+"conditions", "Add at least one condition")
		}
		for j, c := range r.Conditions {
			cp := fmt.Sprintf("%sconditions.%d.", p, j)
			v := strings.TrimSpace(c.Value)
			if strings.ContainsAny(c.Value, "'\r\n") {
				e.Add(cp+"value", "Quotes and line breaks are not allowed")
				continue
			}
			switch c.Type {
			case CondHost, CondSNI:
				if c.Type == CondHost && f.Mode != "http" {
					e.Add(cp+"type", "Host rules need HTTP mode (use SNI for TCP)")
				}
				if c.Type == CondSNI && f.Mode != "tcp" {
					e.Add(cp+"type", "SNI rules need TCP mode (use Host for HTTP)")
				}
				if !lbDomainRe.MatchString(strings.ToLower(v)) {
					e.Add(cp+"value", "Not a valid domain")
				}
			case CondPathBeg, CondPath:
				if f.Mode != "http" {
					e.Add(cp+"type", "Path rules need HTTP mode")
				}
				if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t") {
					e.Add(cp+"value", "Path must start with / and contain no spaces")
				}
			case CondPathRegex:
				if f.Mode != "http" {
					e.Add(cp+"type", "Path rules need HTTP mode")
				}
				if v == "" {
					e.Add(cp+"value", "Enter a regular expression")
				}
			case CondHeader:
				if f.Mode != "http" {
					e.Add(cp+"type", "Header rules need HTTP mode")
				}
				if !lbHeaderRe.MatchString(strings.TrimSpace(c.Name)) {
					e.Add(cp+"name", "Not a valid header name")
				}
			case CondSrc:
				parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
				if len(parts) == 0 {
					e.Add(cp+"value", "Enter an IP address or CIDR")
				}
				for _, part := range parts {
					if net.ParseIP(part) == nil {
						if _, _, err := net.ParseCIDR(part); err != nil {
							e.Add(cp+"value", "%s is not an IP address or CIDR", part)
						}
					}
				}
			default:
				e.Add(cp+"type", "Unknown condition type")
			}
		}
	}
	return e.Err()
}

func (s *HAProxySettings) Validate() error {
	e := Errs{}
	for field, v := range map[string]string{
		"timeoutConnect": s.TimeoutConnect, "timeoutClient": s.TimeoutClient,
		"timeoutServer": s.TimeoutServer, "checkInterval": s.CheckInterval,
	} {
		if !lbDurationRe.MatchString(v) {
			e.Add(field, "Use a duration like 5s or 500ms")
		}
	}
	if s.MaxConn < 1 || s.MaxConn > 10_000_000 {
		e.Add("maxConn", "Max connections must be 1–10000000")
	}
	if s.Rise < 1 || s.Rise > 100 {
		e.Add("rise", "Rise must be 1–100")
	}
	if s.Fall < 1 || s.Fall > 100 {
		e.Add("fall", "Fall must be 1–100")
	}
	if s.StatsEnabled {
		if _, _, err := SplitBind(s.StatsBind); err != nil {
			e.Add("statsBind", "%s", err.Error())
		}
	}
	if s.ExposePortStart < 1024 || s.ExposePortStart > 65000 {
		e.Add("exposePortStart", "Pick a port between 1024 and 65000")
	}
	return e.Err()
}
