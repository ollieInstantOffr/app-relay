package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AuthSession is a row of the sessions table. ID is the sha256 (hex) of the
// cookie token; the raw token is never stored.
type AuthSession struct {
	ID         string
	UserID     string
	Username   string // joined, read-only
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
	MFAPending bool
	Remember   bool
}

const sessionCols = `s.id, s.user_id, u.username, s.created_at, s.expires_at, s.last_seen_at, s.ip, s.user_agent, s.mfa_pending, s.remember`

func scanSession(sc interface{ Scan(...any) error }) (*AuthSession, error) {
	var a AuthSession
	var created, expires, seen string
	if err := sc.Scan(&a.ID, &a.UserID, &a.Username, &created, &expires, &seen, &a.IP, &a.UserAgent, &a.MFAPending, &a.Remember); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt, a.ExpiresAt, a.LastSeenAt = ParseTime(created), ParseTime(expires), ParseTime(seen)
	return &a, nil
}

func (s *Store) CreateSession(ctx context.Context, a *AuthSession) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at, ip, user_agent, mfa_pending, remember)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.UserID, AuthTime(a.CreatedAt), AuthTime(a.ExpiresAt), AuthTime(a.LastSeenAt), a.IP, a.UserAgent, a.MFAPending, a.Remember)
	return err
}

func (s *Store) GetSession(ctx context.Context, id string) (*AuthSession, error) {
	return scanSession(s.DB.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, id))
}

// ListSessions returns unexpired sessions, newest activity first. userID ""
// lists every user's sessions.
func (s *Store) ListSessions(ctx context.Context, userID string, now time.Time) ([]AuthSession, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.expires_at > ?`
	args := []any{AuthTime(now)}
	if userID != "" {
		q += ` AND s.user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY s.last_seen_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuthSession{}
	for rows.Next() {
		a, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// TouchSession slides a session: last_seen_at = at, expires_at = expires.
func (s *Store) TouchSession(ctx context.Context, id string, at, expires time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`, AuthTime(at), AuthTime(expires), id)
	return err
}

func (s *Store) SetSessionMFAPending(ctx context.Context, userID string, pending bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET mfa_pending = ? WHERE user_id = ?`, pending, userID)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserSessions removes a user's sessions except keepID; returns how many.
func (s *Store) DeleteUserSessions(ctx context.Context, userID, keepID string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id != ?`, userID, keepID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, AuthTime(now))
	return err
}

// ---------------------------------------------------------------- login attempts

// RecordLoginAttempt stores one sign-in attempt. username should be lowercased.
func (s *Store) RecordLoginAttempt(ctx context.Context, at time.Time, ip, username string, success bool) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO login_attempts (at, ip, username, success) VALUES (?, ?, ?, ?)`, AuthTime(at), ip, username, success)
	return err
}

// LoginFailures counts failed attempts since `since`: for the (ip, username)
// pair (ignoring failures before the pair's last success) and for the ip
// across all usernames.
func (s *Store) LoginFailures(ctx context.Context, ip, username string, since time.Time) (pair, byIP int, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_attempts
		WHERE ip = ? AND username = ? AND success = 0 AND at >= ?
		AND at > COALESCE((SELECT MAX(at) FROM login_attempts WHERE ip = ? AND username = ? AND success = 1), '')`,
		ip, username, AuthTime(since), ip, username).Scan(&pair)
	if err != nil {
		return 0, 0, err
	}
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_attempts WHERE ip = ? AND success = 0 AND at >= ?`, ip, AuthTime(since)).Scan(&byIP)
	return pair, byIP, err
}

// LoginHistory reports whether username ever signed in successfully, and
// whether it did so from ip.
func (s *Store) LoginHistory(ctx context.Context, username, ip string) (hasAny, fromIP bool, err error) {
	var n, m int
	if err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(ip = ?), 0) FROM login_attempts WHERE username = ? AND success = 1`, ip, username).Scan(&n, &m); err != nil {
		return false, false, err
	}
	return n > 0, m > 0, nil
}

// ClearLoginFailures forgets failed attempts for username (password reset from the CLI).
func (s *Store) ClearLoginFailures(ctx context.Context, username string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM login_attempts WHERE username = ? AND success = 0`, username)
	return err
}

// PruneLoginAttempts removes failures before failedBefore and successes before okBefore.
func (s *Store) PruneLoginAttempts(ctx context.Context, failedBefore, okBefore time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM login_attempts WHERE (success = 0 AND at < ?) OR (success = 1 AND at < ?)`,
		AuthTime(failedBefore), AuthTime(okBefore))
	return err
}
