package mcp

// Tools for the public DNS integration (Settings → Public DNS). They call the
// REST API in-process (see bridge.go).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/instantoffr/relay/internal/core"
)

type dnsZoneArgs struct {
	Zone string `json:"zone" jsonschema:"Domain (zone) of a connected DNS provider, e.g. example.com (see list_dns_zones)"`
}

type dnsCheckArgs struct {
	Domains []string `json:"domains" jsonschema:"Domains to check, e.g. [\"app.example.com\"]"`
}

type dnsRecordArgs struct {
	Zone     string `json:"zone" jsonschema:"Domain (zone), e.g. example.com"`
	Type     string `json:"type" jsonschema:"A, AAAA, CNAME, MX, TXT, CAA or NS"`
	Name     string `json:"name" jsonschema:"Name relative to the zone: www, *.dev, or @ for the domain itself"`
	Data     string `json:"data" jsonschema:"Value: an IP for A/AAAA, a hostname for CNAME/MX/NS, text for TXT, 0 issue \"letsencrypt.org\" for CAA"`
	TTL      int    `json:"ttl,omitempty" jsonschema:"Seconds (0 = the Public DNS default; GoDaddy needs at least 600)"`
	Priority *int   `json:"priority,omitempty" jsonschema:"MX priority"`
	Proxied  *bool  `json:"proxied,omitempty" jsonschema:"Cloudflare only: proxy through Cloudflare"`
	Reason   string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type dnsUpdateArgs struct {
	Zone     string `json:"zone" jsonschema:"Domain (zone), e.g. example.com"`
	ID       string `json:"id" jsonschema:"Record id from list_dns_records"`
	Type     string `json:"type,omitempty" jsonschema:"New type (empty keeps it)"`
	Name     string `json:"name,omitempty" jsonschema:"New name (empty keeps it)"`
	Data     string `json:"data,omitempty" jsonschema:"New value (empty keeps it)"`
	TTL      int    `json:"ttl,omitempty" jsonschema:"New TTL in seconds (0 keeps it)"`
	Priority *int   `json:"priority,omitempty" jsonschema:"New MX priority"`
	Proxied  *bool  `json:"proxied,omitempty" jsonschema:"Cloudflare only: proxy through Cloudflare"`
	Reason   string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type dnsDeleteArgs struct {
	Zone   string `json:"zone" jsonschema:"Domain (zone), e.g. example.com"`
	ID     string `json:"id" jsonschema:"Record id from list_dns_records"`
	Reason string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type dnsSyncArgs struct {
	HostIDs []string `json:"hostIds,omitempty" jsonschema:"Only these proxy host ids (default: every enabled host)"`
	Reason  string   `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type dnsRecord struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	FQDN     string `json:"fqdn"`
	Data     string `json:"data"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Proxied  *bool  `json:"proxied,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

func (s *Service) registerDNSTools() {
	addRead(s, toolInfo{Name: "list_dns_zones", Title: "List public DNS domains",
		Description: "Public DNS integration status: whether it's enabled, the connected DNS providers (GoDaddy, Cloudflare) with errors, their domains (zones), Relay's public IP and the target new records point to."},
		nil, func(ctx context.Context, c *call, _ noArgs) (*readResult, error) {
			if err := requireUnrestricted(c, "reading public DNS"); err != nil {
				return nil, err
			}
			return s.apiRead(ctx, c, "/dns/status", nil, "public DNS")
		})
	addRead(s, toolInfo{Name: "list_dns_records", Title: "List DNS records",
		Description: "All records of a domain at its DNS provider: id, type, name, value, TTL, priority, which proxy hosts they serve and whether Relay may change them."},
		nil, func(ctx context.Context, c *call, in dnsZoneArgs) (*readResult, error) {
			if err := requireUnrestricted(c, "reading DNS records"); err != nil {
				return nil, err
			}
			return s.apiRead(ctx, c, "/dns/zones/"+url.PathEscape(strings.TrimSpace(in.Zone))+"/records", nil, in.Zone+" records")
		})
	addRead(s, toolInfo{Name: "check_dns", Title: "Check DNS for domains",
		Description: "Check whether domains have public DNS pointing to Relay: exists, missing (with the record Relay would create), covered by a wildcard, conflict (points elsewhere), excluded or not in a connected domain."},
		nil, func(ctx context.Context, c *call, in dnsCheckArgs) (*readResult, error) {
			if err := requireUnrestricted(c, "checking DNS"); err != nil {
				return nil, err
			}
			var out any
			if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodPost, "/dns/check", map[string]any{"domains": in.Domains}, &out); err != nil {
				return nil, err
			}
			return readValue(out, "DNS check", strings.Join(in.Domains, ", ")), nil
		})

	addWrite(s, toolInfo{Name: "create_dns_record", Title: "Create a DNS record",
		Description: "Create a record at the domain's DNS provider. Takes effect immediately (not a pending change). May wait for human approval."},
		map[string][]any{"type": {"A", "AAAA", "CNAME", "MX", "TXT", "CAA", "NS"}}, false,
		func(ctx context.Context, c *call, in dnsRecordArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing DNS"); err != nil {
				return nil, err
			}
			zone := strings.TrimSpace(in.Zone)
			if zone == "" {
				return nil, errors.New("zone is required")
			}
			body := map[string]any{"type": in.Type, "name": in.Name, "data": in.Data, "ttl": in.TTL, "priority": in.Priority, "proxied": in.Proxied}
			label := fmt.Sprintf("%s %s → %s", strings.ToUpper(in.Type), fqdnLabel(in.Name, zone), in.Data)
			return &plan{
				Summary: "Create DNS record " + bold(label), Target: fqdnLabel(in.Name, zone), Preview: prettyJSON(body), Detail: "dns create " + label,
				Exec: func(ctx context.Context) (*outcome, error) {
					var rec map[string]any
					if err := s.apiCall(ctx, http.MethodPost, "/dns/zones/"+url.PathEscape(zone)+"/records", body, &rec); err != nil {
						return nil, err
					}
					return &outcome{Text: "Created " + label + ". DNS changes can take a few minutes to reach resolvers.", Structured: rec}, nil
				},
			}, nil
		})
	addWrite(s, toolInfo{Name: "update_dns_record", Title: "Change a DNS record",
		Description: "Change a record at the domain's DNS provider (fields left empty keep their value). Takes effect immediately. May wait for human approval."},
		nil, false,
		func(ctx context.Context, c *call, in dnsUpdateArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing DNS"); err != nil {
				return nil, err
			}
			zone := strings.TrimSpace(in.Zone)
			cur, err := s.dnsRecord(core.WithActor(ctx, c.actor), zone, in.ID)
			if err != nil {
				return nil, err
			}
			next := *cur
			if in.Type != "" {
				next.Type = strings.ToUpper(in.Type)
			}
			if in.Name != "" {
				next.Name = in.Name
			}
			if in.Data != "" {
				next.Data = in.Data
			}
			if in.TTL > 0 {
				next.TTL = in.TTL
			}
			if in.Priority != nil {
				next.Priority = in.Priority
			}
			if in.Proxied != nil {
				next.Proxied = in.Proxied
			}
			body := map[string]any{"type": next.Type, "name": next.Name, "data": next.Data, "ttl": next.TTL, "priority": next.Priority, "proxied": next.Proxied}
			return &plan{
				Summary: "Change DNS record " + bold(cur.Type+" "+cur.FQDN), Target: cur.FQDN, Preview: jsonDiff(cur, next), Detail: "dns update " + cur.FQDN,
				Exec: func(ctx context.Context) (*outcome, error) {
					var rec map[string]any
					if err := s.apiCall(ctx, http.MethodPut, "/dns/zones/"+url.PathEscape(zone)+"/records/"+url.PathEscape(in.ID), body, &rec); err != nil {
						return nil, err
					}
					return &outcome{Text: fmt.Sprintf("Changed %s %s → %s %s.", cur.Type, cur.Data, next.Type, next.Data), Structured: rec}, nil
				},
			}, nil
		})
	addWrite(s, toolInfo{Name: "delete_dns_record", Title: "Delete a DNS record",
		Description: "Delete a record at the domain's DNS provider. Takes effect immediately. May wait for human approval."},
		nil, true,
		func(ctx context.Context, c *call, in dnsDeleteArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing DNS"); err != nil {
				return nil, err
			}
			zone := strings.TrimSpace(in.Zone)
			cur, err := s.dnsRecord(core.WithActor(ctx, c.actor), zone, in.ID)
			if err != nil {
				return nil, err
			}
			label := cur.Type + " " + cur.FQDN + " → " + cur.Data
			return &plan{
				Summary: "Delete DNS record " + bold(label), Target: cur.FQDN, Preview: prettyJSON(cur), Detail: "dns delete " + label,
				Exec: func(ctx context.Context) (*outcome, error) {
					if err := s.apiCall(ctx, http.MethodDelete, "/dns/zones/"+url.PathEscape(zone)+"/records/"+url.PathEscape(in.ID), nil, nil); err != nil {
						return nil, err
					}
					return &outcome{Text: "Deleted " + label + ".", Structured: map[string]any{"id": in.ID, "deleted": true}}, nil
				},
			}, nil
		})
	addWrite(s, toolInfo{Name: "sync_dns", Title: "Create missing DNS records for hosts",
		Description: "Create the missing public DNS records of enabled proxy hosts (all, or hostIds) at the connected DNS providers. Existing or conflicting records are never changed. May wait for human approval."},
		nil, false,
		func(ctx context.Context, c *call, in dnsSyncArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing DNS"); err != nil {
				return nil, err
			}
			var check struct {
				Results []struct {
					Domain  string         `json:"domain"`
					Status  string         `json:"status"`
					Planned map[string]any `json:"planned"`
				} `json:"results"`
			}
			domains, err := s.hostDomains(core.WithActor(ctx, c.actor), in.HostIDs)
			if err != nil {
				return nil, err
			}
			if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodPost, "/dns/check", map[string]any{"domains": domains}, &check); err != nil {
				return nil, err
			}
			var lines []string
			for _, r := range check.Results {
				if r.Status == "missing" && r.Planned != nil {
					lines = append(lines, fmt.Sprintf("+ %v %s → %v", r.Planned["type"], r.Domain, r.Planned["data"]))
				}
			}
			if len(lines) == 0 {
				return nil, errors.New("every proxy host domain in a connected DNS domain already has a record (or can't get one automatically); nothing to create")
			}
			body := map[string]any{"hostIds": in.HostIDs}
			return &plan{
				Summary: fmt.Sprintf("Create %s", bold(fmt.Sprintf("%d DNS record%s", len(lines), plural(len(lines))))),
				Target:  "public DNS", Preview: strings.Join(lines, "\n"), Detail: fmt.Sprintf("dns sync %d", len(lines)),
				Exec: func(ctx context.Context) (*outcome, error) {
					var out map[string]any
					if err := s.apiCall(ctx, http.MethodPost, "/dns/sync", body, &out); err != nil {
						return nil, err
					}
					return &outcome{Text: fmt.Sprintf("Synced public DNS: %d record(s) planned.", len(lines)), Structured: out}, nil
				},
			}, nil
		})
}

func fqdnLabel(name, zone string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "@" {
		return zone
	}
	return strings.TrimSuffix(name, "."+zone) + "." + zone
}

func (s *Service) dnsRecord(ctx context.Context, zone, id string) (*dnsRecord, error) {
	var out struct {
		Records []dnsRecord `json:"records"`
	}
	if err := s.apiCall(ctx, http.MethodGet, "/dns/zones/"+url.PathEscape(zone)+"/records", nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Records {
		if out.Records[i].ID == id {
			return &out.Records[i], nil
		}
	}
	return nil, fmt.Errorf("no record %q in %s (see list_dns_records)", id, zone)
}

func (s *Service) hostDomains(ctx context.Context, ids []string) ([]string, error) {
	var hosts []struct {
		ID      string   `json:"id"`
		Domains []string `json:"domains"`
		Enabled bool     `json:"enabled"`
	}
	if err := s.apiCall(ctx, http.MethodGet, "/hosts", nil, &hosts); err != nil {
		return nil, err
	}
	var out []string
	for _, h := range hosts {
		if !h.Enabled {
			continue
		}
		if len(ids) > 0 {
			found := false
			for _, id := range ids {
				found = found || id == h.ID
			}
			if !found {
				continue
			}
		}
		out = append(out, h.Domains...)
	}
	return out, nil
}
