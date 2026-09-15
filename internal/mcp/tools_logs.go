package mcp

// Read tools for the error log, the audit log and the activity feed.

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

type errorLogArgs struct {
	Source string `json:"source,omitempty" jsonschema:"Only this source, e.g. nginx, edge (Relay Edge) or haproxy"`
	Level  string `json:"level,omitempty" jsonschema:"Minimum level with a trailing + (e.g. warn+, error+) or a comma list (error,crit)"`
	Search string `json:"search,omitempty" jsonschema:"Only messages containing this text, e.g. a domain or upstream address"`
	Since  string `json:"since,omitempty" jsonschema:"How far back: a duration like 15m, 6h, 7d or an RFC 3339 time (default: everything kept)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Entries to return, newest first (default 50, max 500)"`
}

type auditLogArgs struct {
	Search    string `json:"search,omitempty" jsonschema:"Only entries whose action, target or detail contains this text"`
	ActorType string `json:"actorType,omitempty" jsonschema:"Only this kind of actor: user, token, mcp or system"`
	Actor     string `json:"actor,omitempty" jsonschema:"Only this actor (username or token name)"`
	Since     string `json:"since,omitempty" jsonschema:"How far back: a duration like 1h or 7d, or an RFC 3339 time"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Entries to return, newest first (default 50, max 500)"`
}

type activityArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"Events to return, newest first (default 20, max 200)"`
}

func (s *Service) registerLogTools() {
	addRead(s, toolInfo{Name: "query_error_log", Title: "Search the error log",
		Description: "Search the engine and Relay error log (nginx / Relay Edge / HAProxy errors, upstream connection failures, certificate problems) by source, minimum level, text and time window. Use it to find out why a host returns 502/504 or why an apply failed."},
		nil, s.toolErrorLog)
	addRead(s, toolInfo{Name: "query_audit_log", Title: "Search the audit log",
		Description: "Search the audit log of who changed what: logins, config edits, applies, rollbacks, token and MCP actions, with actor, IP, target and result. Admin only."},
		map[string][]any{"actorType": {"user", "token", "mcp", "system"}}, s.toolAuditLog)
	addRead(s, toolInfo{Name: "get_activity", Title: "Get recent activity",
		Description: "The recent activity feed from the Overview page: applies, certificate issues and renewals, engine upgrades, health changes and other notable events."},
		nil, s.toolActivity)
}

func clampLimit(n, def, max int) string {
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return strconv.Itoa(n)
}

func setIf(q url.Values, key, val string) {
	if v := strings.TrimSpace(val); v != "" {
		q.Set(key, v)
	}
}

func (s *Service) toolErrorLog(ctx context.Context, c *call, in errorLogArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading the error log"); err != nil {
		return nil, err
	}
	q := url.Values{"limit": {clampLimit(in.Limit, 50, 500)}}
	setIf(q, "source", in.Source)
	setIf(q, "level", in.Level)
	setIf(q, "q", in.Search)
	setIf(q, "since", in.Since)
	return s.apiRead(ctx, c, "/logs/error", q, "error log")
}

func (s *Service) toolAuditLog(ctx context.Context, c *call, in auditLogArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading the audit log"); err != nil {
		return nil, err
	}
	q := url.Values{"limit": {clampLimit(in.Limit, 50, 500)}}
	setIf(q, "q", in.Search)
	setIf(q, "actorType", in.ActorType)
	setIf(q, "actor", in.Actor)
	setIf(q, "since", in.Since)
	return s.apiRead(ctx, c, "/audit", q, "audit log")
}

func (s *Service) toolActivity(ctx context.Context, c *call, in activityArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading activity"); err != nil {
		return nil, err
	}
	return s.apiRead(ctx, c, "/activity", url.Values{"limit": {clampLimit(in.Limit, 20, 200)}}, "activity")
}
