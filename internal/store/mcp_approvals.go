package store

// MCP approvals queries (slice: mcp).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// Approval is one confirm-gated MCP tool call.
type Approval struct {
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"createdAt"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	TokenID    string          `json:"tokenId"`
	ClientName string          `json:"clientName"`
	Tool       string          `json:"tool"`
	Args       json.RawMessage `json:"args"`
	Summary    string          `json:"summary"`
	Target     string          `json:"target"`
	Reason     string          `json:"reason"`
	Preview    string          `json:"preview"`
	Status     string          `json:"status"`
	DecidedBy  string          `json:"decidedBy"`
	DecidedAt  *time.Time      `json:"decidedAt,omitempty"`
	Result     string          `json:"result"`
	// Actor is the JSON-encoded core.Actor that requested the call (internal).
	Actor string `json:"-"`
	// Output is the JSON-encoded tool outcome once executed (internal).
	Output string `json:"-"`
}

const mcpApprovalCols = `id, created_at, expires_at, token_id, client_name, tool, args, summary, target, reason, preview, status, decided_by, decided_at, result, actor, output`

func scanMCPApproval(sc interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var created, expires, args string
	var decided sql.NullString
	if err := sc.Scan(&a.ID, &created, &expires, &a.TokenID, &a.ClientName, &a.Tool, &args, &a.Summary, &a.Target,
		&a.Reason, &a.Preview, &a.Status, &a.DecidedBy, &decided, &a.Result, &a.Actor, &a.Output); err != nil {
		return nil, err
	}
	a.CreatedAt = ParseTime(created)
	a.ExpiresAt = ParseTime(expires)
	a.DecidedAt = NullTime(decided)
	if json.Valid([]byte(args)) {
		a.Args = json.RawMessage(args)
	} else {
		a.Args = json.RawMessage("{}")
	}
	return &a, nil
}

func (s *Store) MCPInsertApproval(ctx context.Context, a *Approval) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	args := string(a.Args)
	if args == "" {
		args = "{}"
	}
	if a.Actor == "" {
		a.Actor = "{}"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO approvals (`+mcpApprovalCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, FormatTime(a.CreatedAt), FormatTime(a.ExpiresAt), a.TokenID, a.ClientName, a.Tool, args, a.Summary, a.Target,
		a.Reason, a.Preview, a.Status, a.DecidedBy, TimeOrNull(a.DecidedAt), a.Result, a.Actor, a.Output)
	return err
}

func (s *Store) MCPGetApproval(ctx context.Context, id string) (*Approval, error) {
	a, err := scanMCPApproval(s.DB.QueryRowContext(ctx, `SELECT `+mcpApprovalCols+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// MCPListApprovals returns approvals newest first. status "" or "all" returns every status.
func (s *Store) MCPListApprovals(ctx context.Context, status string, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + mcpApprovalCols + ` FROM approvals`
	args := []any{}
	if status != "" && status != "all" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanMCPApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// MCPFindPendingApproval finds a live pending approval for the same token, tool
// and arguments (a client retrying a call that is still waiting).
func (s *Store) MCPFindPendingApproval(ctx context.Context, tokenID, tool, args string, now time.Time) (*Approval, error) {
	a, err := scanMCPApproval(s.DB.QueryRowContext(ctx, `SELECT `+mcpApprovalCols+` FROM approvals
		WHERE status = 'pending' AND token_id = ? AND tool = ? AND args = ? AND expires_at > ?
		ORDER BY created_at DESC LIMIT 1`, tokenID, tool, args, FormatTime(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// MCPDecideApproval moves a pending, unexpired approval to status. It returns
// false when the approval was no longer pending (already decided or expired).
func (s *Store) MCPDecideApproval(ctx context.Context, id, status, decidedBy string, now time.Time) (bool, error) {
	q := `UPDATE approvals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ? AND status = 'pending'`
	args := []any{status, decidedBy, FormatTime(now), id}
	if status != ApprovalExpired {
		q += ` AND expires_at > ?`
		args = append(args, FormatTime(now))
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MCPSetApprovalOutcome stores the execution result of an approved call.
func (s *Store) MCPSetApprovalOutcome(ctx context.Context, id, result, output string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE approvals SET result = ?, output = ? WHERE id = ?`, result, output, id)
	return err
}

// MCPDueApprovals lists pending approvals whose expiry has passed.
func (s *Store) MCPDueApprovals(ctx context.Context, now time.Time) ([]Approval, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+mcpApprovalCols+` FROM approvals WHERE status = 'pending' AND expires_at <= ? ORDER BY created_at`, FormatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		a, err := scanMCPApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
