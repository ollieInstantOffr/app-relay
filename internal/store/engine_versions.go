package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// VersionRow is one config_versions row (engine slice).
type VersionRow struct {
	ID        int64
	CreatedAt time.Time
	Actor     string
	Summary   string
	Status    string // live | superseded | rolled_back | failed | draft
	Snapshot  string // JSON model.Snapshot (only when loaded with blobs)
	// ProxyEngine is the proxy engine the version was rendered for: nginx | edge.
	ProxyEngine string
	// NginxFiles / NginxHash hold the proxy engine's files (columns nginx_files
	// and nginx_hash, whichever engine it is).
	NginxFiles string // JSON map path → content (only with blobs)
	// LBEngine is the load balancer engine the version was rendered for:
	// haproxy | balancer.
	LBEngine string
	// HAProxyCfg / HAProxyHash / HAProxyRunning describe the active load
	// balancer engine's release (columns haproxy_cfg, haproxy_hash and
	// haproxy_running, whichever engine it is): its main file content
	// (haproxy.cfg or balancer.json), the release hash and whether it runs.
	HAProxyCfg     string // (only with blobs)
	Changes        string // JSON []core.PendingItem
	Error          string
	ValidateMs     int64
	ReloadMs       int64
	RolledBackTo   *int64
	NginxHash      string
	HAProxyHash    string
	HAProxyRunning bool
	FailedEngine   string
	FailedStage    string
	Output         string
	// TunnelFiles / TunnelHash / TunnelRunning describe the tunnel engine's
	// release: its files (JSON map, only with blobs), hash and whether it runs.
	TunnelFiles   string
	TunnelHash    string
	TunnelRunning bool
}

const versionCols = `id, created_at, actor, summary, status, changes, error, validate_ms, reload_ms, rolled_back_to,
	nginx_hash, haproxy_hash, haproxy_running, failed_engine, failed_stage, output, proxy_engine, lb_engine, tunnel_hash, tunnel_running`

func scanVersion(sc interface{ Scan(...any) error }, blobs bool) (*VersionRow, error) {
	var v VersionRow
	var at string
	var rb sql.NullInt64
	var running, tunnelRunning int
	dest := []any{&v.ID, &at, &v.Actor, &v.Summary, &v.Status, &v.Changes, &v.Error, &v.ValidateMs, &v.ReloadMs, &rb,
		&v.NginxHash, &v.HAProxyHash, &running, &v.FailedEngine, &v.FailedStage, &v.Output, &v.ProxyEngine, &v.LBEngine, &v.TunnelHash, &tunnelRunning}
	if blobs {
		dest = append(dest, &v.Snapshot, &v.NginxFiles, &v.HAProxyCfg, &v.TunnelFiles)
	}
	if err := sc.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	v.CreatedAt = ParseTime(at)
	v.HAProxyRunning = running != 0
	v.TunnelRunning = tunnelRunning != 0
	if rb.Valid {
		x := rb.Int64
		v.RolledBackTo = &x
	}
	return &v, nil
}

func versionSelect(blobs bool) string {
	if blobs {
		return `SELECT ` + versionCols + `, snapshot, nginx_files, haproxy_cfg, tunnel_files FROM config_versions`
	}
	return `SELECT ` + versionCols + ` FROM config_versions`
}

// NextVersionID returns max(id)+1.
func (s *Store) NextVersionID(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) + 1 FROM config_versions`).Scan(&n)
	return n, err
}

func (s *Store) InsertVersion(ctx context.Context, v *VersionRow) error {
	if v.Changes == "" {
		v.Changes = "[]"
	}
	if v.ProxyEngine == "" {
		v.ProxyEngine = "nginx"
	}
	if v.LBEngine == "" {
		v.LBEngine = "haproxy"
	}
	running, tunnelRunning := 0, 0
	if v.HAProxyRunning {
		running = 1
	}
	if v.TunnelRunning {
		tunnelRunning = 1
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO config_versions
		(id, created_at, actor, summary, status, snapshot, nginx_files, haproxy_cfg, changes, error, validate_ms, reload_ms, rolled_back_to,
		 nginx_hash, haproxy_hash, haproxy_running, failed_engine, failed_stage, output, proxy_engine, lb_engine, tunnel_files, tunnel_hash, tunnel_running)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, FormatTime(v.CreatedAt), v.Actor, v.Summary, v.Status, v.Snapshot, v.NginxFiles, v.HAProxyCfg, v.Changes, v.Error,
		v.ValidateMs, v.ReloadMs, v.RolledBackTo, v.NginxHash, v.HAProxyHash, running, v.FailedEngine, v.FailedStage, v.Output, v.ProxyEngine, v.LBEngine,
		v.TunnelFiles, v.TunnelHash, tunnelRunning)
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

// FinishVersion records the outcome of an apply.
func (s *Store) FinishVersion(ctx context.Context, v *VersionRow) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE config_versions SET status = ?, error = ?, validate_ms = ?, reload_ms = ?, rolled_back_to = ?,
		failed_engine = ?, failed_stage = ?, output = ? WHERE id = ?`,
		v.Status, v.Error, v.ValidateMs, v.ReloadMs, v.RolledBackTo, v.FailedEngine, v.FailedStage, v.Output, v.ID)
	return err
}

// PromoteVersion marks id live and every other live version superseded.
func (s *Store) PromoteVersion(ctx context.Context, id int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE config_versions SET status = 'superseded' WHERE status = 'live' AND id <> ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE config_versions SET status = 'live' WHERE id = ?`, id)
		return err
	})
}

func (s *Store) GetVersion(ctx context.Context, id int64, blobs bool) (*VersionRow, error) {
	return scanVersion(s.DB.QueryRowContext(ctx, versionSelect(blobs)+` WHERE id = ?`, id), blobs)
}

// LiveVersion returns the live version or ErrNotFound.
func (s *Store) LiveVersion(ctx context.Context, blobs bool) (*VersionRow, error) {
	return scanVersion(s.DB.QueryRowContext(ctx, versionSelect(blobs)+` WHERE status = 'live' ORDER BY id DESC LIMIT 1`), blobs)
}

// ListVersions returns versions newest first (without snapshots/files).
func (s *Store) ListVersions(ctx context.Context, limit int, beforeID int64) ([]VersionRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := versionSelect(false)
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionRow{}
	for rows.Next() {
		v, err := scanVersion(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// PreviousVersionID returns the newest version id below id (0 when none).
func (s *Store) PreviousVersionID(ctx context.Context, id int64) (int64, error) {
	var n sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT MAX(id) FROM config_versions WHERE id < ?`, id).Scan(&n)
	return n.Int64, err
}

func (s *Store) CountVersions(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM config_versions`).Scan(&n)
	return n, err
}
