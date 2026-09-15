package model

import (
	"net"
	"regexp"
	"slices"
	"strings"
)

// SettingsPublicDNS configures the public DNS integration: Relay manages
// records at the user's DNS provider and can create the record a proxy host
// needs. Changes apply immediately (not a pending change).
const SettingsPublicDNS = "public_dns"

// PublicDNSProviderTypes are the DNS provider types that can manage records.
var PublicDNSProviderTypes = []string{"godaddy", "cloudflare"}

type PublicDNSSettings struct {
	Enabled bool `json:"enabled"`
	// ProviderIDs are the DNS providers whose domains Relay manages.
	ProviderIDs []string `json:"providerIds"`
	// AutoCreate creates missing records for enabled proxy hosts after each apply.
	AutoCreate bool `json:"autoCreate"`
	// RecordType of created records: A (to Target or the public IP) or CNAME (to Target).
	RecordType string `json:"recordType"`
	Target     string `json:"target"`
	TTL        int    `json:"ttl"`
	// ExcludedZones are never changed automatically.
	ExcludedZones []string `json:"excludedZones"`
}

func DefaultPublicDNS() PublicDNSSettings {
	return PublicDNSSettings{ProviderIDs: []string{}, AutoCreate: true, RecordType: "A", TTL: 600, ExcludedZones: []string{}}
}

func (s *PublicDNSSettings) Normalize() {
	s.RecordType = strings.ToUpper(strings.TrimSpace(s.RecordType))
	if s.RecordType == "" {
		s.RecordType = "A"
	}
	s.Target = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s.Target)), ".")
	if s.TTL == 0 {
		s.TTL = 600
	}
	s.ProviderIDs = uniqueTrimmed(s.ProviderIDs, false)
	s.ExcludedZones = uniqueTrimmed(s.ExcludedZones, true)
}

func uniqueTrimmed(in []string, domain bool) []string {
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if domain {
			v = strings.TrimSuffix(strings.ToLower(v), ".")
		}
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func (s *PublicDNSSettings) Validate() error {
	e := Errs{}
	switch s.RecordType {
	case "A":
		if s.Target != "" {
			if ip := net.ParseIP(s.Target); ip == nil || ip.To4() == nil {
				e.Add("target", "Use an IPv4 address, or leave it empty to use Relay's public IP")
			}
		}
	case "CNAME":
		if s.Target == "" {
			e.Add("target", "Enter the hostname the records should point to")
		} else if !ValidDNSName(s.Target, false) {
			e.Add("target", "Use a hostname like edge.example.com")
		}
	default:
		e.Add("recordType", "Pick A or CNAME")
	}
	if s.TTL < 60 || s.TTL > 86400 {
		e.Add("ttl", "TTL must be between 60 and 86400 seconds")
	}
	if s.Enabled && len(s.ProviderIDs) == 0 {
		e.Add("providerIds", "Pick at least one DNS provider")
	}
	return e.Err()
}

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// ValidDNSName reports whether n is a dotted DNS name with at least two
// labels; wildcard allows "*" as the first label.
func ValidDNSName(n string, wildcard bool) bool {
	n = strings.TrimSuffix(strings.ToLower(n), ".")
	if len(n) == 0 || len(n) > 253 || net.ParseIP(n) != nil {
		return false
	}
	labels := strings.Split(n, ".")
	if len(labels) < 2 {
		return false
	}
	for i, l := range labels {
		if l == "*" && i == 0 && wildcard {
			continue
		}
		if !dnsLabelRe.MatchString(l) {
			return false
		}
	}
	return true
}
