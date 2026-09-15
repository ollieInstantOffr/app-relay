package store

import (
	"context"
	"time"
)

// MetricsTotalsByKey sums metrics_minute rows with fromMinute <= minute <
// toMinute per key (host id, "stream:<id>", "" for all HTTP, MetricsKeyUnknown).
func (s *Store) MetricsTotalsByKey(ctx context.Context, fromMinute, toMinute int64, withHist bool) (map[string]*MetricsTotals, error) {
	out := map[string]*MetricsTotals{}
	if toMinute <= fromMinute {
		return out, nil
	}
	histCol := "''"
	if withHist {
		histCol = "latency_hist"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT host, requests, s2xx, s3xx, s4xx, s5xx, bytes_out, bytes_in, `+histCol+`
		FROM metrics_minute WHERE minute >= ? AND minute < ?`, fromMinute, toMinute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, hist string
		var t MetricsTotals
		if err := rows.Scan(&key, &t.Requests, &t.S2xx, &t.S3xx, &t.S4xx, &t.S5xx, &t.BytesOut, &t.BytesIn, &hist); err != nil {
			return nil, err
		}
		t.Hist = parseHist(hist)
		cur := out[key]
		if cur == nil {
			cur = &MetricsTotals{}
			out[key] = cur
		}
		cur.add(t)
	}
	return out, rows.Err()
}

// ClientTraffic is the HTTP traffic of one client address to one host.
type ClientTraffic struct {
	HostID   string
	IP       string
	Requests int64
	Blocked  int64 // answered 403 or 444 (access lists, geo-blocking, blocklist, unknown hosts)
}

// ClientTrafficSince groups HTTP access_log rows since the given time by host
// and client address. hostID "" covers every host.
func (s *Store) ClientTrafficSince(ctx context.Context, since time.Time, hostID string) ([]ClientTraffic, error) {
	q := `SELECT host_id, client_ip, COUNT(*), COALESCE(SUM(CASE WHEN status IN (403, 444) THEN 1 ELSE 0 END), 0)
		FROM access_log WHERE kind = 'http' AND ts >= ?`
	args := []any{FormatLogTime(since)}
	if hostID != "" {
		q += ` AND host_id = ?`
		args = append(args, hostID)
	}
	rows, err := s.DB.QueryContext(ctx, q+` GROUP BY host_id, client_ip`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientTraffic
	for rows.Next() {
		var c ClientTraffic
		if err := rows.Scan(&c.HostID, &c.IP, &c.Requests, &c.Blocked); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HostPath summarises how one host's requests were handled since a time.
type HostPath struct {
	Requests    int64
	TLS         int64 // served over TLS
	RateLimited int64 // answered 429
	Errors      int64 // 5xx
	// Mean timings in seconds over proxied requests (nil without samples).
	Request   *float64
	Connect   *float64
	Header    *float64
	Upstream  *float64
	Upstreams []HostPathUpstream
}

// HostPathUpstream is the traffic to one upstream address.
type HostPathUpstream struct {
	Addr     string
	Requests int64
	Errors   int64
	Response *float64 // mean upstream response time, seconds
}

// HostPathSince returns HostPath for hostID since the given time.
func (s *Store) HostPathSince(ctx context.Context, hostID string, since time.Time) (HostPath, error) {
	var p HostPath
	ts := FormatLogTime(since)
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN ssl_protocol NOT IN ('', '-') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END), 0),
		AVG(CASE WHEN upstream_response_time IS NOT NULL THEN request_time END),
		AVG(upstream_connect_time), AVG(upstream_header_time), AVG(upstream_response_time)
		FROM access_log WHERE kind = 'http' AND host_id = ? AND ts >= ?`, hostID, ts).
		Scan(&p.Requests, &p.TLS, &p.RateLimited, &p.Errors, &p.Request, &p.Connect, &p.Header, &p.Upstream)
	if err != nil {
		return p, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT upstream_addr, COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END), 0), AVG(upstream_response_time)
		FROM access_log WHERE kind = 'http' AND host_id = ? AND ts >= ? AND upstream_addr NOT IN ('', '-')
		GROUP BY upstream_addr ORDER BY COUNT(*) DESC LIMIT 32`, hostID, ts)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var u HostPathUpstream
		if err := rows.Scan(&u.Addr, &u.Requests, &u.Errors, &u.Response); err != nil {
			return p, err
		}
		p.Upstreams = append(p.Upstreams, u)
	}
	return p, rows.Err()
}
