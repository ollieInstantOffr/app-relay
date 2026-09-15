// Package mcp is Relay's Model Context Protocol server: a streamable-HTTP
// endpoint at /mcp (token auth, access list, per-tool permissions with an
// approvals inbox), the admin API for approvals, and `relay mcp-stdio`.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	tokenPrefix = "rl_mcp_"
	// headerTransport marks requests from `relay mcp-stdio` on the same machine.
	headerTransport = "X-Relay-MCP-Transport"
	// headerClient carries the stdio client's name (trusted only from the local proxy).
	headerClient = "X-Relay-MCP-Client"
	// placeholderTool is added and removed to make the SDK announce
	// notifications/tools/list_changed after a settings change.
	placeholderTool = "relay_tools_changed"
	// stdioClientName is the upstream clientInfo name of `relay mcp-stdio`.
	stdioClientName = "relay-mcp-stdio"
)

const serverInstructions = `Relay manages a reverse proxy (nginx or Relay Edge) and a HAProxy load balancer.
Configuration writes (create_host, update_host, delete_host) are saved as pending changes and are not live until apply_changes runs.
Some write tools wait for a human to approve the call in Relay's approvals inbox; the call returns once it was approved, denied or expired.
Tokens can be limited to certain domains or backends; objects outside that scope are invisible.`

type toolInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

// call is the per-invocation context handed to tool implementations.
type call struct {
	actor core.Actor
	scope scope
}

// readResult is what a read tool returns.
type readResult struct {
	Text       string
	Structured any
	Target     string
	Detail     string // compact audit detail
}

// plan is a validated write, ready to execute (directly or after approval).
type plan struct {
	Summary string // human summary with **bold** targets
	Target  string // short label ("minio / 10.0.0.92")
	Preview string // what will change (diff lines or text)
	Detail  string // audit detail
	Exec    func(ctx context.Context) (*outcome, error)
}

// outcome is the stored/returned result of an executed write.
type outcome struct {
	Text       string `json:"text"`
	Structured any    `json:"structured,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	Version    *int64 `json:"version,omitempty"`
}

type writeTool struct {
	info toolInfo
	plan func(ctx context.Context, c *call, raw json.RawMessage) (*plan, error)
}

type Service struct {
	app     *core.App
	server  *mcp.Server
	sdk     http.Handler
	catalog []toolInfo
	byName  map[string]toolInfo
	writes  map[string]writeTool

	now           func() time.Time
	ttlOverride   time.Duration // tests
	pollEvery     time.Duration
	progressEvery time.Duration
}

func New(app *core.App) *Service {
	s := &Service{
		app:           app,
		byName:        map[string]toolInfo{},
		writes:        map[string]writeTool{},
		now:           time.Now,
		pollEvery:     2 * time.Second,
		progressEvery: 15 * time.Second,
	}
	version := app.Config.Version
	if version == "" {
		version = "dev"
	}
	s.server = mcp.NewServer(&mcp.Implementation{Name: "relay", Title: "Relay", Version: version}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	s.registerReadTools()
	s.registerWriteTools()
	s.server.AddReceivingMiddleware(s.filterToolList)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.server }, &mcp.StreamableHTTPOptions{
		// Every request carries a bearer token, so DNS-rebinding protection is
		// unnecessary — and it would reject requests proxied by our own nginx.
		DisableLocalhostProtection: true,
		SessionTimeout:             time.Hour,
	})
	s.sdk = auth.RequireBearerToken(s.verifyGated, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)
	return s
}

func (s *Service) Start(ctx context.Context) error {
	go s.janitor(ctx)
	return nil
}

func (s *Service) settings(ctx context.Context) (model.MCPSettings, error) {
	return store.LoadSettings[model.MCPSettings](ctx, s.app.Store, model.SettingsMCP)
}

// ---------------------------------------------------------------- endpoint gate

type gateKey struct{}

// Handler serves /mcp: enabled + transport check, access list, token auth,
// then the SDK's streamable HTTP handler.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveMCP) }

func (s *Service) serveMCP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.settings(ctx)
	if err != nil {
		s.app.Log.Error("mcp settings", "err", err)
		writeRPCError(w, http.StatusInternalServerError, -32603, "internal error")
		return
	}
	if !st.Enabled {
		writeRPCError(w, http.StatusNotFound, -32001, "Relay's MCP server is disabled (Settings → MCP server)")
		return
	}
	stdio := isLocalStdioProxy(r)
	if stdio {
		if !hasString(st.Transports, "stdio") {
			writeRPCError(w, http.StatusForbidden, -32001, "the stdio transport is disabled in Relay's MCP settings")
			return
		}
	} else {
		if !hasString(st.Transports, "http") {
			writeRPCError(w, http.StatusNotFound, -32001, "the HTTP transport is disabled in Relay's MCP settings")
			return
		}
		if st.AccessListID != "" && !s.accessAllowed(ctx, st.AccessListID, core.ClientIP(r)) {
			writeRPCError(w, http.StatusForbidden, -32001, "this client is not allowed to reach Relay's MCP endpoint")
			return
		}
	}

	raw, ok := bearerToken(r)
	if !ok || !strings.HasPrefix(raw, tokenPrefix) {
		unauthorized(w, "missing MCP token: send Authorization: Bearer rl_mcp_…")
		return
	}
	if s.app.Auth == nil {
		unauthorized(w, "invalid or expired MCP token")
		return
	}
	actor, err := s.app.Auth.AuthenticateToken(ctx, raw, "mcp")
	if err != nil || actor.IsZero() {
		unauthorized(w, "invalid or expired MCP token")
		return
	}
	actor.Type = core.ActorMCP
	if actor.TokenID == "" {
		actor.TokenID = actor.ID
	}
	actor.IP = core.ClientIP(r)
	actor.ClientName = ""
	if stdio {
		if name := r.Header.Get(headerClient); name != "" {
			actor.ClientName = friendlyClient(name)
		}
	}
	r = r.WithContext(context.WithValue(ctx, gateKey{}, actor))
	s.sdk.ServeHTTP(w, r)
}

// verifyGated hands the actor authenticated by serveMCP to the SDK, which
// binds sessions to the token and exposes it to tool handlers.
func (s *Service) verifyGated(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
	a, ok := r.Context().Value(gateKey{}).(core.Actor)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: "token:" + a.TokenID, Scopes: []string{a.Scope}, Extra: map[string]any{"actor": a}}, nil
}

func bearerToken(r *http.Request) (string, bool) {
	f := strings.Fields(r.Header.Get("Authorization"))
	if len(f) != 2 || !strings.EqualFold(f[0], "bearer") {
		return "", false
	}
	return f[1], true
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="relay", error="invalid_token"`)
	writeRPCError(w, http.StatusUnauthorized, -32001, msg)
}

func writeRPCError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": nil,
		"error": map[string]any{"code": code, "message": msg},
	})
}

// isLocalStdioProxy reports whether the request comes from `relay mcp-stdio`
// running on this machine: marked, from a loopback socket, and not proxied.
func isLocalStdioProxy(r *http.Request) bool {
	if r.Header.Get(headerTransport) != "stdio" {
		return false
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// accessAllowed evaluates the IP rules of an access list: first match wins,
// no match (or a missing list) denies.
func (s *Service) accessAllowed(ctx context.Context, listID, clientIP string) bool {
	al, err := s.app.Store.AccessLists().Get(ctx, listID)
	if err != nil {
		return false
	}
	return ipRulesAllow(al.Rules, clientIP)
}

func ipRulesAllow(rules []model.IPRule, clientIP string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(clientIP))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, rule := range rules {
		c := strings.TrimSpace(rule.CIDR)
		match := false
		if strings.EqualFold(c, "all") {
			match = true
		} else if p, err := netip.ParsePrefix(c); err == nil {
			match = p.Masked().Contains(addr)
		} else if a, err := netip.ParseAddr(c); err == nil {
			match = a.Unmap() == addr
		}
		if match {
			return strings.EqualFold(rule.Action, "allow")
		}
	}
	return false
}

// ConnectedSessions counts live MCP sessions.
func (s *Service) ConnectedSessions() int {
	n := 0
	for range s.server.Sessions() {
		n++
	}
	return n
}

// ---------------------------------------------------------------- tool list

// filterToolList hides disabled tools (and the placeholder) from tools/list.
func (s *Service) filterToolList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if method != "tools/list" || err != nil {
			return res, err
		}
		lr, ok := res.(*mcp.ListToolsResult)
		if !ok {
			return res, err
		}
		st, serr := s.settings(ctx)
		if serr != nil {
			return nil, serr
		}
		tools := make([]*mcp.Tool, 0, len(lr.Tools))
		for _, t := range lr.Tools {
			if _, known := s.byName[t.Name]; known && EffectivePermission(st, t.Name) != model.ToolDisabled {
				tools = append(tools, t)
			}
		}
		lr.Tools = tools
		return lr, nil
	}
}

// notifyToolListChanged makes connected clients refetch tools/list.
func (s *Service) notifyToolListChanged() {
	s.server.AddTool(&mcp.Tool{Name: placeholderTool, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return errorResult(errors.New("unknown tool")), nil
		})
	s.server.RemoveTools(placeholderTool)
}

// Catalog lists every tool in registration order.
func (s *Service) Catalog() []toolInfo { return append([]toolInfo(nil), s.catalog...) }

func (s *Service) addInfo(info toolInfo) {
	s.catalog = append(s.catalog, info)
	s.byName[info.Name] = info
}

// schemaFor infers an input schema and adds enums for some properties.
func schemaFor[In any](enums map[string][]any) *jsonschema.Schema {
	sch, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	for prop, values := range enums {
		if p, ok := sch.Properties[prop]; ok {
			p.Enum = values
		}
	}
	return sch
}

func annotations(info toolInfo, destructive bool) *mcp.ToolAnnotations {
	a := &mcp.ToolAnnotations{Title: info.Title, OpenWorldHint: boolPtr(false)}
	if info.Kind == KindRead {
		a.ReadOnlyHint = true
		a.IdempotentHint = true
	} else {
		a.DestructiveHint = boolPtr(destructive)
	}
	return a
}

// addRead registers a read tool.
func addRead[In any](s *Service, info toolInfo, enums map[string][]any, fn func(ctx context.Context, c *call, in In) (*readResult, error)) {
	info.Kind = KindRead
	s.addInfo(info)
	tool := &mcp.Tool{Name: info.Name, Title: info.Title, Description: info.Description, InputSchema: schemaFor[In](enums), Annotations: annotations(info, false)}
	mcp.AddTool(s.server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		ctx, c, denied := s.begin(ctx, req, info)
		if denied != nil {
			return denied, nil, nil
		}
		res, err := fn(ctx, c, in)
		if err != nil {
			s.audit(ctx, c.actor, info.Name, argTarget(req.Params.Arguments), err.Error(), "failed", nil)
			return errorResult(err), nil, nil
		}
		s.audit(ctx, c.actor, info.Name, res.Target, res.Detail, "ok", nil)
		return textResult(res.Text, res.Structured), nil, nil
	})
}

// addWrite registers a write tool.
func addWrite[In any](s *Service, info toolInfo, enums map[string][]any, destructive bool, fn func(ctx context.Context, c *call, in In) (*plan, error)) {
	info.Kind = KindWrite
	s.addInfo(info)
	s.writes[info.Name] = writeTool{info: info, plan: func(ctx context.Context, c *call, raw json.RawMessage) (*plan, error) {
		var in In
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
		}
		return fn(ctx, c, in)
	}}
	tool := &mcp.Tool{Name: info.Name, Title: info.Title, Description: info.Description, InputSchema: schemaFor[In](enums), Annotations: annotations(info, destructive)}
	mcp.AddTool(s.server, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		ctx, c, denied := s.begin(ctx, req, info)
		if denied != nil {
			return denied, nil, nil
		}
		raw, err := json.Marshal(in)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return s.runWrite(ctx, req, c, info, raw), nil, nil
	})
}

// begin resolves the caller and enforces tool permission and token scope.
func (s *Service) begin(ctx context.Context, req *mcp.CallToolRequest, info toolInfo) (context.Context, *call, *mcp.CallToolResult) {
	var actor core.Actor
	var ok bool
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
		actor, ok = req.Extra.TokenInfo.Extra["actor"].(core.Actor)
	}
	if !ok {
		return ctx, nil, errorResult(errors.New("unauthenticated"))
	}
	tok, err := s.app.Store.MCPGetToken(ctx, actor.TokenID)
	if err != nil || !tok.Usable(s.now()) {
		return ctx, nil, errorResult(errors.New("this MCP token was revoked or has expired"))
	}
	if actor.ClientName == "" {
		if ci := req.ClientInfo(); ci != nil && ci.Name != stdioClientName {
			name := ci.Title
			if name == "" {
				name = ci.Name
			}
			actor.ClientName = friendlyClient(name)
		}
	}
	if actor.ClientName == "" {
		actor.ClientName = tok.Name
	}
	if actor.Name == "" {
		actor.Name = tok.Name
	}
	ctx = core.WithActor(ctx, actor)
	c := &call{actor: actor, scope: newScope(tok.LimitTo)}

	st, err := s.settings(ctx)
	if err != nil {
		return ctx, nil, errorResult(err)
	}
	target := argTarget(req.Params.Arguments)
	switch {
	case EffectivePermission(st, info.Name) == model.ToolDisabled:
		s.audit(ctx, actor, info.Name, target, joinDetail(target, "tool disabled for token"), "denied", nil)
		return ctx, nil, errorResult(fmt.Errorf("tool %s is disabled in Relay's MCP settings", info.Name))
	case info.Kind == KindWrite && actor.Scope != core.ScopeWrite:
		s.audit(ctx, actor, info.Name, target, joinDetail(target, "read-only token"), "denied", nil)
		return ctx, nil, errorResult(errors.New("tool requires a read+write token"))
	}
	return ctx, c, nil
}

func joinDetail(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func (s *Service) audit(ctx context.Context, actor core.Actor, action, target, detail, result string, version *int64) {
	a := actor
	s.app.Audit(ctx, core.AuditEntry{Actor: &a, Action: action, Target: target, Detail: detail, Result: result, Version: version})
}

func (s *Service) notify(ctx context.Context, n core.Notification) {
	if s.app.Notify != nil {
		s.app.Notify.Notify(ctx, n)
	}
}

func textResult(text string, structured any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, StructuredContent: structured}
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}, IsError: true}
}

func outcomeResult(o *outcome) *mcp.CallToolResult {
	if o.IsError {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: o.Text}}, StructuredContent: o.Structured, IsError: true}
	}
	return textResult(o.Text, o.Structured)
}

// ---------------------------------------------------------------- writes

// runWrite plans a write and executes it directly (allow) or through the
// approvals inbox (confirm).
func (s *Service) runWrite(ctx context.Context, req *mcp.CallToolRequest, c *call, info toolInfo, raw json.RawMessage) *mcp.CallToolResult {
	wt := s.writes[info.Name]
	pl, err := wt.plan(ctx, c, raw)
	if err != nil {
		s.audit(ctx, c.actor, info.Name, argTarget(raw), err.Error(), "failed", nil)
		return errorResult(err)
	}
	st, err := s.settings(ctx)
	if err != nil {
		return errorResult(err)
	}
	if EffectivePermission(st, info.Name) == model.ToolConfirm {
		return s.awaitApproval(ctx, req, c, info, raw, pl, st)
	}
	out, err := s.execute(ctx, c.actor, info, pl, "ok", "")
	if err != nil {
		return errorResult(err)
	}
	return outcomeResult(out)
}

// execute runs a plan as actor and records audit, activity and notification.
func (s *Service) execute(ctx context.Context, actor core.Actor, info toolInfo, pl *plan, auditResult, approvedBy string) (*outcome, error) {
	ctx = core.WithActor(ctx, actor)
	out, err := pl.Exec(ctx)
	if err != nil {
		s.audit(ctx, actor, info.Name, pl.Target, joinDetail(pl.Detail, approvedByLabel(approvedBy), err.Error()), "failed", nil)
		return nil, err
	}
	if out.IsError {
		s.audit(ctx, actor, info.Name, pl.Target, joinDetail(pl.Detail, approvedByLabel(approvedBy), out.Text), "failed", out.Version)
		return out, nil
	}
	s.audit(ctx, actor, info.Name, pl.Target, joinDetail(pl.Detail, approvedByLabel(approvedBy)), auditResult, out.Version)
	client := actor.ClientName
	if client == "" {
		client = actor.Name
	}
	s.app.Activity(ctx, "mcp.write", "info", plain(pl.Summary), client, joinDetail("via MCP", approvedByLabel(approvedBy)))
	s.notify(ctx, core.Notification{
		Event:   model.EventMCPWriteExecuted,
		Level:   "info",
		Title:   fmt.Sprintf("%s ran %s", client, info.Name),
		Message: joinDetail(plain(pl.Summary), approvedByLabel(approvedBy)),
		URL:     "/logs/audit",
	})
	return out, nil
}

func approvedByLabel(user string) string {
	if user == "" {
		return ""
	}
	return "approved by " + user
}
