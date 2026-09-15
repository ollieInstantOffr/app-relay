package store

// Read-only queries on api_tokens and users used by the MCP server (slice: mcp).
// Tokens and users are owned by the auth slice; these never write.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// MCPToken is the subset of an API token the MCP server needs.
type MCPToken struct {
	ID        string
	Name      string
	Scope     string
	Surfaces  []string
	LimitTo   []string
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// Usable reports whether the token is neither revoked nor expired.
func (t *MCPToken) Usable(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	return t.ExpiresAt == nil || now.Before(*t.ExpiresAt)
}

func (s *Store) MCPGetToken(ctx context.Context, id string) (*MCPToken, error) {
	var t MCPToken
	var surfaces, limit string
	var expires, revoked sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, scope, surfaces, limit_to, expires_at, revoked_at FROM api_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Scope, &surfaces, &limit, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(surfaces), &t.Surfaces)
	_ = json.Unmarshal([]byte(limit), &t.LimitTo)
	t.ExpiresAt = NullTime(expires)
	t.RevokedAt = NullTime(revoked)
	return &t, nil
}

// MCPUser is a user row without secrets.
type MCPUser struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Email        string     `json:"email"`
	Role         string     `json:"role"`
	TOTPEnabled  bool       `json:"totpEnabled"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastActiveAt *time.Time `json:"lastActiveAt,omitempty"`
}

func (s *Store) MCPListUsers(ctx context.Context) ([]MCPUser, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, username, email, role, totp_enabled, disabled, created_at, last_active_at FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPUser{}
	for rows.Next() {
		var u MCPUser
		var created string
		var last sql.NullString
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.Role, &u.TOTPEnabled, &u.Disabled, &created, &last); err != nil {
			return nil, err
		}
		u.CreatedAt = ParseTime(created)
		u.LastActiveAt = NullTime(last)
		out = append(out, u)
	}
	return out, rows.Err()
}
