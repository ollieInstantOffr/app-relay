package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/instantoffr/relay/internal/model"
)

// Repo is a typed document repository for one configuration entity kind.
type Repo[T any, PT interface {
	*T
	model.Entity
}] struct {
	s    *Store
	kind string
}

func NewRepo[T any, PT interface {
	*T
	model.Entity
}](s *Store, kind string) *Repo[T, PT] {
	return &Repo[T, PT]{s: s, kind: kind}
}

func (s *Store) Hosts() *Repo[model.ProxyHost, *model.ProxyHost] {
	return NewRepo[model.ProxyHost](s, model.KindHost)
}
func (s *Store) Redirects() *Repo[model.Redirect, *model.Redirect] {
	return NewRepo[model.Redirect](s, model.KindRedirect)
}
func (s *Store) Streams() *Repo[model.Stream, *model.Stream] {
	return NewRepo[model.Stream](s, model.KindStream)
}
func (s *Store) AccessLists() *Repo[model.AccessList, *model.AccessList] {
	return NewRepo[model.AccessList](s, model.KindAccessList)
}
func (s *Store) Certificates() *Repo[model.Certificate, *model.Certificate] {
	return NewRepo[model.Certificate](s, model.KindCertificate)
}
func (s *Store) DNSProviders() *Repo[model.DNSProvider, *model.DNSProvider] {
	return NewRepo[model.DNSProvider](s, model.KindDNSProvider)
}
func (s *Store) Backends() *Repo[model.Backend, *model.Backend] {
	return NewRepo[model.Backend](s, model.KindBackend)
}
func (s *Store) Frontends() *Repo[model.Frontend, *model.Frontend] {
	return NewRepo[model.Frontend](s, model.KindFrontend)
}
func (s *Store) Gateways() *Repo[model.Gateway, *model.Gateway] {
	return NewRepo[model.Gateway](s, model.KindGateway)
}

func (r *Repo[T, PT]) Kind() string { return r.kind }

// List returns all entities ordered by creation time.
func (r *Repo[T, PT]) List(ctx context.Context) ([]T, error) {
	return listDocs[T](ctx, r.s.DB, r.kind)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listDocs[T any](ctx context.Context, db querier, kind string) ([]T, error) {
	rows, err := db.QueryContext(ctx, `SELECT data FROM `+kind+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repo[T, PT]) Get(ctx context.Context, id string) (*T, error) {
	var data string
	err := r.s.DB.QueryRowContext(ctx, `SELECT data FROM `+r.kind+` WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := new(T)
	if err := json.Unmarshal([]byte(data), v); err != nil {
		return nil, err
	}
	return v, nil
}

// Create assigns an id (when empty) and timestamps, then inserts.
func (r *Repo[T, PT]) Create(ctx context.Context, v *T) error {
	m := PT(v).GetMeta()
	if m.ID == "" {
		m.ID = NewID()
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = r.s.DB.ExecContext(ctx, `INSERT INTO `+r.kind+` (id, data, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		m.ID, string(data), FormatTime(m.CreatedAt), FormatTime(m.UpdatedAt))
	if err != nil && isUniqueErr(err) {
		return ErrConflict
	}
	return err
}

// Update replaces the stored document, preserving CreatedAt.
func (r *Repo[T, PT]) Update(ctx context.Context, v *T) error {
	m := PT(v).GetMeta()
	prev, err := r.Get(ctx, m.ID)
	if err != nil {
		return err
	}
	m.CreatedAt = PT(prev).GetMeta().CreatedAt
	m.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = r.s.DB.ExecContext(ctx, `UPDATE `+r.kind+` SET data = ?, updated_at = ? WHERE id = ?`,
		string(data), FormatTime(m.UpdatedAt), m.ID)
	return err
}

func (r *Repo[T, PT]) Delete(ctx context.Context, id string) error {
	res, err := r.s.DB.ExecContext(ctx, `DELETE FROM `+r.kind+` WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func replaceAll[T any, PT interface {
	*T
	model.Entity
}](ctx context.Context, tx *sql.Tx, kind string, items []T) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+kind); err != nil {
		return err
	}
	for i := range items {
		m := PT(&items[i]).GetMeta()
		data, err := json.Marshal(&items[i])
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+kind+` (id, data, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			m.ID, string(data), FormatTime(m.CreatedAt), FormatTime(m.UpdatedAt)); err != nil {
			return err
		}
	}
	return nil
}

func isUniqueErr(err error) bool {
	return err != nil && (contains(err.Error(), "UNIQUE constraint") || contains(err.Error(), "constraint failed"))
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
