package edge

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// metrics are lock-free counters behind /stub_status and /metrics.
type metrics struct {
	accepted atomic.Uint64
	requests atomic.Uint64
	active   atomic.Int64 // open HTTP connections
	reading  atomic.Int64 // connected, no request yet
	waiting  atomic.Int64 // keep-alive idle
	writing  atomic.Int64 // requests in progress (all protocols)

	reloadsOK     atomic.Uint64
	reloadsFailed atomic.Uint64
	certReloads   atomic.Uint64

	hosts   sync.Map // host id → *hostMetrics
	streams sync.Map // stream id → *streamMetrics
}

var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type hostMetrics struct {
	byClass   [6]atomic.Uint64 // 1xx…5xx, other
	buckets   [len(durationBuckets) + 1]atomic.Uint64
	sumMicros atomic.Uint64
	sent      atomic.Uint64
	received  atomic.Uint64
	upConnect atomic.Uint64
	upTimeout atomic.Uint64
	upOther   atomic.Uint64
}

type streamMetrics struct {
	ok, connectFailed, failed atomic.Uint64
	sent, received            atomic.Uint64
}

func newMetrics() *metrics { return &metrics{} }

func (m *metrics) host(id string) *hostMetrics {
	if v, ok := m.hosts.Load(id); ok {
		return v.(*hostMetrics)
	}
	v, _ := m.hosts.LoadOrStore(id, &hostMetrics{})
	return v.(*hostMetrics)
}

func (m *metrics) stream(id string) *streamMetrics {
	if v, ok := m.streams.Load(id); ok {
		return v.(*streamMetrics)
	}
	v, _ := m.streams.LoadOrStore(id, &streamMetrics{})
	return v.(*streamMetrics)
}

func (hm *hostMetrics) observe(status int, d time.Duration, sent, received int64) {
	class := status/100 - 1
	if class < 0 || class > 4 {
		class = 5
	}
	hm.byClass[class].Add(1)
	secs := d.Seconds()
	i := sort.SearchFloat64s(durationBuckets[:], secs)
	hm.buckets[i].Add(1)
	hm.sumMicros.Add(uint64(d.Microseconds()))
	hm.sent.Add(uint64(sent))
	hm.received.Add(uint64(received))
}

// trackedConn carries the http.ConnState of a connection so the gauges can
// move between states without a map lookup.
type trackedConn struct {
	net.Conn
	state atomic.Int32
}

type trackedListener struct{ net.Listener }

func (l trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c}
	tc.state.Store(-1)
	return tc, nil
}

func (m *metrics) connState(c net.Conn, st http.ConnState) {
	if tlsConn, ok := c.(*tls.Conn); ok {
		c = tlsConn.NetConn()
	}
	tc, ok := c.(*trackedConn)
	if !ok {
		return
	}
	prev := http.ConnState(tc.state.Swap(int32(st)))
	switch prev {
	case http.StateNew:
		m.reading.Add(-1)
	case http.StateIdle:
		m.waiting.Add(-1)
	}
	switch st {
	case http.StateNew:
		m.accepted.Add(1)
		m.active.Add(1)
		m.reading.Add(1)
	case http.StateIdle:
		m.waiting.Add(1)
	case http.StateHijacked, http.StateClosed:
		if prev != http.StateHijacked && prev != http.StateClosed {
			m.active.Add(-1)
		}
	}
}

func (s *Server) statusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			rt := s.table.Load()
			w.Header().Set("Content-Type", "application/json")
			if rt == nil || s.stopping.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "{\"status\":\"stopping\"}\n")
				return
			}
			fmt.Fprintf(w, "{\"status\":\"ok\",\"hash\":%s}\n", appendJSONString(nil, rt.hash))
		case "/stub_status":
			m := s.metrics
			w.Header().Set("Content-Type", "text/plain")
			acc := m.accepted.Load()
			fmt.Fprintf(w, "Active connections: %d \nserver accepts handled requests\n %d %d %d \nReading: %d Writing: %d Waiting: %d \n",
				max(m.active.Load(), 0), acc, acc, m.requests.Load(), max(m.reading.Load(), 0), max(m.writing.Load(), 0), max(m.waiting.Load(), 0))
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			bw := bufio.NewWriter(w)
			s.writeMetrics(bw)
			bw.Flush()
		default:
			http.NotFound(w, r)
		}
	})
}

func promLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func (s *Server) writeMetrics(w *bufio.Writer) {
	m := s.metrics
	header := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	type hostRow struct {
		id string
		m  *hostMetrics
	}
	var hosts []hostRow
	m.hosts.Range(func(k, v any) bool {
		hosts = append(hosts, hostRow{k.(string), v.(*hostMetrics)})
		return true
	})
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].id < hosts[j].id })

	classes := [...]string{"1xx", "2xx", "3xx", "4xx", "5xx", "other"}
	header("relay_edge_http_requests_total", "counter", "HTTP requests by host and status class.")
	for _, h := range hosts {
		for i, cl := range classes {
			if n := h.m.byClass[i].Load(); n > 0 {
				fmt.Fprintf(w, "relay_edge_http_requests_total{host_id=\"%s\",code=\"%s\"} %d\n", promLabel(h.id), cl, n)
			}
		}
	}
	header("relay_edge_http_request_duration_seconds", "histogram", "HTTP request duration by host.")
	for _, h := range hosts {
		id := promLabel(h.id)
		var cum uint64
		for i, le := range durationBuckets {
			cum += h.m.buckets[i].Load()
			fmt.Fprintf(w, "relay_edge_http_request_duration_seconds_bucket{host_id=\"%s\",le=\"%g\"} %d\n", id, le, cum)
		}
		cum += h.m.buckets[len(durationBuckets)].Load()
		fmt.Fprintf(w, "relay_edge_http_request_duration_seconds_bucket{host_id=\"%s\",le=\"+Inf\"} %d\n", id, cum)
		fmt.Fprintf(w, "relay_edge_http_request_duration_seconds_sum{host_id=\"%s\"} %g\n", id, float64(h.m.sumMicros.Load())/1e6)
		fmt.Fprintf(w, "relay_edge_http_request_duration_seconds_count{host_id=\"%s\"} %d\n", id, cum)
	}
	header("relay_edge_http_response_bytes_total", "counter", "Response body bytes sent by host.")
	for _, h := range hosts {
		fmt.Fprintf(w, "relay_edge_http_response_bytes_total{host_id=\"%s\"} %d\n", promLabel(h.id), h.m.sent.Load())
	}
	header("relay_edge_http_request_bytes_total", "counter", "Request bytes received by host.")
	for _, h := range hosts {
		fmt.Fprintf(w, "relay_edge_http_request_bytes_total{host_id=\"%s\"} %d\n", promLabel(h.id), h.m.received.Load())
	}
	header("relay_edge_upstream_errors_total", "counter", "Failed upstream requests by host and kind.")
	for _, h := range hosts {
		id := promLabel(h.id)
		for _, kv := range [...]struct {
			kind string
			n    uint64
		}{{"connect", h.m.upConnect.Load()}, {"timeout", h.m.upTimeout.Load()}, {"other", h.m.upOther.Load()}} {
			if kv.n > 0 {
				fmt.Fprintf(w, "relay_edge_upstream_errors_total{host_id=\"%s\",kind=\"%s\"} %d\n", id, kv.kind, kv.n)
			}
		}
	}

	header("relay_edge_connections_active", "gauge", "Open HTTP connections.")
	fmt.Fprintf(w, "relay_edge_connections_active %d\n", max(m.active.Load(), 0))
	header("relay_edge_connections_accepted_total", "counter", "Accepted HTTP connections.")
	fmt.Fprintf(w, "relay_edge_connections_accepted_total %d\n", m.accepted.Load())
	header("relay_edge_requests_in_flight", "gauge", "Requests being processed.")
	fmt.Fprintf(w, "relay_edge_requests_in_flight %d\n", max(m.writing.Load(), 0))

	header("relay_edge_config_reloads_total", "counter", "Configuration reloads by result.")
	fmt.Fprintf(w, "relay_edge_config_reloads_total{result=\"success\"} %d\n", m.reloadsOK.Load())
	fmt.Fprintf(w, "relay_edge_config_reloads_total{result=\"failure\"} %d\n", m.reloadsFailed.Load())
	header("relay_edge_certificate_reloads_total", "counter", "Certificates reloaded after changing on disk.")
	fmt.Fprintf(w, "relay_edge_certificate_reloads_total %d\n", m.certReloads.Load())
	if rt := s.table.Load(); rt != nil {
		header("relay_edge_config_info", "gauge", "Active configuration.")
		fmt.Fprintf(w, "relay_edge_config_info{hash=\"%s\"} 1\n", promLabel(rt.hash))
	}

	type streamRow struct {
		id string
		m  *streamMetrics
	}
	var streams []streamRow
	m.streams.Range(func(k, v any) bool {
		streams = append(streams, streamRow{k.(string), v.(*streamMetrics)})
		return true
	})
	sort.Slice(streams, func(i, j int) bool { return streams[i].id < streams[j].id })
	header("relay_edge_stream_sessions_total", "counter", "Finished stream sessions by stream and status.")
	for _, st := range streams {
		id := promLabel(st.id)
		fmt.Fprintf(w, "relay_edge_stream_sessions_total{stream_id=\"%s\",status=\"200\"} %d\n", id, st.m.ok.Load())
		fmt.Fprintf(w, "relay_edge_stream_sessions_total{stream_id=\"%s\",status=\"502\"} %d\n", id, st.m.connectFailed.Load())
		fmt.Fprintf(w, "relay_edge_stream_sessions_total{stream_id=\"%s\",status=\"500\"} %d\n", id, st.m.failed.Load())
	}
	header("relay_edge_stream_bytes_total", "counter", "Stream bytes by stream and direction.")
	for _, st := range streams {
		id := promLabel(st.id)
		fmt.Fprintf(w, "relay_edge_stream_bytes_total{stream_id=\"%s\",direction=\"sent\"} %d\n", id, st.m.sent.Load())
		fmt.Fprintf(w, "relay_edge_stream_bytes_total{stream_id=\"%s\",direction=\"received\"} %d\n", id, st.m.received.Load())
	}

	c := s.cache
	header("relay_edge_asset_cache_requests_total", "counter", "Asset cache lookups by result.")
	fmt.Fprintf(w, "relay_edge_asset_cache_requests_total{result=\"hit\"} %d\n", c.hits.Load())
	fmt.Fprintf(w, "relay_edge_asset_cache_requests_total{result=\"miss\"} %d\n", c.misses.Load())
	fmt.Fprintf(w, "relay_edge_asset_cache_requests_total{result=\"stale\"} %d\n", c.stale.Load())
	header("relay_edge_asset_cache_bytes", "gauge", "Bytes held by the asset cache.")
	fmt.Fprintf(w, "relay_edge_asset_cache_bytes %d\n", c.bytes())

	header("relay_edge_log_lines_dropped_total", "counter", "Log lines dropped because a log queue was full.")
	var accessDropped, streamDropped uint64
	if l := s.accessLog.Load(); l != nil {
		accessDropped = l.dropped.Load()
	}
	if l := s.streamLog.Load(); l != nil {
		streamDropped = l.dropped.Load()
	}
	fmt.Fprintf(w, "relay_edge_log_lines_dropped_total{log=\"access\"} %d\n", accessDropped+s.droppedBefore.Load())
	fmt.Fprintf(w, "relay_edge_log_lines_dropped_total{log=\"stream\"} %d\n", streamDropped)
	fmt.Fprintf(w, "relay_edge_log_lines_dropped_total{log=\"error\"} %d\n", s.errlog.dropped())
}
