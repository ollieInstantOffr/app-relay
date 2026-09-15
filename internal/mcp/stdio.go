package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type StdioOptions struct {
	URL   string // relay base URL, e.g. http://127.0.0.1:8181
	Token string
}

// RunStdio implements `relay mcp-stdio`: an MCP stdio server proxying to the HTTP endpoint.
//
// Local clients spawn it with `docker exec -i relay relay mcp-stdio --token rl_mcp_…`.
// It connects to Relay's /mcp endpoint as an MCP client (the endpoint enforces
// the token, tool permissions, approvals and the "stdio" transport setting),
// mirrors the tool list and forwards tool calls. Nothing but protocol traffic
// is written to stdout.
func RunStdio(ctx context.Context, o StdioOptions) error {
	token := strings.TrimSpace(o.Token)
	if token == "" {
		return errors.New("an MCP token is required: relay mcp-stdio --token rl_mcp_… (or set RELAY_TOKEN)")
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		return fmt.Errorf("expected an MCP token starting with %s", tokenPrefix)
	}
	base := strings.TrimRight(strings.TrimSpace(o.URL), "/")
	if base == "" {
		base = "http://127.0.0.1:8181"
	}
	p := &stdioProxy{endpoint: base + "/mcp", token: token, progress: map[any]*mcp.ServerSession{}, local: map[string]bool{}}
	p.server = mcp.NewServer(&mcp.Implementation{Name: "relay", Title: "Relay", Version: "stdio"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	p.server.AddReceivingMiddleware(p.captureClient)

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := p.connect(connectCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to %s: %w", p.endpoint, err)
	}
	defer p.close()
	if err := p.syncTools(ctx); err != nil {
		return fmt.Errorf("list tools from %s: %w", p.endpoint, err)
	}
	fmt.Fprintf(os.Stderr, "relay mcp-stdio: connected to %s\n", p.endpoint)
	err = p.server.Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type stdioProxy struct {
	endpoint   string
	token      string
	server     *mcp.Server
	clientName atomic.Value // string

	mu       sync.Mutex
	session  *mcp.ClientSession
	local    map[string]bool            // tool names registered locally
	progress map[any]*mcp.ServerSession // progress token → local session
}

// roundTrip adds the bearer token and stdio markers to upstream requests.
type stdioRoundTripper struct {
	p    *stdioProxy
	base http.RoundTripper
}

func (rt stdioRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+rt.p.token)
	r.Header.Set(headerTransport, "stdio")
	if name, _ := rt.p.clientName.Load().(string); name != "" {
		r.Header.Set(headerClient, name)
	}
	return rt.base.RoundTrip(r)
}

// captureClient remembers the local client's name for the upstream header.
func (p *stdioProxy) captureClient(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "initialize" {
			if ip, ok := req.GetParams().(*mcp.InitializeParams); ok && ip != nil && ip.ClientInfo != nil {
				name := ip.ClientInfo.Title
				if name == "" {
					name = ip.ClientInfo.Name
				}
				p.clientName.Store(sanitizeLabel(name, 64))
			}
		}
		return next(ctx, method, req)
	}
}

func (p *stdioProxy) connect(ctx context.Context) error {
	client := mcp.NewClient(&mcp.Implementation{Name: stdioClientName, Version: "1"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := p.syncTools(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "relay mcp-stdio: refresh tools: %v\n", err)
				}
			}()
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			p.mu.Lock()
			sess := p.progress[req.Params.ProgressToken]
			p.mu.Unlock()
			if sess != nil {
				_ = sess.NotifyProgress(ctx, req.Params)
			}
		},
	})
	transport := &mcp.StreamableClientTransport{
		Endpoint:   p.endpoint,
		HTTPClient: &http.Client{Transport: stdioRoundTripper{p: p, base: http.DefaultTransport}},
	}
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}
	p.mu.Lock()
	old := p.session
	p.session = sess
	p.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (p *stdioProxy) close() {
	p.mu.Lock()
	sess := p.session
	p.session = nil
	p.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
}

func (p *stdioProxy) current() *mcp.ClientSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session
}

// syncTools mirrors the upstream tool list onto the local server.
func (p *stdioProxy) syncTools(ctx context.Context) error {
	sess := p.current()
	if sess == nil {
		return errors.New("not connected")
	}
	seen := map[string]bool{}
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		seen[t.Name] = true
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		p.server.AddTool(&mcp.Tool{
			Name: t.Name, Title: t.Title, Description: t.Description,
			InputSchema: schema, OutputSchema: t.OutputSchema, Annotations: t.Annotations,
		}, p.forward(t.Name))
	}
	p.mu.Lock()
	var gone []string
	for name := range p.local {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	p.local = seen
	p.mu.Unlock()
	if len(gone) > 0 {
		p.server.RemoveTools(gone...)
	}
	return nil
}

func (p *stdioProxy) forward(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Newer clients send clientInfo per request instead of in initialize.
		if ci := req.ClientInfo(); ci != nil {
			n := ci.Title
			if n == "" {
				n = ci.Name
			}
			if n = sanitizeLabel(n, 64); n != "" {
				p.clientName.Store(n)
			}
		}
		params := &mcp.CallToolParams{Name: name}
		if len(req.Params.Arguments) > 0 {
			params.Arguments = req.Params.Arguments
		}
		if tok := req.Params.GetProgressToken(); tok != nil && req.Session != nil {
			params.SetProgressToken(tok)
			p.mu.Lock()
			p.progress[tok] = req.Session
			p.mu.Unlock()
			defer func() {
				p.mu.Lock()
				delete(p.progress, tok)
				p.mu.Unlock()
			}()
		}
		res, err := p.callTool(ctx, params)
		if err != nil {
			return errorResult(fmt.Errorf("Relay MCP endpoint: %v", err)), nil
		}
		return res, nil
	}
}

// callTool forwards a call, reconnecting when the upstream session is gone.
// A call is only retried when the server reported the session missing, which
// means it was never processed.
func (p *stdioProxy) callTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	sess := p.current()
	if sess == nil {
		if err := p.connect(ctx); err != nil {
			return nil, err
		}
		sess = p.current()
	}
	res, err := sess.CallTool(ctx, params)
	if err == nil {
		return res, nil
	}
	missing := errors.Is(err, mcp.ErrSessionMissing)
	if !missing && !errors.Is(err, mcp.ErrConnectionClosed) {
		return nil, err
	}
	if cerr := p.connect(ctx); cerr != nil {
		return nil, fmt.Errorf("%v (reconnect failed: %v)", err, cerr)
	}
	if !missing {
		return nil, err
	}
	return p.current().CallTool(ctx, params)
}
