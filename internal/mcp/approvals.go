package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// approvalEvent is the payload of events.ApprovalChanged.
type approvalEvent struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Tool       string `json:"tool"`
	ClientName string `json:"clientName"`
	Done       bool   `json:"done"` // approved and executed
}

func (s *Service) approvalTTL(st model.MCPSettings) time.Duration {
	if s.ttlOverride > 0 {
		return s.ttlOverride
	}
	m := st.ApprovalTimeoutMinutes
	if m <= 0 {
		m = 10
	}
	return time.Duration(m) * time.Minute
}

func (s *Service) publishApproval(a *store.Approval, done bool) {
	s.app.Bus.Publish(events.ApprovalChanged, approvalEvent{ID: a.ID, Status: a.Status, Tool: a.Tool, ClientName: a.ClientName, Done: done})
}

// awaitApproval files (or re-attaches to) an approval and blocks until it is
// decided, expires, or ctx ends.
func (s *Service) awaitApproval(ctx context.Context, req *mcp.CallToolRequest, c *call, info toolInfo, raw json.RawMessage, pl *plan, st model.MCPSettings) *mcp.CallToolResult {
	a, err := s.fileApproval(ctx, c, info, raw, pl, st)
	if err != nil {
		return errorResult(err)
	}
	var progress func(elapsed time.Duration)
	if req != nil && req.Session != nil {
		if tok := req.Params.GetProgressToken(); tok != nil {
			sess := req.Session
			n := 0.0
			progress = func(elapsed time.Duration) {
				n++
				_ = sess.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: tok, Progress: n,
					Message: fmt.Sprintf("Waiting for approval in Relay (%s so far, expires %s)", elapsed.Round(time.Second), a.ExpiresAt.Local().Format("15:04")),
				})
			}
		}
	}
	final, err := s.waitApproval(ctx, a.ID, progress)
	if err != nil {
		return errorResult(fmt.Errorf("stopped waiting for approval (%v); the request stays in Relay's approvals inbox until it expires", err))
	}
	return approvalResult(final)
}

func (s *Service) fileApproval(ctx context.Context, c *call, info toolInfo, raw json.RawMessage, pl *plan, st model.MCPSettings) (*store.Approval, error) {
	now := s.now()
	if existing, err := s.app.Store.MCPFindPendingApproval(ctx, c.actor.TokenID, info.Name, string(raw), now); err == nil {
		return existing, nil
	}
	actorJSON, _ := json.Marshal(c.actor)
	var args struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &args)
	a := &store.Approval{
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.approvalTTL(st)),
		TokenID:    c.actor.TokenID,
		ClientName: c.actor.ClientName,
		Tool:       info.Name,
		Args:       raw,
		Summary:    pl.Summary,
		Target:     pl.Target,
		Reason:     sanitizeLabel(args.Reason, 500),
		Preview:    pl.Preview,
		Status:     store.ApprovalPending,
		Actor:      string(actorJSON),
	}
	if err := s.app.Store.MCPInsertApproval(ctx, a); err != nil {
		return nil, err
	}
	s.publishApproval(a, false)
	s.audit(ctx, c.actor, info.Name, pl.Target, joinDetail(pl.Detail, "awaiting approval"), "pending", nil)
	s.notify(ctx, core.Notification{
		Event:   model.EventMCPWriteExecuted,
		Level:   "warn",
		Title:   fmt.Sprintf("%s wants to run %s", a.ClientName, info.Name),
		Message: joinDetail(plain(pl.Summary), "approve or deny in Relay before "+a.ExpiresAt.Local().Format("15:04")),
		URL:     "/logs/approvals",
	})
	return a, nil
}

// waitApproval blocks until approval id is final: denied, expired, or
// approved with its outcome stored.
func (s *Service) waitApproval(ctx context.Context, id string, progress func(time.Duration)) (*store.Approval, error) {
	events, cancel := s.app.Bus.Subscribe(64)
	defer cancel()
	poll := time.NewTicker(s.pollEvery)
	defer poll.Stop()
	var progressC <-chan time.Time
	if progress != nil {
		t := time.NewTicker(s.progressEvery)
		defer t.Stop()
		progressC = t.C
	}
	started := s.now()
	for {
		a, err := s.app.Store.MCPGetApproval(context.WithoutCancel(ctx), id)
		if err != nil {
			return nil, err
		}
		switch {
		case a.Status == store.ApprovalDenied || a.Status == store.ApprovalExpired:
			return a, nil
		case a.Status == store.ApprovalApproved && a.Output != "":
			return a, nil
		case a.Status == store.ApprovalPending && !s.now().Before(a.ExpiresAt):
			s.expire(context.WithoutCancel(ctx), a)
			continue
		}
		wait := time.Until(a.ExpiresAt)
		if a.Status != store.ApprovalPending || wait < 0 {
			wait = s.pollEvery
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case _, ok := <-events:
			// Any bus event (usually approval.changed) triggers a re-read;
			// the ticker covers dropped events.
			if !ok {
				events = nil
			}
		case <-poll.C:
		case <-timer.C:
		case <-progressC:
			progress(s.now().Sub(started))
		}
		timer.Stop()
	}
}

func approvalResult(a *store.Approval) *mcp.CallToolResult {
	switch a.Status {
	case store.ApprovalDenied:
		by := a.DecidedBy
		if by == "" {
			by = "an administrator"
		}
		return errorResult(fmt.Errorf("denied by %s — nothing was changed", by))
	case store.ApprovalExpired:
		return errorResult(fmt.Errorf("the approval request expired at %s without a decision — nothing was changed", a.ExpiresAt.Local().Format("15:04")))
	}
	var out outcome
	if err := json.Unmarshal([]byte(a.Output), &out); err != nil {
		return textResult(a.Result, nil)
	}
	res := outcomeResult(&out)
	if !out.IsError && a.DecidedBy != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: "Approved by " + a.DecidedBy + "."})
	}
	return res
}

// expire marks a pending approval expired (once) and records it.
func (s *Service) expire(ctx context.Context, a *store.Approval) {
	ok, err := s.app.Store.MCPDecideApproval(ctx, a.ID, store.ApprovalExpired, "", s.now())
	if err != nil || !ok {
		return
	}
	a.Status = store.ApprovalExpired
	s.publishApproval(a, false)
	s.audit(ctx, approvalActor(a), a.Tool, a.Target, joinDetail(plain(a.Summary), "approval expired"), "failed", nil)
}

func approvalActor(a *store.Approval) core.Actor {
	var actor core.Actor
	_ = json.Unmarshal([]byte(a.Actor), &actor)
	actor.Type = core.ActorMCP
	if actor.TokenID == "" {
		actor.TokenID = a.TokenID
	}
	if actor.ClientName == "" {
		actor.ClientName = a.ClientName
	}
	return actor
}

func (s *Service) janitor(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.expireDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) expireDue(ctx context.Context) {
	due, err := s.app.Store.MCPDueApprovals(ctx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			s.app.Log.Warn("mcp approvals janitor", "err", err)
		}
		return
	}
	for i := range due {
		s.expire(ctx, &due[i])
	}
}

// ErrNotPending is returned when deciding an approval that is already final.
type notPendingError struct{ a *store.Approval }

func (e notPendingError) Error() string {
	switch e.a.Status {
	case store.ApprovalExpired:
		return "this approval request has expired"
	case store.ApprovalApproved:
		return "already approved by " + e.a.DecidedBy
	case store.ApprovalDenied:
		return "already denied by " + e.a.DecidedBy
	}
	return "this approval request is no longer pending"
}

// Decide approves or denies a pending approval as user. Approving executes
// the call immediately with the original MCP actor and stores the outcome.
func (s *Service) Decide(ctx context.Context, id string, approve bool, user core.Actor) (*store.Approval, *outcome, error) {
	ctx = context.WithoutCancel(ctx)
	a, err := s.app.Store.MCPGetApproval(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	if a.Status == store.ApprovalPending && !now.Before(a.ExpiresAt) {
		s.expire(ctx, a)
		a, _ = s.app.Store.MCPGetApproval(ctx, id)
	}
	if a.Status != store.ApprovalPending {
		return a, nil, notPendingError{a}
	}
	status := store.ApprovalDenied
	if approve {
		status = store.ApprovalApproved
	}
	ok, err := s.app.Store.MCPDecideApproval(ctx, id, status, user.Name, now)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		a, _ = s.app.Store.MCPGetApproval(ctx, id)
		return a, nil, notPendingError{a}
	}
	a.Status, a.DecidedBy, a.DecidedAt = status, user.Name, &now
	mcpActor := approvalActor(a)
	u := user
	verb := "approval.deny"
	if approve {
		verb = "approval.approve"
	}
	s.app.Audit(ctx, core.AuditEntry{Actor: &u, Action: verb, Target: joinDetail(a.Tool, a.Target), Detail: joinDetail(plain(a.Summary), a.ClientName), Result: "ok"})

	if !approve {
		s.publishApproval(a, false)
		s.audit(ctx, mcpActor, a.Tool, a.Target, joinDetail(plain(a.Summary), "denied by "+user.Name), "denied", nil)
		return a, nil, nil
	}
	s.publishApproval(a, false)

	out := s.executeApproved(ctx, a, mcpActor, user.Name)
	data, _ := json.Marshal(out)
	result := out.Text
	if out.IsError {
		result = "Failed: " + out.Text
	}
	if err := s.app.Store.MCPSetApprovalOutcome(ctx, a.ID, result, string(data)); err != nil {
		return nil, nil, err
	}
	a.Result, a.Output = result, string(data)
	s.publishApproval(a, true)
	return a, out, nil
}

// executeApproved re-plans the stored call against current state (the token
// must still be valid) and executes it.
func (s *Service) executeApproved(ctx context.Context, a *store.Approval, actor core.Actor, approver string) *outcome {
	wt, ok := s.writes[a.Tool]
	if !ok {
		return &outcome{Text: "unknown tool " + a.Tool, IsError: true}
	}
	tok, err := s.app.Store.MCPGetToken(ctx, actor.TokenID)
	if err != nil || !tok.Usable(s.now()) {
		s.audit(ctx, actor, a.Tool, a.Target, joinDetail(plain(a.Summary), "token revoked before execution"), "failed", nil)
		return &outcome{Text: "the MCP token was revoked or expired before the call could run", IsError: true}
	}
	if tok.Scope != core.ScopeWrite {
		return &outcome{Text: "tool requires a read+write token", IsError: true}
	}
	actor.Scope = tok.Scope
	ctx = core.WithActor(ctx, actor)
	c := &call{actor: actor, scope: newScope(tok.LimitTo)}
	pl, err := wt.plan(ctx, c, a.Args)
	if err != nil {
		s.audit(ctx, actor, a.Tool, a.Target, joinDetail(plain(a.Summary), approvedByLabel(approver), err.Error()), "failed", nil)
		return &outcome{Text: "no longer possible: " + err.Error(), IsError: true}
	}
	out, err := s.execute(ctx, actor, wt.info, pl, "confirmed", approver)
	if err != nil {
		return &outcome{Text: err.Error(), IsError: true}
	}
	return out
}

// decisionResponse is returned by POST /api/approvals/{id}/approve|deny.
type decisionResponse struct {
	store.Approval
	Failed bool `json:"failed"`
}

func decideError(err error) error {
	var np notPendingError
	switch {
	case errors.As(err, &np):
		return httpx.Errorf(http.StatusConflict, "not_pending", np.Error())
	case errors.Is(err, store.ErrNotFound):
		return httpx.Errorf(http.StatusNotFound, "not_found", "approval not found")
	}
	return err
}
