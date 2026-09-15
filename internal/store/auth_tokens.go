package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// APITokenRow is a row of api_tokens. Hash is the sha256 (hex) of the raw token.
type APITokenRow struct {
	ID         string
	Name       string
	Prefix     string
	Last4      string
	Hash       string
	Scope      string
	Surfaces   []string
	LimitTo    []string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedBy  string // user id
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

const tokenCols = `id, name, prefix, last4, hash, scope, surfaces, limit_to, expires_at, last_used_at, created_by, created_at, revoked_at`

func scanToken(sc interface{ Scan(...any) error }) (*APITokenRow, error) {
	var t APITokenRow
	var surfaces, limit, created string
	var expires, used, revoked sql.NullString
	if err := sc.Scan(&t.ID, &t.Name, &t.Prefix, &t.Last4, &t.Hash, &t.Scope, &surfaces, &limit, &expires, &used, &t.CreatedBy, &created, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(surfaces), &t.Surfaces); err != nil || t.Surfaces == nil {
		t.Surfaces = []string{}
	}
	if err := json.Unmarshal([]byte(limit), &t.LimitTo); err != nil || t.LimitTo == nil {
		t.LimitTo = []string{}
	}
	t.CreatedAt = ParseTime(created)
	t.ExpiresAt, t.LastUsedAt, t.RevokedAt = NullTime(expires), NullTime(used), NullTime(revoked)
	return &t, nil
}

func authTimeOrNull(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return AuthTime(*t)
}

func (s *Store) CreateAPIToken(ctx context.Context, t *APITokenRow) error {
	surfaces, _ := json.Marshal(nonNilStrings(t.Surfaces))
	limit, _ := json.Marshal(nonNilStrings(t.LimitTo))
	_, err := s.DB.ExecContext(ctx, `INSERT INTO api_tokens (`+tokenCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Prefix, t.Last4, t.Hash, t.Scope, string(surfaces), string(limit), authTimeOrNull(t.ExpiresAt),
		authTimeOrNull(t.LastUsedAt), t.CreatedBy, AuthTime(t.CreatedAt), authTimeOrNull(t.RevokedAt))
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ListAPITokens returns tokens newest first; createdBy "" lists all.
func (s *Store) ListAPITokens(ctx context.Context, createdBy string) ([]APITokenRow, error) {
	q := `SELECT ` + tokenCols + ` FROM api_tokens`
	args := []any{}
	if createdBy != "" {
		q += ` WHERE created_by = ?`
		args = append(args, createdBy)
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APITokenRow{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) GetAPIToken(ctx context.Context, id string) (*APITokenRow, error) {
	return scanToken(s.DB.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE id = ?`, id))
}

func (s *Store) GetAPITokenByHash(ctx context.Context, hash string) (*APITokenRow, error) {
	return scanToken(s.DB.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE hash = ?`, hash))
}

func (s *Store) RevokeAPIToken(ctx context.Context, id string, at time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, AuthTime(at), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeUserAPITokens revokes every active token created by userID.
func (s *Store) RevokeUserAPITokens(ctx context.Context, userID string, at time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE created_by = ? AND revoked_at IS NULL`, AuthTime(at), userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) DeleteAPIToken(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchAPIToken(ctx context.Context, id string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, AuthTime(at), id)
	return err
}
