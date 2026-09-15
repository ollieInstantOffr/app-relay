package store

import (
	"context"
	"time"
)

// Observe slice: health check state and retention.

type HealthRecord struct {
	Target     string
	Status     string
	LatencyMs  int64
	HTTPStatus int
	Detail     string
	CheckedAt  time.Time
	ChangedAt  time.Time
}

func (s *Store) ListHealthChecks(ctx context.Context) ([]HealthRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT target, status, latency_ms, http_status, detail, checked_at, changed_at FROM health_checks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HealthRecord{}
	for rows.Next() {
		var r HealthRecord
		var checked, changed string
		if err := rows.Scan(&r.Target, &r.Status, &r.LatencyMs, &r.HTTPStatus, &r.Detail, &checked, &changed); err != nil {
			return nil, err
		}
		r.CheckedAt = ParseTime(checked)
		r.ChangedAt = ParseTime(changed)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertHealthCheck(ctx context.Context, r HealthRecord) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO health_checks (target, status, latency_ms, http_status, detail, checked_at, changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(target) DO UPDATE SET status = excluded.status, latency_ms = excluded.latency_ms, http_status = excluded.http_status,
		detail = excluded.detail, checked_at = excluded.checked_at, changed_at = excluded.changed_at`,
		r.Target, r.Status, r.LatencyMs, r.HTTPStatus, r.Detail, FormatTime(r.CheckedAt), FormatTime(r.ChangedAt))
	return err
}

func (s *Store) DeleteHealthCheck(ctx context.Context, target string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM health_checks WHERE target = ?`, target)
	return err
}

// RetentionPolicy for operational data owned by the observe slice.
type RetentionPolicy struct {
	AccessMaxAge   time.Duration
	AccessMaxRows  int64
	ErrorMaxAge    time.Duration
	MetricsMaxAge  time.Duration
	ActivityMaxAge time.Duration
}

const pruneChunk = 20000

// PruneObserve deletes expired rows in small chunks (so ingestion is never
// blocked for long) and returns the number of rows removed per table.
func (s *Store) PruneObserve(ctx context.Context, now time.Time, p RetentionPolicy) (map[string]int64, error) {
	out := map[string]int64{}
	chunked := func(table, query string, args ...any) error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			res, err := s.DB.ExecContext(ctx, query, append(args, pruneChunk)...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			out[table] += n
			if n < pruneChunk {
				return nil
			}
		}
	}
	if p.AccessMaxAge > 0 {
		if err := chunked("access_log", `DELETE FROM access_log WHERE id IN (SELECT id FROM access_log WHERE ts < ? ORDER BY id LIMIT ?)`,
			FormatLogTime(now.Add(-p.AccessMaxAge))); err != nil {
			return out, err
		}
	}
	if p.AccessMaxRows > 0 {
		var count int64
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_log`).Scan(&count); err != nil {
			return out, err
		}
		if count > p.AccessMaxRows {
			var cutoff int64
			if err := s.DB.QueryRowContext(ctx, `SELECT id FROM access_log ORDER BY id DESC LIMIT 1 OFFSET ?`, p.AccessMaxRows).Scan(&cutoff); err == nil {
				if err := chunked("access_log", `DELETE FROM access_log WHERE id IN (SELECT id FROM access_log WHERE id <= ? ORDER BY id LIMIT ?)`, cutoff); err != nil {
					return out, err
				}
			}
		}
	}
	if p.ErrorMaxAge > 0 {
		if err := chunked("error_log", `DELETE FROM error_log WHERE id IN (SELECT id FROM error_log WHERE ts < ? ORDER BY id LIMIT ?)`,
			FormatLogTime(now.Add(-p.ErrorMaxAge))); err != nil {
			return out, err
		}
	}
	if p.MetricsMaxAge > 0 {
		if err := chunked("metrics_minute", `DELETE FROM metrics_minute WHERE rowid IN (SELECT rowid FROM metrics_minute WHERE minute < ? LIMIT ?)`,
			now.Add(-p.MetricsMaxAge).Unix()/60); err != nil {
			return out, err
		}
	}
	if p.ActivityMaxAge > 0 {
		if err := chunked("activity", `DELETE FROM activity WHERE id IN (SELECT id FROM activity WHERE at < ? ORDER BY id LIMIT ?)`,
			FormatTime(now.Add(-p.ActivityMaxAge))); err != nil {
			return out, err
		}
	}
	for k, v := range out {
		if v == 0 {
			delete(out, k)
		}
	}
	return out, nil
}
