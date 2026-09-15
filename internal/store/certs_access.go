package store

// Queries owned by the certs slice (access-list denials, live snapshot and
// ACME bookkeeping).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

// DenialRow is one 403 response from the access log.
type DenialRow struct {
	ID       int64     `json:"id"`
	TS       time.Time `json:"ts"`
	HostID   string    `json:"hostId"`
	Host     string    `json:"host"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	ClientIP string    `json:"clientIp"`
}

// CertsAccessDenials returns recent 403 responses for the given host ids or
// host names (newest first).
func (s *Store) CertsAccessDenials(ctx context.Context, hostIDs, hostNames []string, since time.Time, limit int) ([]DenialRow, error) {
	out := []DenialRow{}
	if len(hostIDs) == 0 && len(hostNames) == 0 {
		return out, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var conds []string
	args := []any{FormatTime(since)}
	if len(hostIDs) > 0 {
		conds = append(conds, "host_id IN ("+placeholders(len(hostIDs))+")")
		for _, id := range hostIDs {
			args = append(args, id)
		}
	}
	if len(hostNames) > 0 {
		conds = append(conds, "host IN ("+placeholders(len(hostNames))+")")
		for _, h := range hostNames {
			args = append(args, h)
		}
	}
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, `SELECT id, ts, host_id, host, method, path, status, client_ip FROM access_log
		WHERE ts >= ? AND status = 403 AND kind = 'http' AND (`+strings.Join(conds, " OR ")+`)
		ORDER BY ts DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r DenialRow
		var ts string
		if err := rows.Scan(&r.ID, &ts, &r.HostID, &r.Host, &r.Method, &r.Path, &r.Status, &r.ClientIP); err != nil {
			return nil, err
		}
		r.TS = ParseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CertsLiveSnapshot returns the snapshot of the live config version, or
// ErrNotFound when nothing has been applied yet.
func (s *Store) CertsLiveSnapshot(ctx context.Context) (*model.Snapshot, error) {
	var data string
	err := s.DB.QueryRowContext(ctx, `SELECT snapshot FROM config_versions WHERE status = 'live' ORDER BY id DESC LIMIT 1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
