package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Observe slice: ingestion batches and per-minute traffic metrics.
//
// metrics_minute.host keys: "" = all HTTP traffic, "<host id>" = one proxy
// host, "_unknown" = HTTP requests that matched no host (default server),
// "stream:<id>" = one stream. latency_hist is a JSON array of bucket counts
// (bounds are defined by the logs package).

// MetricsKeyUnknown aggregates HTTP requests served by the default server.
const MetricsKeyUnknown = "_unknown"

// MetricsDelta is added to one (minute, host) row.
type MetricsDelta struct {
	Minute   int64
	Host     string
	Requests int64
	S2xx     int64
	S3xx     int64
	S4xx     int64
	S5xx     int64
	BytesOut int64
	BytesIn  int64
	Hist     []int64
}

// ObserveBatch is written atomically by the log ingester.
type ObserveBatch struct {
	Access  []AccessRecord // IDs are filled in on success
	Errors  []ErrorRecord  // IDs are filled in on success
	Metrics []MetricsDelta
	KV      map[string][]byte // e.g. tail offsets, committed with the rows
}

func (s *Store) WriteObserveBatch(ctx context.Context, b *ObserveBatch) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if len(b.Access) > 0 {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO access_log (ts, kind, host_id, host, method, path, protocol, status, client_ip,
				upstream_addr, upstream_status, request_time, upstream_connect_time, upstream_header_time, upstream_response_time,
				bytes_sent, bytes_received, user_agent, referer, request_id, ssl_protocol, extra)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for i := range b.Access {
				r := &b.Access[i]
				extra := "{}"
				if len(r.Extra) > 0 {
					if data, err := json.Marshal(r.Extra); err == nil {
						extra = string(data)
					}
				}
				res, err := stmt.ExecContext(ctx, FormatLogTime(r.TS), r.Kind, r.HostID, r.Host, r.Method, r.Path, r.Protocol, r.Status,
					r.ClientIP, r.UpstreamAddr, r.UpstreamStatus, r.RequestTime, r.UpstreamConnectTime, r.UpstreamHeaderTime,
					r.UpstreamResponseTime, r.BytesSent, r.BytesReceived, r.UserAgent, r.Referer, r.RequestID, r.SSLProtocol, extra)
				if err != nil {
					return err
				}
				r.ID, _ = res.LastInsertId()
			}
		}
		if len(b.Errors) > 0 {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO error_log (ts, source, level, message) VALUES (?, ?, ?, ?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for i := range b.Errors {
				r := &b.Errors[i]
				res, err := stmt.ExecContext(ctx, FormatLogTime(r.TS), r.Source, r.Level, r.Message)
				if err != nil {
					return err
				}
				r.ID, _ = res.LastInsertId()
			}
		}
		for _, d := range b.Metrics {
			var prev string
			err := tx.QueryRowContext(ctx, `SELECT latency_hist FROM metrics_minute WHERE minute = ? AND host = ?`, d.Minute, d.Host).Scan(&prev)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			hist := AddHist(parseHist(prev), d.Hist)
			if hist == nil {
				hist = []int64{}
			}
			histJSON, _ := json.Marshal(hist)
			if _, err := tx.ExecContext(ctx, `INSERT INTO metrics_minute (minute, host, requests, s2xx, s3xx, s4xx, s5xx, bytes_out, bytes_in, latency_hist)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(minute, host) DO UPDATE SET requests = requests + excluded.requests, s2xx = s2xx + excluded.s2xx,
				s3xx = s3xx + excluded.s3xx, s4xx = s4xx + excluded.s4xx, s5xx = s5xx + excluded.s5xx,
				bytes_out = bytes_out + excluded.bytes_out, bytes_in = bytes_in + excluded.bytes_in, latency_hist = excluded.latency_hist`,
				d.Minute, d.Host, d.Requests, d.S2xx, d.S3xx, d.S4xx, d.S5xx, d.BytesOut, d.BytesIn, string(histJSON)); err != nil {
				return err
			}
		}
		for k, v := range b.KV {
			if _, err := tx.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func parseHist(s string) []int64 {
	if s == "" || s == "[]" {
		return nil
	}
	var h []int64
	_ = json.Unmarshal([]byte(s), &h)
	return h
}

// AddHist adds b into a element-wise (growing a as needed) and returns it.
func AddHist(a, b []int64) []int64 {
	if len(b) > len(a) {
		a = append(a, make([]int64, len(b)-len(a))...)
	}
	for i, v := range b {
		a[i] += v
	}
	return a
}

// MetricsTotals are summed metrics_minute rows.
type MetricsTotals struct {
	Requests int64
	S2xx     int64
	S3xx     int64
	S4xx     int64
	S5xx     int64
	BytesOut int64
	BytesIn  int64
	Hist     []int64
}

func (t *MetricsTotals) add(o MetricsTotals) {
	t.Requests += o.Requests
	t.S2xx += o.S2xx
	t.S3xx += o.S3xx
	t.S4xx += o.S4xx
	t.S5xx += o.S5xx
	t.BytesOut += o.BytesOut
	t.BytesIn += o.BytesIn
	t.Hist = AddHist(t.Hist, o.Hist)
}

// MetricsSelector picks metrics_minute keys.
type MetricsSelector struct {
	Host           string // exact key ("" = all HTTP)
	IncludeStreams bool   // also add every "stream:*" key
}

func (m MetricsSelector) where() (string, []any) {
	if m.IncludeStreams {
		return `(host = ? OR host LIKE 'stream:%')`, []any{m.Host}
	}
	return `host = ?`, []any{m.Host}
}

// MetricsBuckets sums rows with fromMinute <= minute < fromMinute+step*n into
// n buckets of stepMinutes.
func (s *Store) MetricsBuckets(ctx context.Context, sel MetricsSelector, fromMinute, stepMinutes int64, n int, withHist bool) ([]MetricsTotals, error) {
	out := make([]MetricsTotals, n)
	if stepMinutes < 1 {
		stepMinutes = 1
	}
	w, args := sel.where()
	histCol := "''"
	if withHist {
		histCol = "latency_hist"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT minute, requests, s2xx, s3xx, s4xx, s5xx, bytes_out, bytes_in, `+histCol+`
		FROM metrics_minute WHERE minute >= ? AND minute < ? AND `+w,
		append([]any{fromMinute, fromMinute + stepMinutes*int64(n)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var minute int64
		var t MetricsTotals
		var hist string
		if err := rows.Scan(&minute, &t.Requests, &t.S2xx, &t.S3xx, &t.S4xx, &t.S5xx, &t.BytesOut, &t.BytesIn, &hist); err != nil {
			return nil, err
		}
		t.Hist = parseHist(hist)
		i := (minute - fromMinute) / stepMinutes
		if i >= 0 && i < int64(n) {
			out[i].add(t)
		}
	}
	return out, rows.Err()
}

// MetricsTotal sums rows with fromMinute <= minute < toMinute.
func (s *Store) MetricsTotal(ctx context.Context, sel MetricsSelector, fromMinute, toMinute int64, withHist bool) (MetricsTotals, error) {
	if toMinute <= fromMinute {
		return MetricsTotals{}, nil
	}
	b, err := s.MetricsBuckets(ctx, sel, fromMinute, toMinute-fromMinute, 1, withHist)
	if err != nil {
		return MetricsTotals{}, err
	}
	return b[0], nil
}

// HostSeries holds per proxy host request counts.
type HostSeries struct {
	Total  int64   // fromMinute..toMinute
	Series []int64 // n buckets of stepMinutes starting at seriesFrom
}

// MetricsHostSeries returns request series for every proxy host key.
func (s *Store) MetricsHostSeries(ctx context.Context, seriesFrom, stepMinutes int64, n int, totalFrom, totalTo int64) (map[string]*HostSeries, error) {
	from := seriesFrom
	if totalFrom < from {
		from = totalFrom
	}
	to := seriesFrom + stepMinutes*int64(n)
	if totalTo > to {
		to = totalTo
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT host, minute, requests FROM metrics_minute
		WHERE minute >= ? AND minute < ? AND host != '' AND host NOT LIKE 'stream:%' AND host != ?`, from, to, MetricsKeyUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*HostSeries{}
	for rows.Next() {
		var host string
		var minute, req int64
		if err := rows.Scan(&host, &minute, &req); err != nil {
			return nil, err
		}
		hs := out[host]
		if hs == nil {
			hs = &HostSeries{Series: make([]int64, n)}
			out[host] = hs
		}
		if minute >= totalFrom && minute < totalTo {
			hs.Total += req
		}
		if i := (minute - seriesFrom) / stepMinutes; minute >= seriesFrom && i < int64(n) {
			hs.Series[i] += req
		}
	}
	return out, rows.Err()
}

// StreamIDFromMetricsKey returns the stream id of a "stream:<id>" key.
func StreamIDFromMetricsKey(key string) (string, bool) {
	return strings.CutPrefix(key, "stream:")
}
