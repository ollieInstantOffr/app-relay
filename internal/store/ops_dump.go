package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// TableDump is a portable copy of one SQLite table (backups, slice: ops).
// BLOB values are encoded as {"$b64": "…"}.
type TableDump struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// ListTables returns the names of all regular user tables (no sqlite_*
// internals, no virtual tables).
func (s *Store) ListTables(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, err
		}
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl)), "CREATE VIRTUAL TABLE") {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// DumpTable reads every row of table.
func (s *Store) DumpTable(ctx context.Context, table string) (*TableDump, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT * FROM `+quoteIdent(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	d := &TableDump{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = map[string]string{"$b64": base64.StdEncoding.EncodeToString(b)}
			}
		}
		d.Rows = append(d.Rows, vals)
	}
	return d, rows.Err()
}

// CountRows returns the number of rows in table (0 when it does not exist).
func (s *Store) CountRows(ctx context.Context, table string) int {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(table)).Scan(&n); err != nil {
		return 0
	}
	return n
}

// DecodeTableDump parses a dump written by DumpTable, preserving integer
// precision and BLOB values.
func DecodeTableDump(r io.Reader) (*TableDump, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var raw struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	for _, row := range raw.Rows {
		if len(row) != len(raw.Columns) {
			return nil, fmt.Errorf("row has %d values, want %d", len(row), len(raw.Columns))
		}
		for i, v := range row {
			switch x := v.(type) {
			case json.Number:
				if n, err := x.Int64(); err == nil {
					row[i] = n
				} else if f, err := x.Float64(); err == nil {
					row[i] = f
				} else {
					row[i] = x.String()
				}
			case map[string]any:
				s, _ := x["$b64"].(string)
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, fmt.Errorf("invalid blob value: %w", err)
				}
				row[i] = b
			}
		}
	}
	return &TableDump{Columns: raw.Columns, Rows: raw.Rows}, nil
}

func (s *Store) tableColumns(ctx context.Context, q querier, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		cols[n] = true
	}
	return cols, rows.Err()
}

// RestoreTables replaces the contents of every table in dumps that also
// exists in the current schema, in a single transaction. Columns unknown to
// the current schema are ignored. When keepSessionID is set, that sessions
// row survives the restore (if its user exists in the restored users table),
// so the admin performing the restore stays signed in. It reports whether the
// session was kept.
func (s *Store) RestoreTables(ctx context.Context, dumps map[string]*TableDump, keepSessionID string) (sessionKept bool, err error) {
	current, err := s.ListTables(ctx)
	if err != nil {
		return false, err
	}
	exists := map[string]bool{}
	for _, t := range current {
		exists[t] = true
	}
	names := make([]string, 0, len(dumps))
	for name := range dumps {
		if exists[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return err
		}
		var session []any
		var sessionCols []string
		if keepSessionID != "" && exists["sessions"] {
			rows, err := tx.QueryContext(ctx, `SELECT * FROM sessions WHERE id = ?`, keepSessionID)
			if err != nil {
				return err
			}
			sessionCols, _ = rows.Columns()
			if rows.Next() {
				session = make([]any, len(sessionCols))
				ptrs := make([]any, len(sessionCols))
				for i := range session {
					ptrs[i] = &session[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					return err
				}
			}
			rows.Close()
		}
		for _, name := range names {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+quoteIdent(name)); err != nil {
				return fmt.Errorf("clear %s: %w", name, err)
			}
		}
		for _, name := range names {
			d := dumps[name]
			cols, err := s.tableColumns(ctx, tx, name)
			if err != nil {
				return err
			}
			idx := []int{}
			quoted := []string{}
			for i, c := range d.Columns {
				if cols[c] {
					idx = append(idx, i)
					quoted = append(quoted, quoteIdent(c))
				}
			}
			if len(idx) == 0 || len(d.Rows) == 0 {
				continue
			}
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+quoteIdent(name)+` (`+strings.Join(quoted, ", ")+`) VALUES (`+strings.TrimSuffix(strings.Repeat("?, ", len(idx)), ", ")+`)`)
			if err != nil {
				return fmt.Errorf("restore %s: %w", name, err)
			}
			args := make([]any, len(idx))
			for _, row := range d.Rows {
				for j, i := range idx {
					args[j] = row[i]
				}
				if _, err := stmt.ExecContext(ctx, args...); err != nil {
					stmt.Close()
					return fmt.Errorf("restore %s: %w", name, err)
				}
			}
			stmt.Close()
		}
		if session != nil {
			userCol := -1
			for i, c := range sessionCols {
				if c == "user_id" {
					userCol = i
				}
			}
			var n int
			if userCol >= 0 {
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, session[userCol]).Scan(&n); err != nil {
					return err
				}
			}
			if n > 0 {
				quoted := make([]string, len(sessionCols))
				for i, c := range sessionCols {
					quoted[i] = quoteIdent(c)
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO sessions (`+strings.Join(quoted, ", ")+`) VALUES (`+strings.TrimSuffix(strings.Repeat("?, ", len(sessionCols)), ", ")+`)`, session...); err != nil {
					return err
				}
				sessionKept = true
			}
		}
		var violations int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if violations > 0 {
			return fmt.Errorf("archive is inconsistent: %d foreign key violations", violations)
		}
		return nil
	})
	return sessionKept, err
}
