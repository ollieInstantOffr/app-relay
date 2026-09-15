package store

import (
	"context"
	"database/sql"
	"errors"
)

// LBMinute is one per-minute aggregate of HAProxy counters for a backend
// (Backend "" = all backends combined).
type LBMinute struct {
	At       int64  `json:"at"`
	Backend  string `json:"backend"`
	SessAvg  int64  `json:"sessAvg"`
	SessMax  int64  `json:"sessMax"`
	Econ     int64  `json:"econ"`
	Eresp    int64  `json:"eresp"`
	Ereq     int64  `json:"ereq"`
	Wretr    int64  `json:"wretr"`
	Wredis   int64  `json:"wredis"`
	QueueMax int64  `json:"queueMax"`
}

// InsertLBMinutes stores aggregates in lb_minute and the shared lb_samples
// table (sess_rate = average, errors = connection + response errors).
func (s *Store) InsertLBMinutes(ctx context.Context, rows []LBMinute) error {
	if len(rows) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO lb_minute (at, backend, sess_avg, sess_max, econ, eresp, ereq, wretr, wredis, queue_max)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(at, backend) DO UPDATE SET sess_avg = excluded.sess_avg, sess_max = excluded.sess_max, econ = excluded.econ,
				eresp = excluded.eresp, ereq = excluded.ereq, wretr = excluded.wretr, wredis = excluded.wredis, queue_max = excluded.queue_max`,
				r.At, r.Backend, r.SessAvg, r.SessMax, r.Econ, r.Eresp, r.Ereq, r.Wretr, r.Wredis, r.QueueMax); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO lb_samples (at, backend, sess_rate, errors, queue) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(at, backend) DO UPDATE SET sess_rate = excluded.sess_rate, errors = excluded.errors, queue = excluded.queue`,
				r.At, r.Backend, r.SessAvg, r.Econ+r.Eresp, r.QueueMax); err != nil {
				return err
			}
		}
		return nil
	})
}

// LBMinutes returns aggregates for backend in [since, until) ordered by time.
func (s *Store) LBMinutes(ctx context.Context, backend string, since, until int64) ([]LBMinute, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, backend, sess_avg, sess_max, econ, eresp, ereq, wretr, wredis, queue_max
		FROM lb_minute WHERE backend = ? AND at >= ? AND at < ? ORDER BY at`, backend, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LBMinute{}
	for rows.Next() {
		var r LBMinute
		if err := rows.Scan(&r.At, &r.Backend, &r.SessAvg, &r.SessMax, &r.Econ, &r.Eresp, &r.Ereq, &r.Wretr, &r.Wredis, &r.QueueMax); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneLBSamples deletes aggregates older than before (unix seconds).
func (s *Store) PruneLBSamples(ctx context.Context, before int64) error {
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM lb_minute WHERE at < ?`, before); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM lb_samples WHERE at < ?`, before)
	return err
}

// LiveHAProxyConfig returns the haproxy.cfg of the live config version
// (written by the engine slice), or ErrNotFound when nothing was applied yet.
func (s *Store) LiveHAProxyConfig(ctx context.Context) (cfg string, version int64, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT id, haproxy_cfg FROM config_versions WHERE status = 'live' ORDER BY id DESC LIMIT 1`).Scan(&version, &cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	return cfg, version, err
}
