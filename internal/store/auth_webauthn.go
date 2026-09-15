package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// WebAuthnCredentialRow is a stored passkey. ID is the base64url credential
// id; Data is the JSON-encoded go-webauthn Credential.
type WebAuthnCredentialRow struct {
	ID         string
	UserID     string
	Name       string
	Data       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

const credCols = `id, user_id, name, data, created_at, last_used_at`

func scanCred(sc interface{ Scan(...any) error }) (*WebAuthnCredentialRow, error) {
	var c WebAuthnCredentialRow
	var created string
	var used sql.NullString
	if err := sc.Scan(&c.ID, &c.UserID, &c.Name, &c.Data, &created, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt = ParseTime(created)
	c.LastUsedAt = NullTime(used)
	return &c, nil
}

func (s *Store) ListWebAuthnCredentials(ctx context.Context, userID string) ([]WebAuthnCredentialRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+credCols+` FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebAuthnCredentialRow{}
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) CountWebAuthnCredentials(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// CountWebAuthnCredentialsByUser returns passkey counts keyed by user id.
func (s *Store) CountWebAuthnCredentialsByUser(ctx context.Context) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id, COUNT(*) FROM webauthn_credentials GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Store) CreateWebAuthnCredential(ctx context.Context, c *WebAuthnCredentialRow) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO webauthn_credentials (`+credCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.UserID, c.Name, c.Data, AuthTime(c.CreatedAt), authTimeOrNull(c.LastUsedAt))
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

// UpdateWebAuthnCredentialUse stores the refreshed credential (sign count, flags) after a login.
func (s *Store) UpdateWebAuthnCredentialUse(ctx context.Context, id, data string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webauthn_credentials SET data = ?, last_used_at = ? WHERE id = ?`, data, AuthTime(at), id)
	return err
}

// DeleteWebAuthnCredential removes a passkey belonging to userID.
func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
