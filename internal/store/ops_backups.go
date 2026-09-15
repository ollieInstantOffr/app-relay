package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// BackupRow is one row of the backups table (slice: ops).
type BackupRow struct {
	ID        string         `json:"id"`
	CreatedAt time.Time      `json:"createdAt"`
	Size      int64          `json:"size"`
	Contents  map[string]int `json:"contents"`
	Trigger   string         `json:"trigger"` // scheduled | manual | before-upgrade | before-restore
	File      string         `json:"file"`
	Status    string         `json:"status"` // ok | failed | running
	Error     string         `json:"error,omitempty"`

	// Off-site copy (S3).
	RemoteStatus string `json:"remoteStatus,omitempty"` // "" | uploading | uploaded | failed
	RemoteKey    string `json:"remoteKey,omitempty"`
	RemoteError  string `json:"remoteError,omitempty"`
}

func (s *Store) InsertBackup(ctx context.Context, b BackupRow) error {
	contents, _ := json.Marshal(nonNilCounts(b.Contents))
	_, err := s.DB.ExecContext(ctx, `INSERT INTO backups (id, created_at, size, contents, trigger, file, status, error, remote_status, remote_key, remote_error) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, FormatTime(b.CreatedAt), b.Size, string(contents), b.Trigger, b.File, b.Status, b.Error, b.RemoteStatus, b.RemoteKey, b.RemoteError)
	return err
}

func (s *Store) UpdateBackup(ctx context.Context, b BackupRow) error {
	contents, _ := json.Marshal(nonNilCounts(b.Contents))
	res, err := s.DB.ExecContext(ctx, `UPDATE backups SET size = ?, contents = ?, file = ?, status = ?, error = ?, remote_status = ?, remote_key = ?, remote_error = ? WHERE id = ?`,
		b.Size, string(contents), b.File, b.Status, b.Error, b.RemoteStatus, b.RemoteKey, b.RemoteError, b.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nonNilCounts(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

const backupCols = `id, created_at, size, contents, trigger, file, status, error, remote_status, remote_key, remote_error`

func scanBackup(sc interface{ Scan(...any) error }) (BackupRow, error) {
	var b BackupRow
	var at, contents string
	if err := sc.Scan(&b.ID, &at, &b.Size, &contents, &b.Trigger, &b.File, &b.Status, &b.Error, &b.RemoteStatus, &b.RemoteKey, &b.RemoteError); err != nil {
		return b, err
	}
	b.CreatedAt = ParseTime(at)
	b.Contents = map[string]int{}
	_ = json.Unmarshal([]byte(contents), &b.Contents)
	return b, nil
}

// ListBackups returns backups newest first.
func (s *Store) ListBackups(ctx context.Context) ([]BackupRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+backupCols+` FROM backups ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupRow{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBackup(ctx context.Context, id string) (*BackupRow, error) {
	b, err := scanBackup(s.DB.QueryRowContext(ctx, `SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- notification log

type NotificationLogRow struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Event     string    `json:"event"`
	ChannelID string    `json:"channelId"`
	Status    string    `json:"status"` // sent | failed | queued
	Title     string    `json:"title"`
	Error     string    `json:"error,omitempty"`
}

func (s *Store) InsertNotificationLog(ctx context.Context, r NotificationLogRow) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO notification_log (at, event, channel_id, status, title, error) VALUES (?, ?, ?, ?, ?, ?)`,
		FormatTime(r.At), r.Event, r.ChannelID, r.Status, r.Title, r.Error)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListNotificationLog(ctx context.Context, limit int) ([]NotificationLogRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, at, event, channel_id, status, title, error FROM notification_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationLogRow{}
	for rows.Next() {
		var r NotificationLogRow
		var at string
		if err := rows.Scan(&r.ID, &at, &r.Event, &r.ChannelID, &r.Status, &r.Title, &r.Error); err != nil {
			return nil, err
		}
		r.At = ParseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneNotificationLog keeps the newest keep rows.
func (s *Store) PruneNotificationLog(ctx context.Context, keep int) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM notification_log WHERE id <= (SELECT id FROM notification_log ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep)
	return err
}
