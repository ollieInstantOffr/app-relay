package store

import (
	"context"
	"time"
)

// WeeklyStats feeds the weekly summary notification (slice: ops).
type WeeklyStats struct {
	Requests  int64 `json:"requests"`
	Errors5xx int64 `json:"errors5xx"`
	Applies   int   `json:"applies"`
	Rollbacks int   `json:"rollbacks"`
	Failed    int   `json:"failed"`
}

func (s *Store) WeeklyStats(ctx context.Context, since time.Time) (WeeklyStats, error) {
	var w WeeklyStats
	minute := since.Unix() / 60
	var totalRows int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(requests), 0), COALESCE(SUM(s5xx), 0) FROM metrics_minute WHERE host = '' AND minute >= ?`, minute).
		Scan(&totalRows, &w.Requests, &w.Errors5xx); err != nil {
		return w, err
	}
	if totalRows == 0 {
		if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(s5xx), 0) FROM metrics_minute WHERE host NOT LIKE 'stream:%' AND minute >= ?`, minute).
			Scan(&w.Requests, &w.Errors5xx); err != nil {
			return w, err
		}
	}
	err := s.DB.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN status != 'draft' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'rolled_back' OR rolled_back_to IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0)
		FROM config_versions WHERE created_at >= ?`, FormatTime(since)).Scan(&w.Applies, &w.Rollbacks, &w.Failed)
	return w, err
}
