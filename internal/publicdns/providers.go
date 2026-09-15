package publicdns

// DNS provider clients that manage records. Each implements provider; add a
// type here and to model.PublicDNSProviderTypes to support another provider.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

type provider interface {
	Zones(ctx context.Context) ([]string, error)
	Records(ctx context.Context, zone string) ([]Record, error)
	Create(ctx context.Context, zone string, in RecordInput) error
	Update(ctx context.Context, zone string, cur Record, in RecordInput) error
	Delete(ctx context.Context, zone string, cur Record) error
	MinTTL() int
}

var (
	godaddyAPI    = "https://api.godaddy.com"
	cloudflareAPI = "https://api.cloudflare.com/client/v4"
	httpClient    = &http.Client{Timeout: 20 * time.Second}
)

// ProviderError is a failure reported by (or reaching) the DNS provider.
type ProviderError struct {
	Provider string
	Status   int
	Message  string
}

func (e *ProviderError) Error() string { return e.Message }

func newProvider(p *model.DNSProvider) (provider, error) {
	c := p.Credentials
	switch p.Type {
	case "godaddy":
		if c["apiKey"] == "" || c["apiSecret"] == "" {
			return nil, fmt.Errorf("DNS provider %s has no GoDaddy API key and secret", p.Name)
		}
		return &godaddy{key: c["apiKey"], secret: c["apiSecret"]}, nil
	case "cloudflare":
		if c["apiToken"] == "" {
			return nil, fmt.Errorf("DNS provider %s has no Cloudflare API token", p.Name)
		}
		return &cloudflare{token: c["apiToken"], zoneIDs: map[string]string{}}, nil
	}
	return nil, fmt.Errorf("%s DNS providers can't manage public DNS records yet", p.Type)
}

// call sends a JSON request and returns the status and body.
func call(ctx context.Context, name, method, rawURL string, headers map[string]string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, &ProviderError{Provider: name, Message: fmt.Sprintf("Could not reach %s: %v", name, err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, data, err
}

// ---------------------------------------------------------------- GoDaddy

type godaddy struct{ key, secret string }

type gdRecord struct {
	Type     string `json:"type,omitempty"`
	Name     string `json:"name,omitempty"`
	Data     string `json:"data"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Port     *int   `json:"port,omitempty"`
	Weight   *int   `json:"weight,omitempty"`
	Service  string `json:"service,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

func (g *godaddy) MinTTL() int { return 600 }

func (g *godaddy) do(ctx context.Context, method, path string, body, out any) error {
	status, data, err := call(ctx, "GoDaddy", method, godaddyAPI+path, map[string]string{"Authorization": "sso-key " + g.key + ":" + g.secret}, body)
	if err != nil {
		return err
	}
	if status >= 300 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Fields  []struct {
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"fields"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		if len(e.Fields) > 0 && e.Fields[0].Message != "" {
			msg = strings.TrimSpace(msg + ": " + e.Fields[0].Message)
		}
		switch {
		case status == http.StatusForbidden && e.Code == "ACCESS_DENIED":
			msg = "GoDaddy denied API access · GoDaddy only allows its API for accounts with 10 or more domains or Discount Domain Club"
		case status == http.StatusUnauthorized:
			msg = "GoDaddy rejected the API key and secret"
		case status == http.StatusTooManyRequests:
			msg = "GoDaddy's rate limit was reached (60 requests a minute); try again shortly"
		case msg == "":
			msg = fmt.Sprintf("GoDaddy returned HTTP %d", status)
		}
		return &ProviderError{Provider: "GoDaddy", Status: status, Message: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &ProviderError{Provider: "GoDaddy", Status: status, Message: "Unexpected response from GoDaddy"}
		}
	}
	return nil
}

func (g *godaddy) Zones(ctx context.Context) ([]string, error) {
	var list []struct {
		Domain string `json:"domain"`
	}
	if err := g.do(ctx, http.MethodGet, "/v1/domains?statuses=ACTIVE&limit=1000", nil, &list); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, strings.ToLower(d.Domain))
	}
	return out, nil
}

func (g *godaddy) raw(ctx context.Context, zone string) ([]gdRecord, error) {
	var list []gdRecord
	err := g.do(ctx, http.MethodGet, "/v1/domains/"+url.PathEscape(zone)+"/records", nil, &list)
	return list, err
}

func gdToRecord(r gdRecord, zone string) Record {
	name := strings.ToLower(r.Name)
	if name == "" {
		name = "@"
	}
	rec := Record{Type: strings.ToUpper(r.Type), Name: name, FQDN: fqdnOf(name, zone), Data: r.Data, TTL: r.TTL, Priority: r.Priority}
	rec.ID = hashID(rec.Type, rec.Name, rec.Data, rec.Priority)
	return rec
}

func (g *godaddy) Records(ctx context.Context, zone string) ([]Record, error) {
	list, err := g.raw(ctx, zone)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(list))
	for _, r := range list {
		out = append(out, gdToRecord(r, zone))
	}
	return out, nil
}

func gdFromInput(in RecordInput) gdRecord {
	return gdRecord{Type: in.Type, Name: in.Name, Data: in.Data, TTL: in.TTL, Priority: in.Priority}
}

func (g *godaddy) Create(ctx context.Context, zone string, in RecordInput) error {
	return g.do(ctx, http.MethodPatch, "/v1/domains/"+url.PathEscape(zone)+"/records", []gdRecord{gdFromInput(in)}, nil)
}

// set returns the other records sharing cur's type and name (GoDaddy
// replaces and deletes records per type and name).
func (g *godaddy) set(ctx context.Context, zone, typ, name string, skipID string) ([]gdRecord, error) {
	list, err := g.raw(ctx, zone)
	if err != nil {
		return nil, err
	}
	var out []gdRecord
	for _, r := range list {
		rec := gdToRecord(r, zone)
		if rec.Type == typ && rec.Name == name && rec.ID != skipID {
			r.Type, r.Name = "", "" // the PUT body carries only the values
			out = append(out, r)
		}
	}
	return out, nil
}

func (g *godaddy) setPath(zone, typ, name string) string {
	return "/v1/domains/" + url.PathEscape(zone) + "/records/" + url.PathEscape(typ) + "/" + url.PathEscape(name)
}

func (g *godaddy) Update(ctx context.Context, zone string, cur Record, in RecordInput) error {
	if cur.Type != in.Type || cur.Name != in.Name {
		if err := g.Create(ctx, zone, in); err != nil {
			return err
		}
		return g.Delete(ctx, zone, cur)
	}
	rest, err := g.set(ctx, zone, cur.Type, cur.Name, cur.ID)
	if err != nil {
		return err
	}
	next := gdFromInput(in)
	next.Type, next.Name = "", ""
	return g.do(ctx, http.MethodPut, g.setPath(zone, cur.Type, cur.Name), append(rest, next), nil)
}

func (g *godaddy) Delete(ctx context.Context, zone string, cur Record) error {
	rest, err := g.set(ctx, zone, cur.Type, cur.Name, cur.ID)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return g.do(ctx, http.MethodDelete, g.setPath(zone, cur.Type, cur.Name), nil, nil)
	}
	return g.do(ctx, http.MethodPut, g.setPath(zone, cur.Type, cur.Name), rest, nil)
}

// ---------------------------------------------------------------- Cloudflare

type cloudflare struct {
	token   string
	mu      sync.Mutex
	zoneIDs map[string]string
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type cfRecord struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Proxied  *bool  `json:"proxied,omitempty"`
}

func (c *cloudflare) MinTTL() int { return 60 }

func (c *cloudflare) do(ctx context.Context, method, path string, body any) (*cfEnvelope, error) {
	status, data, err := call(ctx, "Cloudflare", method, cloudflareAPI+path, map[string]string{"Authorization": "Bearer " + c.token}, body)
	if err != nil {
		return nil, err
	}
	var env cfEnvelope
	_ = json.Unmarshal(data, &env)
	if status >= 300 || !env.Success {
		msg := fmt.Sprintf("Cloudflare returned HTTP %d", status)
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden || (len(env.Errors) > 0 && (env.Errors[0].Code == 10000 || env.Errors[0].Code == 9109)) {
			msg += " · the API token needs Zone · DNS · Edit and Zone · Zone · Read"
		}
		return nil, &ProviderError{Provider: "Cloudflare", Status: status, Message: msg}
	}
	return &env, nil
}

func (c *cloudflare) Zones(ctx context.Context) ([]string, error) {
	var out []string
	ids := map[string]string{}
	for page := 1; page <= 50; page++ {
		env, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones?per_page=50&page=%d", page), nil)
		if err != nil {
			return nil, err
		}
		var list []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(env.Result, &list)
		for _, z := range list {
			name := strings.ToLower(z.Name)
			ids[name] = z.ID
			out = append(out, name)
		}
		if page >= env.ResultInfo.TotalPages {
			break
		}
	}
	c.mu.Lock()
	c.zoneIDs = ids
	c.mu.Unlock()
	sort.Strings(out)
	return out, nil
}

func (c *cloudflare) zoneID(ctx context.Context, zone string) (string, error) {
	c.mu.Lock()
	id := c.zoneIDs[zone]
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	if _, err := c.Zones(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id = c.zoneIDs[zone]; id == "" {
		return "", &ProviderError{Provider: "Cloudflare", Status: http.StatusNotFound, Message: "Cloudflare has no zone " + zone}
	}
	return id, nil
}

func (c *cloudflare) Records(ctx context.Context, zone string) ([]Record, error) {
	id, err := c.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	var out []Record
	for page := 1; page <= 50; page++ {
		env, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?per_page=500&page=%d", url.PathEscape(id), page), nil)
		if err != nil {
			return nil, err
		}
		var list []cfRecord
		_ = json.Unmarshal(env.Result, &list)
		for _, r := range list {
			name := relName(r.Name, zone)
			out = append(out, Record{ID: r.ID, Type: strings.ToUpper(r.Type), Name: name, FQDN: fqdnOf(name, zone), Data: r.Content, TTL: r.TTL, Priority: r.Priority, Proxied: r.Proxied})
		}
		if page >= env.ResultInfo.TotalPages {
			break
		}
	}
	return out, nil
}

func cfFromInput(in RecordInput, zone string) cfRecord {
	ttl := in.TTL
	if ttl <= 0 {
		ttl = 1 // automatic
	}
	return cfRecord{Type: in.Type, Name: fqdnOf(in.Name, zone), Content: in.Data, TTL: ttl, Priority: in.Priority, Proxied: in.Proxied}
}

func (c *cloudflare) Create(ctx context.Context, zone string, in RecordInput) error {
	id, err := c.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(id)+"/dns_records", cfFromInput(in, zone))
	return err
}

func (c *cloudflare) Update(ctx context.Context, zone string, cur Record, in RecordInput) error {
	id, err := c.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(id)+"/dns_records/"+url.PathEscape(cur.ID), cfFromInput(in, zone))
	return err
}

func (c *cloudflare) Delete(ctx context.Context, zone string, cur Record) error {
	id, err := c.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(id)+"/dns_records/"+url.PathEscape(cur.ID), nil)
	return err
}
