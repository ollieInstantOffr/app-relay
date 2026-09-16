package mcp

// Generic create/update/delete tools for configuration entities that don't
// need hand-written arguments. Every call goes through the REST API bridge,
// so validation, hooks and pending changes behave exactly like the UI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/instantoffr/relay/internal/core"
)

// entityKind describes one configuration entity type.
type entityKind struct {
	noun   string // tool suffix: create_<noun>
	plural string // "access lists"
	path   string // REST collection path
	title  string // "access list"
	fields string // field reference for the tool descriptions
	// label names an entity for summaries and lookups.
	label func(m map[string]any) string
	// scopeNames are the names a limited token must cover; nil means the
	// kind is instance-wide and needs an unrestricted token.
	scopeNames func(m map[string]any) []string
	// redact hides secrets in previews and results.
	redact func(m map[string]any)
}

var entityKinds = []entityKind{
	{
		noun: "redirect", plural: "redirects", path: "/redirects", title: "redirect",
		fields: `domains (array; wildcards allowed), fromPath ("" redirects every path, or e.g. "/blog"), to (full URL; nginx-style variables like $host and $request_uri are allowed), code (301, 302, 307 or 308), keepPath (append the request path), certificateId (to answer https:// before redirecting), forceHttps, enabled`,
		label: func(m map[string]any) string {
			return strings.Join(mStrs(m["domains"]), ",") + mStr(m["fromPath"])
		},
		scopeNames: func(m map[string]any) []string { return mStrs(m["domains"]) },
	},
	{
		noun: "access_list", plural: "access lists", path: "/access-lists", title: "access list",
		fields: `name, description, rules (array of {action: "allow"|"deny", cidr: an address, a CIDR or "all", note}; evaluated top to bottom, first match wins), basicAuth {enabled, realm, users: [{username, password}]} (passwords are hashed on save; leave password out to keep an existing user's), satisfyAny (true: an allowed IP OR a valid password is enough)`,
		label:  func(m map[string]any) string { return mStr(m["name"]) },
		redact: redactAccessListUsers,
	},
	{
		noun: "stream", plural: "streams", path: "/streams", title: "TCP/UDP stream",
		fields:     `name, protocol ("tcp"|"udp"|"both"), listenAddress ("0.0.0.0", "127.0.0.1" or "::"), listenPorts ("25565" or a range "2456-2458"), forwardHost, forwardPorts ("" = same as the listen ports), backendId (instead of forwardHost: send to a TCP load balancer backend), proxyProtocol (send the PROXY header), idleTimeout (e.g. "10m"), enabled, tunnelGatewayId (publish the TCP ports through a tunnel gateway from list_tunnels; an empty string stops publishing)`,
		label:      func(m map[string]any) string { return mStr(m["name"]) },
		scopeNames: func(m map[string]any) []string { return []string{mStr(m["name"])} },
	},
	{
		noun: "backend", plural: "backends", path: "/backends", title: "load balancer backend",
		fields:     `name (letters, digits, dots and dashes), mode ("http"|"tcp"), algorithm ("roundrobin"|"leastconn"|"source"|"uri"|"random"|"first"), servers (array of {name, address, port, weight, role: "active"|"backup", check: bool, state: "ready"|"drain"|"maint"}), healthCheck {type: "none"|"tcp"|"http"|"pgsql"|"mysql"|"redis", method, path, expectStatus (e.g. "200" or "2xx"), host, interval (e.g. "5s"), rise, fall}, forwardClientIp, sendProxy, tlsReencrypt, tlsVerify, retries; sticky sessions and timeouts use the same object shapes list_backends returns`,
		label:      func(m map[string]any) string { return mStr(m["name"]) },
		scopeNames: func(m map[string]any) []string { return []string{mStr(m["name"])} },
	},
	{
		noun: "frontend", plural: "frontends", path: "/frontends", title: "load balancer frontend",
		fields: `name, mode ("http"|"tcp"), bind (e.g. ":8080" or "127.0.0.1:10081"), rules (array of {conditions: [{type: "host"|"path_beg"|"path"|"path_reg"|"header"|"src"|"sni", name (header name for type header), value, negate}], backendId}; the first rule whose conditions all match picks the backend), defaultBackendId, acceptProxy, compression, enabled`,
		label:  func(m map[string]any) string { return mStr(m["name"]) },
	},
}

// checkGlobal refuses limited tokens for instance-wide kinds before any lookup.
func (k entityKind) checkGlobal(c *call) error {
	if k.scopeNames == nil {
		return requireUnrestricted(c, "changing "+k.plural)
	}
	return nil
}

func (k entityKind) checkScope(c *call, m map[string]any) error {
	if !c.scope.limited() {
		return nil
	}
	if k.scopeNames == nil {
		return requireUnrestricted(c, "changing "+k.plural)
	}
	names := k.scopeNames(m)
	if !c.scope.allowsAll(names) {
		return fmt.Errorf("%s %s is outside this token's scope (%s)", k.title, noneLabel(strings.Join(names, ", ")), c.scope)
	}
	return nil
}

func (k entityKind) shown(m map[string]any) map[string]any {
	out := mClone(m)
	if k.redact != nil {
		k.redact(out)
	}
	return out
}

type entityCreateArgs struct {
	Config map[string]any `json:"config" jsonschema:"The new object's fields (see the tool description)"`
	Reason string         `json:"reason,omitempty" jsonschema:"Why this change is needed; shown to the person approving it"`
}

type entityUpdateArgs struct {
	ID      string         `json:"id" jsonschema:"Id or name of the object (redirects: id or one of their domains)"`
	Changes map[string]any `json:"changes" jsonschema:"Fields to change as a JSON merge patch: nested objects merge, arrays replace, null removes a field"`
	Reason  string         `json:"reason,omitempty" jsonschema:"Why this change is needed; shown to the person approving it"`
}

type entityDeleteArgs struct {
	ID     string `json:"id" jsonschema:"Id or name of the object"`
	Reason string `json:"reason,omitempty" jsonschema:"Why it should be deleted; shown to the person approving it"`
}

func (s *Service) registerEntityTools() {
	for _, k := range entityKinds {
		k := k
		addWrite(s, toolInfo{Name: "create_" + k.noun, Title: "Create a " + k.title,
			Description: fmt.Sprintf("Create a %s. Fields: %s. Validated exactly like the Relay UI and saved to pending changes; not live until apply_changes. May wait for human approval.", k.title, k.fields)},
			nil, false, func(ctx context.Context, c *call, in entityCreateArgs) (*plan, error) {
				return s.planEntityCreate(ctx, c, k, in)
			})
		addWrite(s, toolInfo{Name: "update_" + k.noun, Title: "Update a " + k.title,
			Description: fmt.Sprintf("Change a %s (found by id or name) with a JSON merge patch of its fields: %s. Saved to pending changes; not live until apply_changes. May wait for human approval.", k.title, k.fields)},
			nil, false, func(ctx context.Context, c *call, in entityUpdateArgs) (*plan, error) {
				return s.planEntityUpdate(ctx, c, k, in)
			})
		addWrite(s, toolInfo{Name: "delete_" + k.noun, Title: "Delete a " + k.title,
			Description: fmt.Sprintf("Delete a %s by id or name. Refused while other objects still use it. Saved to pending changes; not live until apply_changes. May wait for human approval.", k.title)},
			nil, true, func(ctx context.Context, c *call, in entityDeleteArgs) (*plan, error) {
				return s.planEntityDelete(ctx, c, k, in)
			})
	}
}

// findEntity looks an entity up by id, then by its label (or, for redirects,
// one of its domains).
func (s *Service) findEntity(ctx context.Context, k entityKind, ref string) (map[string]any, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("id is required (the %s's id or name)", k.title)
	}
	var items []map[string]any
	if err := s.apiCall(ctx, http.MethodGet, k.path, nil, &items); err != nil {
		return nil, err
	}
	for _, it := range items {
		if mStr(it["id"]) == ref {
			return it, nil
		}
	}
	var matches []map[string]any
	for _, it := range items {
		if strings.EqualFold(k.label(it), ref) || (k.noun == "redirect" && hasFold(mStrs(it["domains"]), ref)) {
			matches = append(matches, it)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no %s matches %q", k.title, ref)
	case 1:
		return matches[0], nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, fmt.Sprintf("%s (%s)", mStr(m["id"]), k.label(m)))
	}
	return nil, fmt.Errorf("%q matches several %s: %s; pass the id", ref, k.plural, strings.Join(ids, ", "))
}

func (s *Service) planEntityCreate(ctx context.Context, c *call, k entityKind, in entityCreateArgs) (*plan, error) {
	if len(in.Config) == 0 {
		return nil, fmt.Errorf("config is required: the new %s's fields", k.title)
	}
	if err := k.checkScope(c, in.Config); err != nil {
		return nil, err
	}
	label := noneLabel(k.label(in.Config))
	return &plan{
		Summary: fmt.Sprintf("Create %s %s", k.title, bold(label)),
		Target:  label,
		Preview: prettyJSON(k.shown(in.Config)),
		Detail:  "create " + k.noun,
		Exec: func(ctx context.Context) (*outcome, error) {
			var created map[string]any
			if err := s.apiCall(ctx, http.MethodPost, k.path, in.Config, &created); err != nil {
				return nil, err
			}
			created = k.shown(created)
			return &outcome{Text: fmt.Sprintf("Created %s %s (id %s). %s", k.title, noneLabel(k.label(created)), mStr(created["id"]), pendingNote), Structured: created}, nil
		},
	}, nil
}

func (s *Service) planEntityUpdate(ctx context.Context, c *call, k entityKind, in entityUpdateArgs) (*plan, error) {
	if len(in.Changes) == 0 {
		return nil, errors.New("changes is required: the fields to change")
	}
	if err := k.checkGlobal(c); err != nil {
		return nil, err
	}
	cur, err := s.findEntity(core.WithActor(ctx, c.actor), k, in.ID)
	if err != nil {
		return nil, err
	}
	if err := k.checkScope(c, cur); err != nil {
		return nil, err
	}
	patch, _ := json.Marshal(in.Changes)
	next, err := mergePatch(cur, patch)
	if err != nil {
		return nil, err
	}
	id := mStr(cur["id"])
	next["id"] = id
	if err := k.checkScope(c, next); err != nil {
		return nil, err
	}
	label := noneLabel(k.label(cur))
	return &plan{
		Summary: fmt.Sprintf("Update %s %s", k.title, bold(label)),
		Target:  label,
		Preview: jsonDiff(k.shown(cur), k.shown(next)),
		Detail:  "update " + k.noun + " " + id,
		Exec: func(ctx context.Context) (*outcome, error) {
			var saved map[string]any
			if err := s.apiCall(ctx, http.MethodPut, k.path+"/"+url.PathEscape(id), next, &saved); err != nil {
				return nil, err
			}
			saved = k.shown(saved)
			return &outcome{Text: fmt.Sprintf("Updated %s %s. %s", k.title, noneLabel(k.label(saved)), pendingNote), Structured: saved}, nil
		},
	}, nil
}

func (s *Service) planEntityDelete(ctx context.Context, c *call, k entityKind, in entityDeleteArgs) (*plan, error) {
	if err := k.checkGlobal(c); err != nil {
		return nil, err
	}
	cur, err := s.findEntity(core.WithActor(ctx, c.actor), k, in.ID)
	if err != nil {
		return nil, err
	}
	if err := k.checkScope(c, cur); err != nil {
		return nil, err
	}
	id, label := mStr(cur["id"]), noneLabel(k.label(cur))
	return &plan{
		Summary: fmt.Sprintf("Delete %s %s", k.title, bold(label)),
		Target:  label,
		Preview: prettyJSON(k.shown(cur)),
		Detail:  "delete " + k.noun + " " + id,
		Exec: func(ctx context.Context) (*outcome, error) {
			if err := s.apiCall(ctx, http.MethodDelete, k.path+"/"+url.PathEscape(id), nil, nil); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("Deleted %s %s. %s", k.title, label, pendingNote), Structured: map[string]any{"id": id, "deleted": true}}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- map helpers

func mStr(v any) string {
	s, _ := v.(string)
	return s
}

func mStrs(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func mClone(m map[string]any) map[string]any {
	out := map[string]any{}
	if m == nil {
		return out
	}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &out)
	return out
}

func hasFold(ss []string, v string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func noneLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unnamed)"
	}
	return s
}

// redactAccessListUsers hides password hashes and any plaintext password.
func redactAccessListUsers(m map[string]any) {
	ba, _ := m["basicAuth"].(map[string]any)
	users, _ := ba["users"].([]any)
	for _, u := range users {
		if um, ok := u.(map[string]any); ok {
			delete(um, "passwordHash")
			if mStr(um["password"]) != "" {
				um["password"] = "••••••"
			}
		}
	}
}

// jsonText renders a result for the text content, capped in size.
func jsonText(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	const max = 12000
	if len(b) > max {
		return string(b[:max]) + "\n… (truncated; the structured content has the full result)"
	}
	return string(b)
}
