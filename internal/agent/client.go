package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client talks to an engine agent over its unix socket.
type Client struct {
	Engine string
	Socket string
	http   *http.Client
}

func NewClient(engine, socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{Engine: engine, Socket: socket, http: &http.Client{Transport: tr, Timeout: 90 * time.Second}}
}

// ErrUnavailable is returned when the agent socket cannot be reached.
type ErrUnavailable struct{ Err error }

func (e ErrUnavailable) Error() string { return "engine agent unavailable: " + e.Err.Error() }
func (e ErrUnavailable) Unwrap() error { return e.Err }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("agent %s %s: %s: %s", c.Engine, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.do(ctx, http.MethodGet, PathStatus, nil, &s)
}

func (c *Client) Validate(ctx context.Context, files Files) (*ValidateResponse, error) {
	var r ValidateResponse
	return &r, c.do(ctx, http.MethodPost, PathValidate, ValidateRequest{Files: files}, &r)
}

func (c *Client) Apply(ctx context.Context, req ApplyRequest) (*ApplyResponse, error) {
	var r ApplyResponse
	return &r, c.do(ctx, http.MethodPost, PathApply, req, &r)
}

func (c *Client) Rollback(ctx context.Context) (*ApplyResponse, error) {
	var r ApplyResponse
	return &r, c.do(ctx, http.MethodPost, PathRollback, nil, &r)
}

func (c *Client) Start(ctx context.Context) (*ActionResponse, error) { return c.action(ctx, PathStart) }
func (c *Client) Stop(ctx context.Context) (*ActionResponse, error)  { return c.action(ctx, PathStop) }
func (c *Client) Reload(ctx context.Context) (*ActionResponse, error) {
	return c.action(ctx, PathReload)
}

func (c *Client) action(ctx context.Context, path string) (*ActionResponse, error) {
	var r ActionResponse
	return &r, c.do(ctx, http.MethodPost, path, nil, &r)
}

func (c *Client) Logs(ctx context.Context, since time.Time, limit int) (*LogsResponse, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.UnixMilli(), 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var r LogsResponse
	return &r, c.do(ctx, http.MethodGet, PathLogs+"?"+q.Encode(), nil, &r)
}

func (c *Client) Listeners(ctx context.Context) (*ListenersResponse, error) {
	var r ListenersResponse
	return &r, c.do(ctx, http.MethodGet, PathListeners, nil, &r)
}

// Runtime sends a command to the HAProxy runtime API (haproxy agent only).
func (c *Client) Runtime(ctx context.Context, command string) (string, error) {
	var r RuntimeResponse
	err := c.do(ctx, http.MethodPost, PathRuntime, RuntimeRequest{Command: command}, &r)
	return r.Output, err
}
