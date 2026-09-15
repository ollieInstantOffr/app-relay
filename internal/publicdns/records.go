package publicdns

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/model"
)

// Record is one DNS record as shown in Relay. Name is relative to the zone
// ("@" for the zone itself).
type Record struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	FQDN     string   `json:"fqdn"`
	Data     string   `json:"data"`
	TTL      int      `json:"ttl"`
	Priority *int     `json:"priority,omitempty"`
	Proxied  *bool    `json:"proxied,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	ReadOnly bool     `json:"readOnly,omitempty"`
}

// RecordInput creates or replaces a record.
type RecordInput struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Proxied  *bool  `json:"proxied,omitempty"`
}

// editableTypes can be created and changed from Relay.
var editableTypes = map[string]bool{"A": true, "AAAA": true, "CNAME": true, "MX": true, "TXT": true, "CAA": true, "NS": true}

// addressTypes are the record types that make a name reach a server.
var addressTypes = map[string]bool{"A": true, "AAAA": true, "CNAME": true}

var (
	nameLabelRe = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)
	caaRe       = regexp.MustCompile(`^\d{1,3} (issue|issuewild|iodef) "[^"]*"$`)
)

// hashID is a stable id for providers without record ids (GoDaddy).
func hashID(t, name, data string, prio *int) string {
	p := ""
	if prio != nil {
		p = strconv.Itoa(*prio)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{t, name, data, p}, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func relName(fqdn, zone string) string {
	fqdn = strings.TrimSuffix(strings.ToLower(fqdn), ".")
	if fqdn == zone {
		return "@"
	}
	return strings.TrimSuffix(fqdn, "."+zone)
}

func fqdnOf(name, zone string) string {
	if name == "" || name == "@" {
		return zone
	}
	return name + "." + zone
}

func trimDot(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".") }

func readOnly(r Record) bool {
	switch r.Type {
	case "SOA", "SRV":
		return true
	case "NS":
		return r.Name == "@"
	}
	return !editableTypes[r.Type]
}

// normalizeInput cleans a record input and validates it for a zone.
func normalizeInput(in *RecordInput, zone string, minTTL, defTTL int) error {
	e := model.Errs{}
	in.Type = strings.ToUpper(strings.TrimSpace(in.Type))
	in.Name = trimDot(in.Name)
	if in.Name == zone {
		in.Name = "@"
	}
	in.Name = strings.TrimSuffix(in.Name, "."+zone)
	if in.Name == "" {
		in.Name = "@"
	}
	in.Data = strings.TrimSpace(in.Data)
	if !editableTypes[in.Type] {
		e.Add("type", "Pick A, AAAA, CNAME, MX, TXT, CAA or NS")
	}
	if in.Name != "@" {
		for i, l := range strings.Split(in.Name, ".") {
			if (l == "*" && i == 0) || nameLabelRe.MatchString(l) {
				continue
			}
			e.Add("name", "Use letters, digits and hyphens, e.g. www or *.dev (@ for the domain itself)")
			break
		}
	}
	switch in.Type {
	case "A":
		if ip := net.ParseIP(in.Data); ip == nil || ip.To4() == nil {
			e.Add("data", "Use an IPv4 address like 203.0.113.10")
		}
	case "AAAA":
		if ip := net.ParseIP(in.Data); ip == nil || ip.To4() != nil {
			e.Add("data", "Use an IPv6 address like 2001:db8::10")
		}
	case "CNAME", "NS", "MX":
		in.Data = trimDot(in.Data)
		if !model.ValidDNSName(in.Data, false) {
			e.Add("data", "Use a hostname like app.example.com")
		}
	case "TXT":
		if in.Data == "" || len(in.Data) > 4000 || strings.ContainsAny(in.Data, "\r\n") {
			e.Add("data", "Enter the text on one line (up to 4000 characters)")
		}
	case "CAA":
		if !caaRe.MatchString(in.Data) {
			e.Add("data", `Use the form 0 issue "letsencrypt.org"`)
		}
	}
	if in.Type == "CNAME" && in.Name == "@" {
		e.Add("name", "A domain itself can't have a CNAME record; use an A record")
	}
	if in.Type == "NS" && in.Name == "@" {
		e.Add("name", "The domain's own nameservers are managed at the provider")
	}
	if in.Type == "MX" {
		if in.Priority == nil {
			p := 10
			in.Priority = &p
		} else if *in.Priority < 0 || *in.Priority > 65535 {
			e.Add("priority", "Priority must be between 0 and 65535")
		}
	} else {
		in.Priority = nil
	}
	if !addressTypes[in.Type] {
		in.Proxied = nil
	}
	if in.TTL == 0 {
		in.TTL = defTTL
	}
	if in.TTL == 1 && minTTL <= 60 {
		// Cloudflare's "automatic" TTL.
	} else if in.TTL < minTTL {
		e.Add("ttl", "%s", fmt.Sprintf("This provider needs a TTL of at least %d seconds", minTTL))
	} else if in.TTL > 604800 {
		e.Add("ttl", "TTL can be at most 604800 seconds (7 days)")
	}
	return e.Err()
}
