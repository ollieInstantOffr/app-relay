package logs

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/store"
)

// GET /api/metrics/overview?range=1h|24h|7d response.
type Overview struct {
	Range           string          `json:"range"`
	Hosts           OverviewHosts   `json:"hosts"`
	Requests        OverviewReqs    `json:"requests"`
	Certificates    OverviewCerts   `json:"certificates"`
	Upstream5xx     Overview5xx     `json:"upstream5xx"`
	Traffic         OverviewTraffic `json:"traffic"`
	Latency         OverviewLatency `json:"latency"`
	BandwidthBytes  int64           `json:"bandwidthBytes"`
	UnknownHostHits int64           `json:"unknownHostHits"`
	// Nginx describes the active proxy engine (nginx or Relay Edge, see
	// ProxyEngine); the field name is kept for compatibility.
	Nginx       OverviewNginx `json:"nginx"`
	ProxyEngine string        `json:"proxyEngine"` // nginx | edge
	LastDataAt  *time.Time    `json:"lastDataAt"`
}

type OverviewHosts struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Down     int `json:"down"`
	Degraded int `json:"degraded"`
	Disabled int `json:"disabled"`
	Unknown  int `json:"unknown"`
}

type OverviewReqs struct {
	Total         int64    `json:"total"`
	PreviousTotal int64    `json:"previousTotal"`
	DeltaPct      *float64 `json:"deltaPct"` // null when the previous period had no traffic
}

type OverviewCerts struct {
	Total        int `json:"total"`
	ExpiringSoon int `json:"expiringSoon"` // valid, expiring within 14 days
	Expired      int `json:"expired"`
}

type Overview5xx struct {
	Pct   float64 `json:"pct"`
	Count int64   `json:"count"`
}

type TrafficBucket struct {
	T        time.Time `json:"t"`
	Requests int64     `json:"requests"`
	S5xx     int64     `json:"s5xx"`
}

type OverviewTraffic struct {
	Buckets     []TrafficBucket `json:"buckets"`
	StepSeconds int64           `json:"stepSeconds"`
}

type OverviewLatency struct {
	P50Ms *float64 `json:"p50Ms"`
	P95Ms *float64 `json:"p95Ms"`
}

type OverviewNginx struct {
	Version   string `json:"version"`
	UptimeSec *int64 `json:"uptimeSec"`
	Running   bool   `json:"running"`
	Reachable bool   `json:"reachable"`
}

type rangeSpec struct {
	dur     time.Duration
	step    time.Duration
	buckets int
}

var overviewRanges = map[string]rangeSpec{
	"1h":  {time.Hour, time.Minute, 60},
	"24h": {24 * time.Hour, time.Hour, 24},
	"7d":  {7 * 24 * time.Hour, 6 * time.Hour, 28},
}

const certExpiringWindow = 14 * 24 * time.Hour

func (h *handlers) metricsOverview(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("range")
	if key == "" {
		key = "24h"
	}
	spec, ok := overviewRanges[key]
	if !ok {
		httpx.Fail(w, r, badRequest("range must be 1h, 24h or 7d"))
		return
	}
	ov, err := buildOverview(r.Context(), h.app, key, spec, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, ov)
}

func buildOverview(ctx context.Context, app *core.App, key string, spec rangeSpec, now time.Time) (*Overview, error) {
	st := app.Store
	ov := &Overview{Range: key}

	// Traffic totals over exactly the last range, compared with the one before.
	nowMin := now.Unix() / 60
	endMin := nowMin + 1
	durMin := int64(spec.dur / time.Minute)
	fromMin := endMin - durMin
	httpSel := store.MetricsSelector{Host: ""}
	cur, err := st.MetricsTotal(ctx, httpSel, fromMin, endMin, true)
	if err != nil {
		return nil, err
	}
	prev, err := st.MetricsTotal(ctx, httpSel, fromMin-durMin, fromMin, false)
	if err != nil {
		return nil, err
	}
	all, err := st.MetricsTotal(ctx, store.MetricsSelector{Host: "", IncludeStreams: true}, fromMin, endMin, false)
	if err != nil {
		return nil, err
	}
	unknown, err := st.MetricsTotal(ctx, store.MetricsSelector{Host: store.MetricsKeyUnknown}, fromMin, endMin, false)
	if err != nil {
		return nil, err
	}
	ov.Requests = OverviewReqs{Total: cur.Requests, PreviousTotal: prev.Requests}
	if prev.Requests > 0 {
		d := (float64(cur.Requests) - float64(prev.Requests)) / float64(prev.Requests) * 100
		ov.Requests.DeltaPct = &d
	}
	ov.Upstream5xx.Count = cur.S5xx
	if cur.Requests > 0 {
		ov.Upstream5xx.Pct = float64(cur.S5xx) / float64(cur.Requests) * 100
	}
	if p, ok := Percentile(cur.Hist, 0.5); ok {
		ov.Latency.P50Ms = &p
	}
	if p, ok := Percentile(cur.Hist, 0.95); ok {
		ov.Latency.P95Ms = &p
	}
	ov.BandwidthBytes = all.BytesOut + all.BytesIn
	ov.UnknownHostHits = unknown.Requests

	// Series aligned to step boundaries; the last bucket is the current one.
	stepMin := int64(spec.step / time.Minute)
	bEnd := (nowMin/stepMin + 1) * stepMin
	bStart := bEnd - stepMin*int64(spec.buckets)
	buckets, err := st.MetricsBuckets(ctx, httpSel, bStart, stepMin, spec.buckets, false)
	if err != nil {
		return nil, err
	}
	ov.Traffic = OverviewTraffic{StepSeconds: stepMin * 60, Buckets: make([]TrafficBucket, len(buckets))}
	for i, b := range buckets {
		ov.Traffic.Buckets[i] = TrafficBucket{T: time.Unix((bStart+int64(i)*stepMin)*60, 0).UTC(), Requests: b.Requests, S5xx: b.S5xx}
	}

	// Proxy engine (nginx or Relay Edge)
	ov.ProxyEngine = app.ProxyEngine(ctx)
	if app.Engine != nil {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		es, err := app.Engine.Status(sctx)
		cancel()
		if err == nil && es != nil {
			if es.Proxy != "" {
				ov.ProxyEngine = es.Proxy
			}
			n := es.Nginx
			if ov.ProxyEngine == "edge" {
				n = es.Edge
			}
			ov.Nginx = OverviewNginx{Version: n.Version, Running: n.Running, Reachable: n.Reachable}
			if n.Running && n.StartedAt != nil {
				up := int64(now.Sub(*n.StartedAt).Seconds())
				ov.Nginx.UptimeSec = &up
			}
		}
	}

	// Hosts & health
	hosts, err := st.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	ov.Hosts.Total = len(hosts)
	nginxDown := ov.Nginx.Reachable && !ov.Nginx.Running
	for _, ph := range hosts {
		if !ph.Enabled {
			ov.Hosts.Disabled++
			continue
		}
		if nginxDown {
			ov.Hosts.Down++
			continue
		}
		status := core.HealthUnknown
		if app.Health != nil {
			if hs, ok := app.Health.Get(core.HostTarget(ph.ID)); ok {
				status = hs.Status
			}
		}
		switch status {
		case core.HealthHealthy:
			ov.Hosts.Healthy++
		case core.HealthDegraded:
			ov.Hosts.Degraded++
		case core.HealthDown:
			ov.Hosts.Down++
		default:
			ov.Hosts.Unknown++
		}
	}

	// Certificates
	certs, err := st.Certificates().List(ctx)
	if err != nil {
		return nil, err
	}
	ov.Certificates.Total = len(certs)
	for _, c := range certs {
		if c.NotAfter == nil {
			continue
		}
		switch left := c.NotAfter.Sub(now); {
		case left <= 0:
			ov.Certificates.Expired++
		case left <= certExpiringWindow:
			ov.Certificates.ExpiringSoon++
		}
	}

	ov.LastDataAt, err = st.LastAccessTime(ctx)
	if err != nil {
		return nil, err
	}
	return ov, nil
}

// HostMetric is one entry of GET /api/metrics/hosts.
type HostMetric struct {
	Requests24h int64   `json:"requests24h"`
	Series      []int64 `json:"series"` // 24 hourly buckets, oldest first; the last is the current hour
}

func (h *handlers) metricsHosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nowMin := time.Now().Unix() / 60
	seriesEnd := (nowMin/60 + 1) * 60
	series, err := h.app.Store.MetricsHostSeries(ctx, seriesEnd-24*60, 60, 24, nowMin+1-24*60, nowMin+1)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hosts, err := h.app.Store.Hosts().List(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make(map[string]HostMetric, len(hosts))
	for _, ph := range hosts {
		m := HostMetric{Series: make([]int64, 24)}
		if s := series[ph.ID]; s != nil {
			m.Requests24h, m.Series = s.Total, s.Series
		}
		out[ph.ID] = m
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// StreamMetric is one entry of GET /api/metrics/streams.
type StreamMetric struct {
	Active      int     `json:"active"`      // TCP: established connections on the listen ports; UDP: distinct clients in the last 5 min
	BytesPerSec float64 `json:"bytesPerSec"` // bytes of sessions that ended in the last 60 s, averaged per second
}

func (h *handlers) metricsStreams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	streams, err := h.app.Store.Streams().List(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	act, err := h.app.Store.StreamActivity(ctx, now.Add(-streamClientWindow), now.Add(-streamBytesWindow))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	conns := establishedByPort()
	out := make(map[string]StreamMetric, len(streams))
	for _, s := range streams {
		m := StreamMetric{BytesPerSec: float64(act[s.ID].Bytes) / streamBytesWindow.Seconds()}
		if s.Enabled {
			if strings.EqualFold(s.Protocol, "udp") {
				m.Active = int(act[s.ID].Clients)
			} else {
				for _, p := range parsePorts(s.ListenPorts) {
					m.Active += conns[p]
				}
			}
		}
		out[s.ID] = m
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// parsePorts parses "25565", "2456-2458" or "80,443".
func parsePorts(spec string) []int {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || a < 1 || a > 65535 {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil || b < a || b > 65535 {
				continue
			}
		}
		for p := a; p <= b && p-a < 1024; p++ {
			out = append(out, p)
		}
	}
	return out
}

var procNetTCP = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// establishedByPort counts ESTABLISHED TCP sockets per local port. Relay
// runs with host networking, so this sees nginx's client connections.
func establishedByPort() map[int]int {
	out := map[int]int{}
	for _, f := range procNetTCP {
		if data, err := os.ReadFile(f); err == nil {
			parseProcNetTCP(string(data), out)
		}
	}
	return out
}

// parseProcNetTCP parses the /proc/net/tcp{,6} format:
//
//	sl  local_address rem_address   st tx_queue …
//	 0: 0100007F:0CEA 00000000:0000 01 …
func parseProcNetTCP(data string, out map[int]int) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if strings.HasSuffix(fields[0], ":") {
			fields = fields[1:]
		}
		if len(fields) < 3 {
			continue
		}
		state, err := strconv.ParseUint(fields[2], 16, 8)
		if err != nil || state != 1 { // TCP_ESTABLISHED
			continue
		}
		local := fields[0]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.ParseUint(local[i+1:], 16, 16)
		if err != nil {
			continue
		}
		out[int(port)]++
	}
}
