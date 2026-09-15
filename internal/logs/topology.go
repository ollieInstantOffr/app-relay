package logs

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Traffic flow data for the Topology page (design 31) and the single host
// flow view (design 32). Structure (hosts, backends, servers) comes from the
// entity APIs and /api/lb/stats; this adds the traffic on each link.

var topologyRanges = map[string]time.Duration{
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
}

// vpnNetworks are the address ranges reported as VPN clients: carrier-grade
// NAT (Tailscale, Headscale, ZeroTier) and Tailscale's IPv6 range.
var vpnNetworks = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// ClientSource classifies a client address: "lan", "vpn" or "internet".
func ClientSource(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "internet"
	}
	addr = addr.Unmap().WithZone("")
	for _, p := range vpnNetworks {
		if p.Contains(addr) {
			return "vpn"
		}
	}
	if model.IsLocalAddr(addr) {
		return "lan"
	}
	return "internet"
}

// FlowTraffic is the traffic on one link over the window.
type FlowTraffic struct {
	Requests    int64    `json:"requests"`
	RPS         float64  `json:"rps"`
	S4xx        int64    `json:"s4xx"`
	S5xx        int64    `json:"s5xx"`
	BytesIn     int64    `json:"bytesIn"`
	BytesOut    int64    `json:"bytesOut"`
	BytesPerSec float64  `json:"bytesPerSec"` // in + out
	P50Ms       *float64 `json:"p50Ms"`
	P95Ms       *float64 `json:"p95Ms"`
}

// FlowClients breaks requests down by where they came from. Blocked requests
// are counted only there, so the four buckets add up to Requests.
type FlowClients struct {
	Unique   int   `json:"unique"`
	Requests int64 `json:"requests"`
	LAN      int64 `json:"lan"`
	VPN      int64 `json:"vpn"`
	Internet int64 `json:"internet"`
	Blocked  int64 `json:"blocked"`
}

// Topology is GET /api/metrics/topology?range=.
type Topology struct {
	Range       string                 `json:"range"`
	WindowSec   int64                  `json:"windowSec"`
	GeneratedAt time.Time              `json:"generatedAt"`
	Totals      FlowTraffic            `json:"totals"`
	Unknown     FlowTraffic            `json:"unknown"` // requests for hosts Relay doesn't serve
	Clients     FlowClients            `json:"clients"`
	Hosts       map[string]FlowTraffic `json:"hosts"`
	HostClients map[string]FlowClients `json:"hostClients"` // per host, same buckets as Clients
	Streams     map[string]FlowTraffic `json:"streams"`     // requests = sessions
	LastDataAt  *time.Time             `json:"lastDataAt"`
}

// HostFlow is GET /api/metrics/topology/hosts/{id}?range=.
type HostFlow struct {
	HostID         string         `json:"hostId"`
	Range          string         `json:"range"`
	WindowSec      int64          `json:"windowSec"`
	GeneratedAt    time.Time      `json:"generatedAt"`
	Traffic        FlowTraffic    `json:"traffic"`
	Clients        FlowClients    `json:"clients"`
	TLSPct         float64        `json:"tlsPct"`
	RateLimitedPct float64        `json:"rateLimitedPct"`
	ErrorPct       float64        `json:"errorPct"`
	Timing         FlowTiming     `json:"timing"`
	Upstreams      []FlowUpstream `json:"upstreams"`
}

// FlowTiming holds mean per-request timings in milliseconds.
type FlowTiming struct {
	RequestMs  *float64 `json:"requestMs"`  // whole request at the proxy
	ConnectMs  *float64 `json:"connectMs"`  // proxy → upstream connect
	HeaderMs   *float64 `json:"headerMs"`   // until upstream response headers
	UpstreamMs *float64 `json:"upstreamMs"` // until the upstream response ended
}

// FlowUpstream is one upstream address the host proxied to.
type FlowUpstream struct {
	Addr       string   `json:"addr"`
	Requests   int64    `json:"requests"`
	SharePct   float64  `json:"sharePct"`
	Errors     int64    `json:"errors"`
	ResponseMs *float64 `json:"responseMs"`
}

func topologyRoutes(h *handlers, r chi.Router) {
	r.Get("/metrics/topology", h.metricsTopology)
	r.Get("/metrics/topology/hosts/{id}", h.metricsHostFlow)
}

func parseTopologyRange(r *http.Request) (string, time.Duration, error) {
	key := r.URL.Query().Get("range")
	if key == "" {
		key = "15m"
	}
	d, ok := topologyRanges[key]
	if !ok {
		return "", 0, badRequest("range must be 5m, 15m, 1h or 24h")
	}
	return key, d, nil
}

func (h *handlers) metricsTopology(w http.ResponseWriter, r *http.Request) {
	key, d, err := parseTopologyRange(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	t, err := buildTopology(r.Context(), h.app.Store, key, d, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h *handlers) metricsHostFlow(w http.ResponseWriter, r *http.Request) {
	key, d, err := parseTopologyRange(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	host, err := h.app.Store.Hosts().Get(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	f, err := buildHostFlow(ctx, h.app.Store, host.ID, key, d, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

// window returns the metrics_minute range covering the last d (the current
// minute included) and the seconds it spans so far.
func window(now time.Time, d time.Duration) (fromMin, toMin int64, seconds float64) {
	nowMin := now.Unix() / 60
	toMin = nowMin + 1
	fromMin = toMin - int64(d/time.Minute)
	seconds = now.Sub(time.Unix(fromMin*60, 0)).Seconds()
	if seconds < 1 {
		seconds = 1
	}
	return fromMin, toMin, seconds
}

func flowTraffic(t *store.MetricsTotals, seconds float64) FlowTraffic {
	if t == nil {
		return FlowTraffic{}
	}
	f := FlowTraffic{
		Requests: t.Requests, S4xx: t.S4xx, S5xx: t.S5xx, BytesIn: t.BytesIn, BytesOut: t.BytesOut,
		RPS: float64(t.Requests) / seconds, BytesPerSec: float64(t.BytesIn+t.BytesOut) / seconds,
	}
	if p, ok := Percentile(t.Hist, 0.5); ok {
		f.P50Ms = &p
	}
	if p, ok := Percentile(t.Hist, 0.95); ok {
		f.P95Ms = &p
	}
	return f
}

// flowClients sums rows into source buckets; Unique counts distinct addresses.
func flowClients(rows []store.ClientTraffic) FlowClients {
	var c FlowClients
	seen := map[string]bool{}
	for _, row := range rows {
		if !seen[row.IP] {
			seen[row.IP] = true
			c.Unique++
		}
		c.Requests += row.Requests
		c.Blocked += row.Blocked
		rest := row.Requests - row.Blocked
		switch ClientSource(row.IP) {
		case "lan":
			c.LAN += rest
		case "vpn":
			c.VPN += rest
		default:
			c.Internet += rest
		}
	}
	return c
}

func buildTopology(ctx context.Context, st *store.Store, key string, d time.Duration, now time.Time) (*Topology, error) {
	fromMin, toMin, secs := window(now, d)
	byKey, err := st.MetricsTotalsByKey(ctx, fromMin, toMin, true)
	if err != nil {
		return nil, err
	}
	t := &Topology{
		Range: key, WindowSec: int64(d / time.Second), GeneratedAt: now.UTC(),
		Totals: flowTraffic(byKey[""], secs), Unknown: flowTraffic(byKey[store.MetricsKeyUnknown], secs),
		Hosts: map[string]FlowTraffic{}, Streams: map[string]FlowTraffic{},
	}
	for k, v := range byKey {
		switch {
		case k == "" || k == store.MetricsKeyUnknown:
		case strings.HasPrefix(k, "stream:"):
			t.Streams[strings.TrimPrefix(k, "stream:")] = flowTraffic(v, secs)
		default:
			t.Hosts[k] = flowTraffic(v, secs)
		}
	}
	rows, err := st.ClientTrafficSince(ctx, time.Unix(fromMin*60, 0), "")
	if err != nil {
		return nil, err
	}
	t.Clients = flowClients(rows)
	byHost := map[string][]store.ClientTraffic{}
	for _, row := range rows {
		if row.HostID != "" {
			byHost[row.HostID] = append(byHost[row.HostID], row)
		}
	}
	t.HostClients = make(map[string]FlowClients, len(byHost))
	for id, hr := range byHost {
		t.HostClients[id] = flowClients(hr)
	}
	if t.LastDataAt, err = st.LastAccessTime(ctx); err != nil {
		return nil, err
	}
	return t, nil
}

func pct(n, of int64) float64 {
	if of <= 0 {
		return 0
	}
	return float64(n) / float64(of) * 100
}

func ms(sec *float64) *float64 {
	if sec == nil {
		return nil
	}
	v := *sec * 1000
	return &v
}

func buildHostFlow(ctx context.Context, st *store.Store, hostID, key string, d time.Duration, now time.Time) (*HostFlow, error) {
	fromMin, toMin, secs := window(now, d)
	tot, err := st.MetricsTotal(ctx, store.MetricsSelector{Host: hostID}, fromMin, toMin, true)
	if err != nil {
		return nil, err
	}
	since := time.Unix(fromMin*60, 0)
	rows, err := st.ClientTrafficSince(ctx, since, hostID)
	if err != nil {
		return nil, err
	}
	path, err := st.HostPathSince(ctx, hostID, since)
	if err != nil {
		return nil, err
	}
	f := &HostFlow{
		HostID: hostID, Range: key, WindowSec: int64(d / time.Second), GeneratedAt: now.UTC(),
		Traffic: flowTraffic(&tot, secs), Clients: flowClients(rows),
		TLSPct: pct(path.TLS, path.Requests), RateLimitedPct: pct(path.RateLimited, path.Requests), ErrorPct: pct(path.Errors, path.Requests),
		Timing:    FlowTiming{RequestMs: ms(path.Request), ConnectMs: ms(path.Connect), HeaderMs: ms(path.Header), UpstreamMs: ms(path.Upstream)},
		Upstreams: []FlowUpstream{},
	}
	var proxied int64
	for _, u := range path.Upstreams {
		proxied += u.Requests
	}
	for _, u := range path.Upstreams {
		f.Upstreams = append(f.Upstreams, FlowUpstream{Addr: u.Addr, Requests: u.Requests, SharePct: pct(u.Requests, proxied), Errors: u.Errors, ResponseMs: ms(u.Response)})
	}
	return f, nil
}
