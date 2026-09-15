package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AuthTime formats a timestamp with fixed-width nanoseconds so that the auth
// tables' TEXT time columns compare correctly in SQL (RFC3339Nano trims
// trailing zeros, which breaks lexicographic ordering within a second).
func AuthTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// User is a row of the users table.
type User struct {
	ID                 string
	Username           string
	Email              string
	Role               string
	PasswordHash       string
	TOTPSecret         string
	TOTPEnabled        bool
	MustChangePassword bool
	Disabled           bool
	CreatedAt          time.Time
	LastActiveAt       *time.Time
}

const userCols = `id, username, email, role, password_hash, totp_secret, totp_enabled, must_change_password, disabled, created_at, last_active_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created string
	var last sql.NullString
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.Role, &u.PasswordHash, &u.TOTPSecret, &u.TOTPEnabled,
		&u.MustChangePassword, &u.Disabled, &created, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = ParseTime(created)
	u.LastActiveAt = NullTime(last)
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountActiveAdmins counts enabled admins.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// GetUserByUsername looks a user up case-insensitively.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ? COLLATE NOCASE`, username))
}

func insertUser(ctx context.Context, db execer, u *User, onlyIfEmpty bool) (sql.Result, error) {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	q := `INSERT INTO users (` + userCols + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if onlyIfEmpty {
		q = `INSERT INTO users (` + userCols + `) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`
	}
	res, err := db.ExecContext(ctx, q, u.ID, u.Username, u.Email, u.Role, u.PasswordHash, u.TOTPSecret, u.TOTPEnabled,
		u.MustChangePassword, u.Disabled, AuthTime(u.CreatedAt), TimeOrNull(u.LastActiveAt))
	if err != nil && isUniqueErr(err) {
		return nil, ErrConflict
	}
	return res, err
}

// CreateUser inserts a user (ErrConflict when the username exists).
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	_, err := insertUser(ctx, s.DB, u, false)
	return err
}

// CreateFirstUser inserts u only while the users table is empty. It reports
// false (without error) when a user already exists. The check and insert are
// a single statement, so concurrent setup requests cannot both succeed.
func (s *Store) CreateFirstUser(ctx context.Context, u *User) (bool, error) {
	res, err := insertUser(ctx, s.DB, u, true)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UpdateUser writes every mutable column of u.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE users SET email = ?, role = ?, password_hash = ?, totp_secret = ?, totp_enabled = ?,
		must_change_password = ?, disabled = ? WHERE id = ?`,
		u.Email, u.Role, u.PasswordHash, u.TOTPSecret, u.TOTPEnabled, u.MustChangePassword, u.Disabled, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user; sessions and passkeys cascade.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchUserActive(ctx context.Context, id string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET last_active_at = ? WHERE id = ?`, AuthTime(at), id)
	return err
}

// FirstUserCreatedAt returns when the first account was created (≈ install time).
func (s *Store) FirstUserCreatedAt(ctx context.Context) (*time.Time, error) {
	var v sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT MIN(created_at) FROM users`).Scan(&v); err != nil {
		return nil, err
	}
	return NullTime(v), nil
}

// DatabaseSize returns the SQLite database size in bytes (page_count × page_size).
func (s *Store) DatabaseSize(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := s.DB.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.DB.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}
