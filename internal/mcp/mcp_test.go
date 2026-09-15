package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// ---------------------------------------------------------------- stubs

const testToken = "rl_mcp_testtoken0000"

type stubAuth struct{}

func (stubAuth) Start(context.Context) error { return nil }
func (stubAuth) Authenticate(*http.Request) (core.Actor, error) {
	return core.Actor{}, core.ErrUnauthenticated
}
func (stubAuth) AuthenticateToken(_ context.Context, raw, surface string) (core.Actor, error) {
	if raw != testToken || surface != "mcp" {
		return core.Actor{}, core.ErrUnauthenticated
	}
	return core.Actor{Type: core.ActorMCP, ID: "tok1", TokenID: "tok1", Name: "desktop-token", Role: core.RoleEditor, Scope: core.ScopeWrite}, nil
}

type stateCall struct{ backend, server, state string }

type stubLB struct {
	mu    sync.Mutex
	calls []stateCall
}

func (*stubLB) Start(context.Context) error { return nil }
func (*stubLB) Stats(context.Context) (*core.LBStats, error) {
	return nil, core.ErrNotImplemented
}
func (l *stubLB) SetServerState(_ context.Context, backendID, serverID, state string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, stateCall{backendID, serverID, state})
	return nil
}
func (l *stubLB) Calls() []stateCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]stateCall(nil), l.calls...)
}

type fixture struct {
	app *core.App
	svc *Service
	lb  *stubLB
	st  *store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{Version: "test", RunDir: t.TempDir()}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	lb := &stubLB{}
	app.Auth = stubAuth{}
	app.LB = lb
	svc := New(app)
	svc.pollEvery = 20 * time.Millisecond
	app.MCP = svc
	f := &fixture{app: app, svc: svc, lb: lb, st: st}
	f.insertToken(t, "tok1", "desktop-token", core.ScopeWrite, nil)
	f.settings(t, func(m *model.MCPSettings) {})
	return f
}

func (f *fixture) insertToken(t *testing.T, id, name, scope string, limitTo []string) {
	t.Helper()
	if limitTo == nil {
		limitTo = []string{}
	}
	lt, _ := json.Marshal(limitTo)
	_, err := f.st.DB.Exec(`INSERT OR REPLACE INTO api_tokens (id, name, prefix, last4, hash, scope, surfaces, limit_to, created_at)
		VALUES (?, ?, 'rl_mcp_', '0000', ?, ?, '["mcp"]', ?, ?)`, id, name, "hash-"+id, scope, string(lt), store.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) settings(t *testing.T, mut func(*model.MCPSettings)) {
	t.Helper()
	s := store.DefaultMCP()
	s.Enabled = true
	s.Transports = []string{"http"}
	mut(&s)
	if err := f.st.PutSettings(context.Background(), model.SettingsMCP, s); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) mcpActor() core.Actor {
	return core.Actor{Type: core.ActorMCP, ID: "tok1", TokenID: "tok1", Name: "desktop-token", Scope: core.ScopeWrite, ClientName: "Claude Desktop"}
}

func (f *fixture) auditResults(t *testing.T, action string) []string {
	t.Helper()
	rows, err := f.st.ListAudit(context.Background(), store.AuditQuery{ActorType: core.ActorMCP, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Action == action {
			out = append(out, rows[i].Result)
		}
	}
	return out
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------- pure logic

func TestEffectivePermission(t *testing.T) {
	s := model.MCPSettings{Tools: map[string]string{
		"list_hosts":   model.ToolDisabled,
		"create_host":  model.ToolAllow,
		"update_host":  model.ToolRead,    // invalid for a write tool → confirm
		"query_logs":   model.ToolConfirm, // invalid for a read tool → read
		"drain_server": "bogus",           // unknown → default (confirm)
	}}
	cases := map[string]string{
		"list_hosts":          model.ToolDisabled,
		"create_host":         model.ToolAllow,
		"update_host":         model.ToolConfirm,
		"query_logs":          model.ToolRead,
		"drain_server":        model.ToolConfirm,
		"delete_host":         model.ToolDisabled, // default
		"get_backend_status":  model.ToolRead,     // default
		"request_certificate": model.ToolConfirm,  // default
		"no_such_tool":        model.ToolDisabled,
	}
	for name, want := range cases {
		if got := EffectivePermission(s, name); got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
	for name := range store.MCPTools {
		if EffectivePermission(model.MCPSettings{}, name) != store.MCPTools[name] {
			t.Errorf("%s: empty settings should use the default", name)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*.home.lan", "grafana.home.lan", true},
		{"*.home.lan", "a.b.home.lan", true},
		{"*.home.lan", "home.lan", false},
		{"*.home.lan", "grafana.home.lan.evil.com", false},
		{"*.HOME.lan", "Grafana.home.LAN", true},
		{"*.home.lan", "*.home.lan", true},
		{"web-app", "web-app", true},
		{"web-app", "web-app-2", false},
		{"web-*", "web-app-2", true},
		{"api-?", "api-1", true},
		{"api-?", "api-12", false},
		{"*", "anything", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
	sc := newScope([]string{"*.home.lan", " web-app "})
	if !sc.allowsAll([]string{"a.home.lan", "b.home.lan"}) || sc.allowsAll([]string{"a.home.lan", "example.com"}) {
		t.Error("allowsAll must require every domain in scope")
	}
	if !sc.allows("web-app") || sc.allows("minio") {
		t.Error("backend names")
	}
	if !newScope(nil).allowsAll(nil) {
		t.Error("empty scope is unrestricted")
	}
}

func TestIPRulesAndHelpers(t *testing.T) {
	rules := []model.IPRule{{Action: "deny", CIDR: "10.0.0.66"}, {Action: "allow", CIDR: "10.0.0.0/8"}, {Action: "deny", CIDR: "all"}}
	for ip, want := range map[string]bool{"10.1.2.3": true, "10.0.0.66": false, "192.168.1.5": false, "::ffff:10.0.0.1": true, "garbage": false} {
		if got := ipRulesAllow(rules, ip); got != want {
			t.Errorf("ipRulesAllow(%s) = %v", ip, got)
		}
	}
	if ipRulesAllow([]model.IPRule{{Action: "allow", CIDR: "10.0.0.0/8"}}, "192.168.1.1") {
		t.Error("no matching rule must deny")
	}
	u, err := parseUpstream("10.0.0.21:3000")
	if err != nil || u.Scheme != "http" || u.Host != "10.0.0.21" || u.Port != 3000 {
		t.Errorf("parseUpstream: %+v %v", u, err)
	}
	if u, _ := parseUpstream("https://nas.lan/app"); u.Port != 443 || u.Path != "/app" {
		t.Errorf("parseUpstream https default: %+v", u)
	}
	if _, err := parseUpstream("ftp://x:1"); err == nil {
		t.Error("ftp upstream must fail")
	}
	if !certCovers("*.home.lan", "grafana.home.lan") || certCovers("*.home.lan", "a.b.home.lan") || certCovers("*.home.lan", "home.lan") {
		t.Error("certCovers")
	}
	if d := diffLines([]string{"a", "b", "c"}, []string{"a", "x", "c"}); d != "  a\n- b\n+ x\n  c" {
		t.Errorf("diffLines = %q", d)
	}
}

// ---------------------------------------------------------------- approvals

func (f *fixture) addBackend(t *testing.T) (*model.Backend, *model.Server) {
	t.Helper()
	b := &model.Backend{Name: "minio", Mode: "http", Algorithm: "roundrobin", Servers: []model.Server{
		{ID: "s1", Name: "minio-1", Address: "10.0.0.91", Port: 9000, Weight: 100, Role: model.ServerActive, State: model.ServerStateReady},
		{ID: "s2", Name: "minio-2", Address: "10.0.0.92", Port: 9000, Weight: 100, Role: model.ServerActive, State: model.ServerStateReady},
	}}
	if err := f.st.Backends().Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return b, &b.Servers[1]
}

func (f *fixture) startDrain(t *testing.T, ctx context.Context) <-chan *mcp.CallToolResult {
	t.Helper()
	raw, _ := json.Marshal(drainServerArgs{Backend: "minio", Server: "10.0.0.92", Reason: "rebalancing disks"})
	c := &call{actor: f.mcpActor(), scope: scope{}}
	ch := make(chan *mcp.CallToolResult, 1)
	go func() { ch <- f.svc.runWrite(core.WithActor(ctx, c.actor), nil, c, f.svc.byName["drain_server"], raw) }()
	return ch
}

func (f *fixture) waitPending(t *testing.T) store.Approval {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		items, err := f.st.MCPListApprovals(context.Background(), store.ApprovalPending, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 1 {
			return items[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending approval appeared")
	return store.Approval{}
}

func awaitResult(t *testing.T, ch <-chan *mcp.CallToolResult) *mcp.CallToolResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("tool call did not return")
		return nil
	}
}

var jonas = core.Actor{Type: core.ActorUser, ID: "u1", Name: "jonas", Role: core.RoleAdmin}

func TestApprovalApprove(t *testing.T) {
	f := newFixture(t)
	b, sv := f.addBackend(t)
	ch := f.startDrain(t, context.Background())

	a := f.waitPending(t)
	if a.Summary != "Drain **10.0.0.92:9000** in backend **minio**" || a.Target != "minio / 10.0.0.92" || a.Reason != "rebalancing disks" || a.ClientName != "Claude Desktop" {
		t.Errorf("approval row: %+v", a)
	}
	if !a.ExpiresAt.After(time.Now().Add(9 * time.Minute)) {
		t.Errorf("expiry should follow approvalTimeoutMinutes (10): %v", a.ExpiresAt)
	}
	if len(f.lb.Calls()) != 0 {
		t.Fatal("must not execute before approval")
	}

	decided, out, err := f.svc.Decide(context.Background(), a.ID, true, jonas)
	if err != nil || out == nil || out.IsError {
		t.Fatalf("Decide: %v %+v", err, out)
	}
	if decided.Status != store.ApprovalApproved || decided.DecidedBy != "jonas" {
		t.Errorf("decided: %+v", decided)
	}
	res := awaitResult(t, ch)
	if res.IsError || !strings.Contains(resultText(res), "is draining") || !strings.Contains(resultText(res), "Approved by jonas") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	if calls := f.lb.Calls(); len(calls) != 1 || calls[0] != (stateCall{b.ID, sv.ID, model.ServerStateDrain}) {
		t.Errorf("SetServerState calls: %+v", calls)
	}
	if got := f.auditResults(t, "drain_server"); strings.Join(got, ",") != "pending,confirmed" {
		t.Errorf("audit results: %v", got)
	}
	if _, _, err := f.svc.Decide(context.Background(), a.ID, false, jonas); err == nil {
		t.Error("deciding twice must fail")
	}
}

func TestApprovalDeny(t *testing.T) {
	f := newFixture(t)
	f.addBackend(t)
	ch := f.startDrain(t, context.Background())
	a := f.waitPending(t)
	if _, _, err := f.svc.Decide(context.Background(), a.ID, false, jonas); err != nil {
		t.Fatal(err)
	}
	res := awaitResult(t, ch)
	if !res.IsError || !strings.Contains(resultText(res), "denied by jonas") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	if len(f.lb.Calls()) != 0 {
		t.Error("denied call must not execute")
	}
	if got := f.auditResults(t, "drain_server"); strings.Join(got, ",") != "pending,denied" {
		t.Errorf("audit results: %v", got)
	}
}

func TestApprovalExpire(t *testing.T) {
	f := newFixture(t)
	f.addBackend(t)
	f.svc.ttlOverride = 150 * time.Millisecond
	ch := f.startDrain(t, context.Background())
	a := f.waitPending(t)
	res := awaitResult(t, ch)
	if !res.IsError || !strings.Contains(resultText(res), "expired") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	got, err := f.st.MCPGetApproval(context.Background(), a.ID)
	if err != nil || got.Status != store.ApprovalExpired {
		t.Fatalf("status: %+v %v", got, err)
	}
	if _, _, err := f.svc.Decide(context.Background(), a.ID, true, jonas); err == nil {
		t.Error("approving an expired request must fail")
	}
	if len(f.lb.Calls()) != 0 {
		t.Error("expired call must not execute")
	}
}

func TestApprovalRetryReattaches(t *testing.T) {
	f := newFixture(t)
	f.addBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch1 := f.startDrain(t, ctx)
	a := f.waitPending(t)
	cancel() // client gave up waiting
	if res := awaitResult(t, ch1); !res.IsError {
		t.Fatal("cancelled wait must return an error")
	}
	ch2 := f.startDrain(t, context.Background()) // client retries the same call
	time.Sleep(50 * time.Millisecond)
	if items, _ := f.st.MCPListApprovals(context.Background(), store.ApprovalPending, 10); len(items) != 1 {
		t.Fatalf("retry must re-attach to the pending approval, have %d", len(items))
	}
	if _, _, err := f.svc.Decide(context.Background(), a.ID, true, jonas); err != nil {
		t.Fatal(err)
	}
	if res := awaitResult(t, ch2); res.IsError {
		t.Fatalf("retry result: %q", resultText(res))
	}
	if len(f.lb.Calls()) != 1 {
		t.Errorf("executed %d times", len(f.lb.Calls()))
	}
}

func TestWritePermissionChecks(t *testing.T) {
	f := newFixture(t)
	f.addBackend(t)
	f.insertToken(t, "tok2", "cursor", core.ScopeRead, nil)
	ctx := context.Background()
	readOnly := core.Actor{Type: core.ActorMCP, ID: "tok2", TokenID: "tok2", Scope: core.ScopeRead}
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "drain_server", Arguments: json.RawMessage(`{"backend":"minio","server":"10.0.0.92"}`)}}

	req.Extra = &mcp.RequestExtra{TokenInfo: tokenInfoFor(readOnly)}
	_, _, denied := f.svc.begin(ctx, req, f.svc.byName["drain_server"])
	if denied == nil || resultText(denied) != "tool requires a read+write token" {
		t.Fatalf("read-only token: %+v", denied)
	}
	f.settings(t, func(m *model.MCPSettings) { m.Tools["drain_server"] = model.ToolDisabled })
	req.Extra = &mcp.RequestExtra{TokenInfo: tokenInfoFor(f.mcpActor())}
	_, _, denied = f.svc.begin(ctx, req, f.svc.byName["drain_server"])
	if denied == nil || !strings.Contains(resultText(denied), "disabled") {
		t.Fatalf("disabled tool: %+v", denied)
	}
	if got := f.auditResults(t, "drain_server"); strings.Join(got, ",") != "denied,denied" {
		t.Errorf("audit: %v", got)
	}

	// allow runs immediately without an approval
	f.settings(t, func(m *model.MCPSettings) { m.Tools["drain_server"] = model.ToolAllow })
	res := awaitResult(t, f.startDrain(t, ctx))
	if res.IsError || len(f.lb.Calls()) != 1 {
		t.Fatalf("allow: %q calls=%d", resultText(res), len(f.lb.Calls()))
	}
	if items, _ := f.st.MCPListApprovals(ctx, "all", 10); len(items) != 0 {
		t.Error("allow must not create approvals")
	}
}

type authTokenInfo = auth.TokenInfo

func tokenInfoFor(a core.Actor) *authTokenInfo {
	return &authTokenInfo{UserID: "token:" + a.TokenID, Extra: map[string]any{"actor": a}}
}

func TestLimitToScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, d := range []string{"grafana.home.lan", "shop.example.com"} {
		h := &model.ProxyHost{Domains: []string{d}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 80}}
		if err := f.st.Hosts().Create(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	c := &call{actor: f.mcpActor(), scope: newScope([]string{"*.home.lan"})}
	res, err := f.svc.toolListHosts(ctx, c, listHostsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "grafana.home.lan") || strings.Contains(res.Text, "shop.example.com") {
		t.Errorf("scoped list: %s", res.Text)
	}
	if _, err := f.svc.toolGetHost(ctx, c, getHostArgs{Host: "shop.example.com"}); err == nil {
		t.Error("out-of-scope host must be invisible")
	}
	if _, err := f.svc.planCreateHost(ctx, c, createHostArgs{Domains: []string{"new.example.com"}, Upstream: "10.0.0.9:80"}); err == nil {
		t.Error("out-of-scope create must fail")
	}
	if _, err := f.svc.planCreateHost(ctx, &call{actor: f.mcpActor()}, createHostArgs{Domains: []string{"GRAFANA.home.lan"}, Upstream: "10.0.0.9:80"}); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("duplicate domain: %v", err)
	}
}

// ---------------------------------------------------------------- HTTP round trip

type bearerRT struct {
	token string
	extra map[string]string
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	for k, v := range b.extra {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestStreamableHTTPRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := &model.ProxyHost{Domains: []string{"grafana.home.lan"}, Enabled: true, Websockets: true,
		Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000}}
	if err := f.st.Hosts().Create(ctx, h); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f.svc.Handler())
	defer srv.Close()

	// no token → 401 with a Bearer challenge
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "claude-ai", Version: "1.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearerRT{token: testToken}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names["list_hosts"] || !names["drain_server"] || names["delete_host"] || names["manage_users"] || names[placeholderTool] {
		t.Errorf("tools/list: %v", names)
	}
	if f.svc.ConnectedSessions() != 1 {
		t.Errorf("connected sessions = %d", f.svc.ConnectedSessions())
	}

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_hosts", Arguments: map[string]any{"domain": "graf"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(resultText(res), "grafana.home.lan → http://10.0.0.21:3000") {
		t.Fatalf("list_hosts: %v %q", res.IsError, resultText(res))
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(structured), `"count":1`) {
		t.Errorf("structured: %s", structured)
	}
	rows, _ := f.st.ListAudit(ctx, store.AuditQuery{ActorType: core.ActorMCP, Limit: 5})
	if len(rows) == 0 || rows[0].Action != "list_hosts" || rows[0].ActorName != "Claude Desktop" || rows[0].Result != "ok" || rows[0].Detail != "domain~graf · 1 hosts" {
		t.Errorf("audit: %+v", rows)
	}

	// disabling the server closes the endpoint
	f.settings(t, func(m *model.MCPSettings) { m.Enabled = false })
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_hosts", Arguments: map[string]any{}}); err == nil {
		t.Error("calls must fail once MCP is disabled")
	}
}

func TestStdioProxyRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.settings(t, func(m *model.MCPSettings) { m.Transports = []string{"stdio"} })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.st.Hosts().Create(ctx, &model.ProxyHost{Domains: []string{"jellyfin.home.lan"}, Enabled: true,
		Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.30", Port: 8096}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f.svc.Handler())
	defer srv.Close()

	// plain HTTP clients are refused when only stdio is enabled
	client := mcp.NewClient(&mcp.Implementation{Name: "x", Version: "1"}, nil)
	if _, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{token: testToken}}, MaxRetries: -1}, nil); err == nil {
		t.Error("HTTP transport should be disabled")
	}

	p := &stdioProxy{endpoint: srv.URL + "/mcp", token: testToken, progress: map[any]*mcp.ServerSession{}, local: map[string]bool{}}
	p.server = mcp.NewServer(&mcp.Implementation{Name: "relay", Version: "stdio"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	p.server.AddReceivingMiddleware(p.captureClient)
	if err := p.connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if err := p.syncTools(ctx); err != nil {
		t.Fatal(err)
	}

	serverT, clientT := mcp.NewInMemoryTransports()
	go p.server.Run(ctx, serverT)
	local := mcp.NewClient(&mcp.Implementation{Name: "cursor-vscode", Version: "1"}, nil)
	ls, err := local.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	res, err := ls.CallTool(ctx, &mcp.CallToolParams{Name: "list_hosts", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(resultText(res), "jellyfin.home.lan") {
		t.Fatalf("stdio list_hosts: %q", resultText(res))
	}
	rows, _ := f.st.ListAudit(ctx, store.AuditQuery{ActorType: core.ActorMCP, Limit: 1})
	if len(rows) != 1 || rows[0].ActorName != "Cursor" {
		t.Errorf("stdio client name: %+v", rows)
	}
}
