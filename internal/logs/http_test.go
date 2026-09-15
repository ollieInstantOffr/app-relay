package logs

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

type apiClient struct {
	t   *testing.T
	srv *httptest.Server
}

func (c apiClient) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if s, ok := out.(*string); ok {
			var buf bytes.Buffer
			buf.ReadFrom(resp.Body)
			*s = buf.String()
		} else if resp.StatusCode < 300 {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				c.t.Fatalf("%s %s: decode: %v", method, path, err)
			}
		}
	}
	return resp.StatusCode
}

func TestRoutesEndToEnd(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	st := app.Store

	host := &model.ProxyHost{Domains: []string{"jellyfin.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.42", Port: 8096}}
	if err := st.Hosts().Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	stream := &model.Stream{Name: "Minecraft", Protocol: "tcp", ListenPorts: "25565", ForwardHost: "10.0.0.60", Enabled: true}
	if err := st.Streams().Create(ctx, stream); err != nil {
		t.Fatal(err)
	}

	g := newIngester(app, newNameCache())
	now := time.Now().UTC()
	g.add(chunk{src: srcAccess, lines: [][]byte{
		accessLine(now.Add(-3*time.Minute), host.ID, "jellyfin.home.lan", "GET", "/web/index.html", 502, 3.01, "192.168.1.24", "Mozilla"),
		accessLine(now.Add(-2*time.Minute), host.ID, "jellyfin.home.lan", "GET", "/System/Info", 200, 0.05, "192.168.1.24", "Mozilla"),
		accessLine(now.Add(-1*time.Minute), "", "203.0.113.7", "GET", "/.env", 444, 0, "45.155.205.233", "zgrab"),
	}})
	g.add(chunk{src: srcStream, lines: [][]byte{[]byte(fmt.Sprintf(`{"ts":"%d.000","stream_id":"%s","protocol":"TCP","remote_addr":"192.168.1.50","server_port":"25565","upstream_addr":"10.0.0.60:25565","status":"200","bytes_sent":"6000","bytes_received":"0","session_time":"30.0","upstream_connect_time":"0.001"}`, now.Add(-20*time.Second).Unix(), stream.ID))}})
	g.add(chunk{src: srcNginxError, lines: [][]byte{[]byte(now.Format("2006/01/02 15:04:05") + " [emerg] 1#1: bind() to 0.0.0.0:443 failed (98: Address already in use)")}})
	g.flush(ctx)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			actor := core.Actor{Type: core.ActorUser, ID: "u1", Name: "admin", Role: core.RoleAdmin}
			next.ServeHTTP(w, req.WithContext(core.WithActor(req.Context(), actor)))
		})
	})
	Routes(app, r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	c := apiClient{t: t, srv: srv}

	// Access list + filters
	var page core.AccessPage
	if code := c.do("GET", "/logs/access?status="+url.QueryEscape(">=500"), nil, &page); code != 200 || len(page.Entries) != 1 || page.Entries[0].Status != 502 {
		t.Fatalf("access >=500: %d %+v", code, page)
	}
	if code := c.do("GET", "/logs/access?status=bogus", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid status: %d", code)
	}
	if code := c.do("GET", "/logs/access?since=yesterday", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid since: %d", code)
	}
	c.do("GET", "/logs/access?since=1h&kind=stream", nil, &page)
	if len(page.Entries) != 1 || page.Entries[0].Host != "Minecraft" && page.Entries[0].Host != ":25565" {
		t.Fatalf("stream rows: %+v", page.Entries)
	}

	// Detail
	c.do("GET", "/logs/access?status=502", nil, &page)
	var detail accessDetail
	if code := c.do("GET", fmt.Sprintf("/logs/access/%d", page.Entries[0].ID), nil, &detail); code != 200 {
		t.Fatalf("detail: %d", code)
	}
	if len(detail.SameClient) != 1 || detail.Host == nil || detail.Host.Domain != "jellyfin.home.lan" || detail.Blocked {
		t.Fatalf("detail: %+v", detail)
	}
	if code := c.do("GET", "/logs/access/999999", nil, nil); code != http.StatusNotFound {
		t.Fatalf("missing detail: %d", code)
	}

	// Histogram: fast path (unfiltered, minute steps) and SQL path (filtered)
	var hist histogramResponse
	c.do("GET", "/logs/access/histogram?since=1h&buckets=60", nil, &hist)
	var total, s5 int64
	for _, b := range hist.Buckets {
		total += b.Total
		s5 += b.S5xx
	}
	if len(hist.Buckets) != 60 || hist.StepSeconds != 60 || total != 4 || s5 != 1 {
		t.Fatalf("histogram fast path: n=%d step=%d total=%d s5=%d", len(hist.Buckets), hist.StepSeconds, total, s5)
	}
	c.do("GET", "/logs/access/histogram?since=1h&buckets=60&host=jellyfin.home.lan", nil, &hist)
	total = 0
	for _, b := range hist.Buckets {
		total += b.Total
	}
	if total != 2 {
		t.Fatalf("histogram sql path total=%d", total)
	}

	// Error log
	var errs errorPage
	if code := c.do("GET", "/logs/error?level="+url.QueryEscape("error+")+"&source=nginx", nil, &errs); code != 200 || len(errs.Entries) != 1 || errs.Entries[0].Level != "emerg" {
		t.Fatalf("error log: %d %+v", code, errs)
	}

	// Export
	var body string
	c.do("GET", "/logs/export?format=csv&since=1h", nil, &body)
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(recs) != 5 || recs[0][0] != "time" {
		t.Fatalf("csv export: %v %d %q", err, len(recs), body)
	}
	c.do("GET", "/logs/export?format=ndjson&log=error", nil, &body)
	if n := strings.Count(strings.TrimSpace(body), "\n") + 1; n != 1 || !strings.Contains(body, "Address already in use") {
		t.Fatalf("ndjson error export: %q", body)
	}
	if code := c.do("GET", "/logs/export?format=xml", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad format: %d", code)
	}

	// Metrics
	var ov Overview
	if code := c.do("GET", "/metrics/overview?range=1h", nil, &ov); code != 200 {
		t.Fatalf("overview: %d", code)
	}
	if ov.Requests.Total != 3 || ov.Upstream5xx.Count != 1 || ov.UnknownHostHits != 1 || ov.Hosts.Total != 1 || ov.Hosts.Unknown != 1 ||
		len(ov.Traffic.Buckets) != 60 || ov.Traffic.StepSeconds != 60 || ov.Latency.P95Ms == nil || ov.BandwidthBytes != 3*150+6000 ||
		ov.LastDataAt == nil || ov.Nginx.Reachable || ov.Requests.DeltaPct != nil {
		t.Fatalf("overview: %+v", ov)
	}
	if code := c.do("GET", "/metrics/overview?range=2d", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad range: %d", code)
	}
	var hm map[string]HostMetric
	c.do("GET", "/metrics/hosts", nil, &hm)
	if m, ok := hm[host.ID]; !ok || m.Requests24h != 2 || len(m.Series) != 24 || m.Series[23]+m.Series[22] != 2 {
		t.Fatalf("host metrics: %+v", hm)
	}
	var sm map[string]StreamMetric
	c.do("GET", "/metrics/streams", nil, &sm)
	if m, ok := sm[stream.ID]; !ok || m.BytesPerSec != 100 {
		t.Fatalf("stream metrics: %+v", sm)
	}

	// Health without a health service
	var hs map[string]core.HealthStatus
	if code := c.do("GET", "/health", nil, &hs); code != 200 || len(hs) != 0 {
		t.Fatalf("health: %d %v", code, hs)
	}

	// Blocklist
	var bl model.BlocklistSettings
	if code := c.do("POST", "/blocklist", map[string]string{"cidr": "10.1.2.3/8", "note": "scanner"}, &bl); code != 200 || len(bl.Entries) != 1 || bl.Entries[0].CIDR != "10.0.0.0/8" || bl.Entries[0].CreatedBy != "admin" {
		t.Fatalf("block add: %d %+v", code, bl)
	}
	if code := c.do("POST", "/blocklist", map[string]string{"cidr": "10.0.0.0/8"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate block: %d", code)
	}
	if code := c.do("POST", "/blocklist", map[string]string{"cidr": "nope"}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid block: %d", code)
	}
	if code := c.do("POST", "/blocklist", map[string]string{"cidr": "192.168.1.24"}, &bl); code != 200 || len(bl.Entries) != 2 {
		t.Fatalf("block ip: %d %+v", code, bl)
	}
	c.do("GET", fmt.Sprintf("/logs/access/%d", page.Entries[0].ID), nil, &detail)
	if !detail.Blocked {
		t.Fatal("detail should report the client as blocked")
	}
	if code := c.do("DELETE", "/blocklist/"+url.PathEscape("10.0.0.0/8"), nil, &bl); code != 200 || len(bl.Entries) != 1 {
		t.Fatalf("block delete: %d %+v", code, bl)
	}
	if code := c.do("DELETE", "/blocklist/"+url.PathEscape("10.0.0.0/8"), nil, nil); code != http.StatusNotFound {
		t.Fatalf("block delete missing: %d", code)
	}
	saved, _ := store.LoadSettings[model.BlocklistSettings](ctx, st, model.SettingsBlocklist)
	if len(saved.Entries) != 1 || saved.Entries[0].CIDR != "192.168.1.24" {
		t.Fatalf("persisted blocklist: %+v", saved)
	}

	// Audit + activity
	var audit []store.AuditRow
	if code := c.do("GET", "/audit?q=blocklist&since=7d", nil, &audit); code != 200 || len(audit) != 3 || audit[0].Action != "blocklist.remove" || audit[0].ActorName != "admin" {
		t.Fatalf("audit: %d %+v", code, audit)
	}
	c.do("GET", "/audit/export?format=csv", nil, &body)
	if recs, err := csv.NewReader(strings.NewReader(body)).ReadAll(); err != nil || len(recs) != 4 {
		t.Fatalf("audit export: %v %q", err, body)
	}
	var acts []store.ActivityRow
	if code := c.do("GET", "/activity?limit=5", nil, &acts); code != 200 || acts == nil {
		t.Fatalf("activity: %d %v", code, acts)
	}
}
