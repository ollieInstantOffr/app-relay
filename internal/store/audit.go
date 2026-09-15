package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type AuditRow struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	ActorType string    `json:"actorType"`
	ActorID   string    `json:"actorId"`
	ActorName string    `json:"actorName"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Detail    string    `json:"detail"`
	Version   *int64    `json:"version,omitempty"`
	Result    string    `json:"result"`
	IP        string    `json:"ip"`
}

func (s *Store) InsertAudit(ctx context.Context, r AuditRow) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log (at, actor_type, actor_id, actor_name, action, target, detail, version, result, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		FormatTime(r.At), r.ActorType, r.ActorID, r.ActorName, r.Action, r.Target, r.Detail, r.Version, r.Result, r.IP)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type AuditQuery struct {
	Search    string // matches actor, action, target, detail
	ActorType string // user | mcp | system | docker | token
	Actor     string // actor name
	Since     time.Time
	BeforeID  int64
	Limit     int
}

func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditRow, error) {
	where := []string{"1=1"}
	args := []any{}
	if q.Search != "" {
		like := "%" + LikeEscape(q.Search) + "%"
		where = append(where, `(actor_name LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR target LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	if q.ActorType != "" {
		where = append(where, "actor_type = ?")
		args = append(args, q.ActorType)
	}
	if q.Actor != "" {
		where = append(where, "actor_name = ?")
		args = append(args, q.Actor)
	}
	if !q.Since.IsZero() {
		where = append(where, "at >= ?")
		args = append(args, FormatTime(q.Since))
	}
	if q.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, q.BeforeID)
	}
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	args = append(args, q.Limit)
	rows, err := s.DB.QueryContext(ctx, `SELECT id, at, actor_type, actor_id, actor_name, action, target, detail, version, result, ip
		FROM audit_log WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		var at string
		var version sql.NullInt64
		if err := rows.Scan(&r.ID, &at, &r.ActorType, &r.ActorID, &r.ActorName, &r.Action, &r.Target, &r.Detail, &version, &r.Result, &r.IP); err != nil {
			return nil, err
		}
		r.At = ParseTime(at)
		if version.Valid {
			v := version.Int64
			r.Version = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type ActivityRow struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Level   string    `json:"level"`
	Title   string    `json:"title"`
	Subject string    `json:"subject"`
	Detail  string    `json:"detail"`
}

func (s *Store) InsertActivity(ctx context.Context, r ActivityRow) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO activity (at, kind, level, title, subject, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		FormatTime(r.At), r.Kind, r.Level, r.Title, r.Subject, r.Detail)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListActivity(ctx context.Context, limit int) ([]ActivityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, at, kind, level, title, subject, detail FROM activity ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActivityRow{}
	for rows.Next() {
		var r ActivityRow
		var at string
		if err := rows.Scan(&r.ID, &at, &r.Kind, &r.Level, &r.Title, &r.Subject, &r.Detail); err != nil {
			return nil, err
		}
		r.At = ParseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
