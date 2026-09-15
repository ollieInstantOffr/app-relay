package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Observe slice: access log queries.

// LogTimeLayout is the fixed-width UTC timestamp format used by access_log
// and error_log so that lexicographic order equals chronological order.
const LogTimeLayout = "2006-01-02T15:04:05.000Z"

func FormatLogTime(t time.Time) string { return t.UTC().Format(LogTimeLayout) }

// AccessRecord is one row of access_log (HTTP request or stream session).
type AccessRecord struct {
	ID                   int64
	TS                   time.Time
	Kind                 string // http | stream
	HostID               string // proxy host id, or stream id for streams
	Host                 string
	Method               string
	Path                 string
	Protocol             string
	Status               int
	ClientIP             string
	UpstreamAddr         string
	UpstreamStatus       string
	RequestTime          float64
	UpstreamConnectTime  *float64
	UpstreamHeaderTime   *float64
	UpstreamResponseTime *float64
	BytesSent            int64
	BytesReceived        int64
	UserAgent            string
	Referer              string
	RequestID            string
	SSLProtocol          string
	Extra                map[string]string
}

// StatusCond is one parsed status filter term.
type StatusCond struct {
	// Op is one of = != > >= < <= (compare with Value) or class / !class
	// (Value is the class digit 1–5).
	Op    string
	Value int
}

// AccessFilter selects access_log rows. Zero values are ignored.
type AccessFilter struct {
	HostID     string
	Host       string
	HostSuffix bool // match Host and any subdomain of it
	Status     []StatusCond
	ClientIP   string
	Method     string
	Search     string // substring of path, user agent, client ip, host or referer
	Kind       string
	Since      time.Time // inclusive
	Until      time.Time // exclusive
	BeforeID   int64
	ExcludeID  int64
	Limit      int
}

// StatusSQL renders status conditions: positive terms are OR'ed, negated
// terms are AND'ed, and both groups must hold.
func StatusSQL(conds []StatusCond) (string, []any) {
	var pos, neg []string
	var pargs, nargs []any
	for _, c := range conds {
		switch c.Op {
		case "class":
			pos = append(pos, "(status >= ? AND status < ?)")
			pargs = append(pargs, c.Value*100, c.Value*100+100)
		case "!class":
			neg = append(neg, "(status < ? OR status >= ?)")
			nargs = append(nargs, c.Value*100, c.Value*100+100)
		case "!=":
			neg = append(neg, "status != ?")
			nargs = append(nargs, c.Value)
		case "=", ">", ">=", "<", "<=":
			pos = append(pos, "status "+c.Op+" ?")
			pargs = append(pargs, c.Value)
		}
	}
	var parts []string
	var args []any
	if len(pos) > 0 {
		parts = append(parts, "("+strings.Join(pos, " OR ")+")")
		args = append(args, pargs...)
	}
	if len(neg) > 0 {
		parts = append(parts, strings.Join(neg, " AND "))
		args = append(args, nargs...)
	}
	return strings.Join(parts, " AND "), args
}

func (f AccessFilter) where() (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if f.HostID != "" {
		where = append(where, "host_id = ?")
		args = append(args, f.HostID)
	}
	if f.Host != "" {
		if f.HostSuffix {
			where = append(where, `(host = ? OR host LIKE ? ESCAPE '\')`)
			args = append(args, f.Host, "%."+LikeEscape(f.Host))
		} else {
			where = append(where, "host = ?")
			args = append(args, f.Host)
		}
	}
	if cond, sargs := StatusSQL(f.Status); cond != "" {
		where = append(where, cond)
		args = append(args, sargs...)
	}
	if f.ClientIP != "" {
		where = append(where, "client_ip = ?")
		args = append(args, f.ClientIP)
	}
	if f.Method != "" {
		where = append(where, "method = ?")
		args = append(args, f.Method)
	}
	if f.Search != "" {
		like := "%" + LikeEscape(f.Search) + "%"
		where = append(where, `(path LIKE ? ESCAPE '\' OR user_agent LIKE ? ESCAPE '\' OR client_ip LIKE ? ESCAPE '\' OR host LIKE ? ESCAPE '\' OR referer LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like, like)
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, FormatLogTime(f.Since))
	}
	if !f.Until.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, FormatLogTime(f.Until))
	}
	if f.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, f.BeforeID)
	}
	if f.ExcludeID > 0 {
		where = append(where, "id != ?")
		args = append(args, f.ExcludeID)
	}
	return strings.Join(where, " AND "), args
}

const accessCols = `id, ts, kind, host_id, host, method, path, protocol, status, client_ip, upstream_addr, upstream_status,
	request_time, upstream_connect_time, upstream_header_time, upstream_response_time, bytes_sent, bytes_received,
	user_agent, referer, request_id, ssl_protocol, extra`

type scanner interface{ Scan(dest ...any) error }

func scanAccess(sc scanner) (AccessRecord, error) {
	var r AccessRecord
	var ts, extra string
	var c, h, rt sql.NullFloat64
	err := sc.Scan(&r.ID, &ts, &r.Kind, &r.HostID, &r.Host, &r.Method, &r.Path, &r.Protocol, &r.Status, &r.ClientIP,
		&r.UpstreamAddr, &r.UpstreamStatus, &r.RequestTime, &c, &h, &rt, &r.BytesSent, &r.BytesReceived,
		&r.UserAgent, &r.Referer, &r.RequestID, &r.SSLProtocol, &extra)
	if err != nil {
		return r, err
	}
	r.TS = ParseTime(ts)
	r.UpstreamConnectTime = nullFloat(c)
	r.UpstreamHeaderTime = nullFloat(h)
	r.UpstreamResponseTime = nullFloat(rt)
	if extra != "" && extra != "{}" {
		_ = json.Unmarshal([]byte(extra), &r.Extra)
	}
	return r, nil
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// QueryAccess returns matching rows, newest first.
func (s *Store) QueryAccess(ctx context.Context, f AccessFilter) ([]AccessRecord, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	w, args := f.where()
	rows, err := s.DB.QueryContext(ctx, `SELECT `+accessCols+` FROM access_log WHERE `+w+` ORDER BY id DESC LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRecord{}
	for rows.Next() {
		r, err := scanAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetAccess(ctx context.Context, id int64) (*AccessRecord, error) {
	r, err := scanAccess(s.DB.QueryRowContext(ctx, `SELECT `+accessCols+` FROM access_log WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// StatusCounts is one histogram bucket.
type StatusCounts struct {
	Total int64 `json:"total"`
	S2xx  int64 `json:"s2xx"` // includes 1xx and other non-error codes
	S3xx  int64 `json:"s3xx"`
	S4xx  int64 `json:"s4xx"`
	S5xx  int64 `json:"s5xx"`
}

// AccessHistogram counts matching rows in n buckets of width step starting at
// start (f.Since/Until/BeforeID are overridden).
func (s *Store) AccessHistogram(ctx context.Context, f AccessFilter, start time.Time, step time.Duration, n int) ([]StatusCounts, error) {
	out := make([]StatusCounts, n)
	stepSec := int64(step / time.Second)
	if stepSec < 1 {
		stepSec = 1
	}
	f.Since = start
	f.Until = start.Add(time.Duration(stepSec*int64(n)) * time.Second)
	f.BeforeID = 0
	w, args := f.where()
	q := `SELECT (CAST(strftime('%s', ts) AS INTEGER) - ?) / ? AS b, COUNT(*),
		SUM(status >= 300 AND status < 400), SUM(status >= 400 AND status < 500), SUM(status >= 500)
		FROM access_log WHERE ` + w + ` GROUP BY b`
	rows, err := s.DB.QueryContext(ctx, q, append([]any{start.Unix(), stepSec}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b, total, s3, s4, s5 int64
		if err := rows.Scan(&b, &total, &s3, &s4, &s5); err != nil {
			return nil, err
		}
		if b < 0 || b >= int64(n) {
			continue
		}
		out[b] = StatusCounts{Total: total, S3xx: s3, S4xx: s4, S5xx: s5, S2xx: total - s3 - s4 - s5}
	}
	return out, rows.Err()
}

// StreamActivity aggregates recent stream sessions per stream id.
type StreamActivity struct {
	Clients int64 // distinct client IPs since clientsSince
	Bytes   int64 // bytes sent + received since bytesSince
}

func (s *Store) StreamActivity(ctx context.Context, clientsSince, bytesSince time.Time) (map[string]StreamActivity, error) {
	from := clientsSince
	if bytesSince.Before(from) {
		from = bytesSince
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT host_id,
		COUNT(DISTINCT CASE WHEN ts >= ? THEN client_ip END),
		COALESCE(SUM(CASE WHEN ts >= ? THEN bytes_sent + bytes_received END), 0)
		FROM access_log WHERE kind = 'stream' AND ts >= ? GROUP BY host_id`,
		FormatLogTime(clientsSince), FormatLogTime(bytesSince), FormatLogTime(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StreamActivity{}
	for rows.Next() {
		var id string
		var a StreamActivity
		if err := rows.Scan(&id, &a.Clients, &a.Bytes); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// LastAccessTime returns the timestamp of the newest access_log row.
func (s *Store) LastAccessTime(ctx context.Context) (*time.Time, error) {
	var ts string
	err := s.DB.QueryRowContext(ctx, `SELECT ts FROM access_log ORDER BY id DESC LIMIT 1`).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := ParseTime(ts)
	return &t, nil
}
