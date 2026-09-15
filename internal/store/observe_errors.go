package store

import (
	"context"
	"strings"
	"time"
)

// Observe slice: error log queries.

// ErrorRecord is one row of error_log.
type ErrorRecord struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	Source  string    `json:"source"` // nginx | edge (Relay Edge) | haproxy | relay | acme
	Level   string    `json:"level"`  // emerg | alert | crit | error | warn | notice | info | debug
	Message string    `json:"message"`
}

type ErrorFilter struct {
	Source   string
	Levels   []string
	Search   string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int
}

func (s *Store) QueryErrors(ctx context.Context, f ErrorFilter) ([]ErrorRecord, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	if len(f.Levels) > 0 {
		where = append(where, "level IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Levels)), ",")+")")
		for _, l := range f.Levels {
			args = append(args, l)
		}
	}
	if f.Search != "" {
		where = append(where, `message LIKE ? ESCAPE '\'`)
		args = append(args, "%"+LikeEscape(f.Search)+"%")
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
	if f.Limit <= 0 {
		f.Limit = 100
	}
	args = append(args, f.Limit)
	rows, err := s.DB.QueryContext(ctx, `SELECT id, ts, source, level, message FROM error_log WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ErrorRecord{}
	for rows.Next() {
		var r ErrorRecord
		var ts string
		if err := rows.Scan(&r.ID, &ts, &r.Source, &r.Level, &r.Message); err != nil {
			return nil, err
		}
		r.TS = ParseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}
