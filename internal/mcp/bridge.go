package mcp

// The API bridge runs REST API requests in-process as the MCP caller. Tools
// built on it get exactly the behaviour of the same action in the UI: hooks,
// validation, pending changes, audit entries and live events — and they can
// never do more than the token owner's role allows (admin-only routes stay
// admin-only).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/instantoffr/relay/internal/core"
)

// apiError is a non-2xx API response.
type apiError struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]string
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if len(e.Fields) == 0 {
		return msg
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return msg + " (" + strings.Join(parts, "; ") + ")"
}

// recorder is a minimal in-memory http.ResponseWriter.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

// apiCall sends method /api<path> with an optional JSON body as the actor in
// ctx and decodes a successful JSON response into out (when non-nil).
func (s *Service) apiCall(ctx context.Context, method, path string, body, out any) error {
	if s.app.API == nil {
		return errors.New("the Relay API is not available in this process")
	}
	actor := core.ActorFrom(ctx)
	if actor.IsZero() {
		return errors.New("unauthenticated")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(core.WithInternalCall(ctx), method, "http://relay.internal/api"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.RemoteAddr = "127.0.0.1:0"
	if actor.IP != "" {
		req.RemoteAddr = net.JoinHostPort(actor.IP, "0")
	}
	rec := &recorder{header: http.Header{}}
	s.app.API.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status >= 400 {
		var eb struct {
			Error struct {
				Code    string            `json:"code"`
				Message string            `json:"message"`
				Fields  map[string]string `json:"fields"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.body.Bytes(), &eb)
		return &apiError{Status: rec.status, Code: eb.Error.Code, Message: eb.Error.Message, Fields: eb.Error.Fields}
	}
	if out != nil && rec.body.Len() > 0 {
		if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
			return fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- JSON helpers

// mergePatch applies an RFC 7386 JSON merge patch to the JSON form of current:
// objects merge recursively, null removes a key, anything else replaces.
func mergePatch(current any, patch json.RawMessage) (map[string]any, error) {
	base, err := toMap(current)
	if err != nil {
		return nil, err
	}
	var p any
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, fmt.Errorf("the changes must be a JSON object: %w", err)
	}
	pm, ok := p.(map[string]any)
	if !ok {
		return nil, errors.New("the changes must be a JSON object")
	}
	return mergeInto(base, pm), nil
}

func mergeInto(dst, patch map[string]any) map[string]any {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeInto(dm, pm)
				continue
			}
			dst[k] = mergeInto(map[string]any{}, pm)
			continue
		}
		dst[k] = v
	}
	return dst
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// prettyJSON renders v for previews, dropping bookkeeping fields and capping
// the size shown to the approver.
func prettyJSON(v any) string {
	m, err := toMap(v)
	if err != nil {
		b, _ := json.MarshalIndent(v, "", "  ")
		return string(b)
	}
	for _, k := range []string{"createdAt", "updatedAt", "createdBy", "updatedBy"} {
		delete(m, k)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	const max = 6000
	if len(b) > max {
		return string(b[:max]) + "\n…"
	}
	return string(b)
}

// jsonDiff shows before → after as diff lines for the approver.
func jsonDiff(before, after any) string {
	d := diffLines(strings.Split(prettyJSON(before), "\n"), strings.Split(prettyJSON(after), "\n"))
	lines := strings.Split(d, "\n")
	// Keep changed lines with a little context so long objects stay readable.
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			for j := max(0, i-2); j <= min(len(lines)-1, i+2); j++ {
				keep[j] = true
			}
		}
	}
	var out []string
	skipped := false
	for i, l := range lines {
		if keep[i] {
			out = append(out, l)
			skipped = false
		} else if !skipped {
			out = append(out, "  …")
			skipped = true
		}
	}
	if len(out) == 0 {
		return "(no changes)"
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- scope

// requireUnrestricted refuses instance-wide operations for tokens limited to
// certain domains or backends.
func requireUnrestricted(c *call, what string) error {
	if c.scope.limited() {
		return fmt.Errorf("this MCP token is limited to %s; %s needs a token without that limit", c.scope, what)
	}
	return nil
}
