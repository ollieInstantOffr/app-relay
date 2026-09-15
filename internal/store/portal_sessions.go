package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PortalSession is a Relay login session for apps (not the admin UI). ID is
// the sha256 (hex) of the cookie token; the raw token is never stored.
type PortalSession struct {
	ID         string
	UserID     string
	Domain     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
	// Joined from users, read-only.
	Username string
	Email    string
	Role     string
	Disabled bool
}

const portalSessionCols = `p.id, p.user_id, p.domain, p.created_at, p.expires_at, p.last_seen_at, p.ip, p.user_agent, u.username, u.email, u.role, u.disabled`

func scanPortalSession(sc interface{ Scan(...any) error }) (*PortalSession, error) {
	var p PortalSession
	var created, expires, seen string
	if err := sc.Scan(&p.ID, &p.UserID, &p.Domain, &created, &expires, &seen, &p.IP, &p.UserAgent, &p.Username, &p.Email, &p.Role, &p.Disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.CreatedAt, p.ExpiresAt, p.LastSeenAt = ParseTime(created), ParseTime(expires), ParseTime(seen)
	return &p, nil
}

func (s *Store) CreatePortalSession(ctx context.Context, p *PortalSession) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO portal_sessions (id, user_id, domain, created_at, expires_at, last_seen_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UserID, p.Domain, AuthTime(p.CreatedAt), AuthTime(p.ExpiresAt), AuthTime(p.LastSeenAt), p.IP, p.UserAgent)
	return err
}

func (s *Store) GetPortalSession(ctx context.Context, id string) (*PortalSession, error) {
	return scanPortalSession(s.DB.QueryRowContext(ctx, `SELECT `+portalSessionCols+` FROM portal_sessions p JOIN users u ON u.id = p.user_id WHERE p.id = ?`, id))
}

func (s *Store) TouchPortalSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE portal_sessions SET last_seen_at = ? WHERE id = ?`, AuthTime(at), id)
	return err
}

func (s *Store) DeletePortalSession(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM portal_sessions WHERE id = ?`, id)
	return err
}

// DeleteUserPortalSessions signs a user out of every app; returns how many.
func (s *Store) DeleteUserPortalSessions(ctx context.Context, userID string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM portal_sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) DeleteExpiredPortalSessions(ctx context.Context, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM portal_sessions WHERE expires_at <= ?`, AuthTime(now))
	return err
}
