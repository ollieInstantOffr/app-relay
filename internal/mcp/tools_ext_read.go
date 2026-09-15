package mcp

// Read tools covering the rest of the UI. They call the REST API in-process
// (see bridge.go), so results match what the UI shows.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/core"
)

type redirectListArgs struct {
	Domain string `json:"domain,omitempty" jsonschema:"Only redirects with a domain containing this text"`
}

type accessListArgs struct {
	AccessList string `json:"accessList" jsonschema:"Access list id or name"`
}

type versionsArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"How many versions to return, newest first (default 20, max 100)"`
}

type diffArgs struct {
	Pending bool  `json:"pending,omitempty" jsonschema:"Show the pending changes (rendered config vs live) instead of a version"`
	Version int64 `json:"version,omitempty" jsonschema:"Config version id to show"`
	Against int64 `json:"against,omitempty" jsonschema:"Compare against this version instead of the one before it"`
}

type engineLogsArgs struct {
	Engine string `json:"engine" jsonschema:"nginx, edge (Relay Edge) or haproxy"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Lines to return (default 200, max 2000)"`
}

type settingsGetArgs struct {
	Key string `json:"key" jsonschema:"Settings document; secrets are redacted"`
}

func (s *Service) registerExtReadTools() {
	addRead(s, toolInfo{Name: "list_redirects", Title: "List redirects",
		Description: "List redirect rules: domains, from path, target, status code, keep path, certificate, force HTTPS and enabled. Change them with create_redirect / update_redirect / delete_redirect."},
		nil, s.toolListRedirects)
	addRead(s, toolInfo{Name: "get_access_list", Title: "Get an access list",
		Description: "Get one access list by id or name: IP rules in order, basic auth realm and usernames (never passwords), satisfy-any and which hosts use it."},
		nil, s.toolGetAccessList)
	addRead(s, toolInfo{Name: "list_frontends", Title: "List load balancer frontends",
		Description: "List load balancer frontends (HAProxy or Relay Balancer): bind address, mode, routing rules with their conditions and target backends, default backend, PROXY protocol and compression."},
		nil, s.apiReadTool("/frontends", "frontends", true))
	addRead(s, toolInfo{Name: "get_default_host", Title: "Get the default host",
		Description: "What requests for unknown domains or the bare IP get: close, 404, redirect or a proxy host, and the certificate presented to unknown TLS names."},
		nil, s.apiReadTool("/settings/default_host", "default host", true))
	addRead(s, toolInfo{Name: "list_versions", Title: "List config versions",
		Description: "List applied configuration versions, newest first: id, status (live, superseded, rolled_back, failed), who applied it, when, the changes it contained, the proxy engine and any error. Use get_config_diff to see a version's changes and rollback_version to restore one."},
		nil, s.toolListVersions)
	addRead(s, toolInfo{Name: "get_config_diff", Title: "Show a config diff",
		Description: "Show the rendered configuration diff of a version (against the previous one or another version), or of the pending changes (pending: true) — the exact nginx / Relay Edge and HAProxy / Relay Balancer lines that change. Secrets are redacted."},
		nil, s.toolConfigDiff)
	addRead(s, toolInfo{Name: "get_health", Title: "Get health of hosts and streams",
		Description: "Health of every proxy host, stream and backend server from Relay's checks: healthy, degraded, down, disabled or unknown, with the last error and when it was checked."},
		nil, s.apiReadTool("/health", "health", true))
	addRead(s, toolInfo{Name: "get_overview", Title: "Get the traffic overview",
		Description: "The Overview dashboard numbers: requests, 5xx rate, p50/p95 latency and bandwidth for the last 24 hours with the previous-period change, requests to unknown hosts, the active proxy engine's state and certificates expiring soon."},
		nil, s.apiReadTool("/metrics/overview", "overview", true))
	addRead(s, toolInfo{Name: "get_engine_status", Title: "Get engine status",
		Description: "Status of the nginx, Relay Edge, HAProxy and Relay Balancer engines: which proxy engine (proxy) and load balancer engine (lb) are active, whether each agent is reachable and running, version, config hash, last reload and last error."},
		nil, s.apiReadTool("/engines", "engines", false))
	addRead(s, toolInfo{Name: "get_engine_logs", Title: "Get engine output",
		Description: "Recent output of an engine's process (nginx, Relay Edge, HAProxy or Relay Balancer): startup messages, reload results and errors such as ports already in use."},
		map[string][]any{"engine": {"nginx", "edge", "haproxy", "balancer"}}, s.toolEngineLogs)
	addRead(s, toolInfo{Name: "get_updates", Title: "Get update status",
		Description: "Cached update status: Relay's running version vs the newest on its update branch (with new commits), and the running vs latest nginx and HAProxy versions. Use check_for_updates for a fresh check."},
		nil, s.apiReadTool("/engines/updates", "updates", false))
	addRead(s, toolInfo{Name: "list_containers", Title: "List Docker containers",
		Description: "Containers found by Docker discovery on every configured Docker host, running or stopped: name, image, Docker host, suggested upstream address and app port, labels and whether a proxy host already uses them. Pass items to create_hosts_from_docker."},
		nil, s.apiReadTool("/docker/containers", "containers", true))
	addRead(s, toolInfo{Name: "list_dns_providers", Title: "List DNS providers",
		Description: "DNS providers configured for DNS-01 certificate challenges (credentials are never returned)."},
		nil, s.apiReadTool("/dns-providers", "DNS providers", true))
	addRead(s, toolInfo{Name: "get_settings", Title: "Get Relay settings",
		Description: "Read a settings document: general (ports, HTTP/3, proxy engine, host defaults), tls (ACME, HSTS, cipher profile), default_host, haproxy, docker, notifications, backup, engines (update checks) or blocklist. Secrets are redacted; security and MCP settings are not available over MCP."},
		map[string][]any{"key": mcpSettingsKeys}, s.toolGetSettings)
	addRead(s, toolInfo{Name: "list_backups", Title: "List backups",
		Description: "Backups on this instance: file, size, trigger (manual, scheduled, before-upgrade), time and whether it was copied to S3 (remoteStatus)."},
		nil, s.apiReadTool("/backups", "backups", true))
	addRead(s, toolInfo{Name: "get_ports", Title: "Get port usage",
		Description: "Which ports the configuration uses and who owns them (reverse proxy HTTP/HTTPS/HTTP3 and streams, load balancer frontends and stats, the admin UI), with conflicts against ports already in use on the host."},
		nil, s.apiReadTool("/ports", "ports", false))
}

// apiReadTool returns a read tool that forwards a GET request. Instance-wide
// data needs an unrestricted token when restricted is true.
func (s *Service) apiReadTool(path, what string, restricted bool) func(ctx context.Context, c *call, _ noArgs) (*readResult, error) {
	return func(ctx context.Context, c *call, _ noArgs) (*readResult, error) {
		if restricted {
			if err := requireUnrestricted(c, "reading "+what); err != nil {
				return nil, err
			}
		}
		return s.apiRead(ctx, c, path, nil, what)
	}
}

func (s *Service) apiRead(ctx context.Context, c *call, path string, q url.Values, what string) (*readResult, error) {
	p := path
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var v any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, p, nil, &v); err != nil {
		return nil, err
	}
	return readValue(v, what, path), nil
}

// readValue wraps a JSON value as a read result; structured content must be
// an object, so lists are wrapped in {"items": …}.
func readValue(v any, what, detail string) *readResult {
	structured := v
	if _, ok := v.(map[string]any); !ok {
		structured = map[string]any{"items": v}
	}
	return &readResult{Text: jsonText(v), Structured: structured, Target: what, Detail: detail}
}

func (s *Service) toolListRedirects(ctx context.Context, c *call, in redirectListArgs) (*readResult, error) {
	var items []map[string]any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, "/redirects", nil, &items); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	var lines []string
	for _, it := range items {
		domains := mStrs(it["domains"])
		if !c.scope.allowsAll(domains) || (in.Domain != "" && !containsFold(domains, in.Domain)) {
			continue
		}
		out = append(out, it)
		lines = append(lines, fmt.Sprintf("%s · %s%s → %s (%v%s)", mStr(it["id"]), strings.Join(domains, ","), mStr(it["fromPath"]), mStr(it["to"]), it["code"],
			map[bool]string{true: ", disabled", false: ""}[it["enabled"] == false]))
	}
	text := fmt.Sprintf("%d redirect%s", len(out), plural(len(out)))
	if len(lines) > 0 {
		text += ":\n" + strings.Join(lines, "\n")
	}
	return &readResult{Text: text, Structured: map[string]any{"redirects": out}, Target: "redirects", Detail: fmt.Sprintf("%d", len(out))}, nil
}

func (s *Service) toolGetAccessList(ctx context.Context, c *call, in accessListArgs) (*readResult, error) {
	al, err := s.findEntity(core.WithActor(ctx, c.actor), entityKinds[1], in.AccessList)
	if err != nil {
		return nil, err
	}
	shown := entityKinds[1].shown(al)
	return readValue(shown, mStr(al["name"]), "access list "+mStr(al["id"])), nil
}

func (s *Service) toolListVersions(ctx context.Context, c *call, in versionsArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading config history"); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return s.apiRead(ctx, c, "/versions", url.Values{"limit": {strconv.Itoa(limit)}}, "versions")
}

func (s *Service) toolConfigDiff(ctx context.Context, c *call, in diffArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading config diffs"); err != nil {
		return nil, err
	}
	if in.Pending {
		return s.apiRead(ctx, c, "/pending/diff", nil, "pending diff")
	}
	if in.Version <= 0 {
		return nil, fmt.Errorf("pass pending: true or a version id (see list_versions)")
	}
	q := url.Values{}
	if in.Against > 0 {
		q.Set("against", strconv.FormatInt(in.Against, 10))
	}
	return s.apiRead(ctx, c, "/versions/"+strconv.FormatInt(in.Version, 10)+"/diff", q, "v"+strconv.FormatInt(in.Version, 10))
}

func (s *Service) toolEngineLogs(ctx context.Context, c *call, in engineLogsArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading engine output"); err != nil {
		return nil, err
	}
	engine := strings.ToLower(strings.TrimSpace(in.Engine))
	if engine != "nginx" && engine != "edge" && engine != "haproxy" && engine != "balancer" {
		return nil, fmt.Errorf("engine must be nginx, edge, haproxy or balancer")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	return s.apiRead(ctx, c, "/engines/"+engine+"/logs", url.Values{"limit": {strconv.Itoa(limit)}}, engine+" output")
}

func (s *Service) toolGetSettings(ctx context.Context, c *call, in settingsGetArgs) (*readResult, error) {
	if err := requireUnrestricted(c, "reading settings"); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(in.Key)
	if err := checkSettingsKey(key); err != nil {
		return nil, err
	}
	return s.apiRead(ctx, c, "/settings/"+key, nil, key+" settings")
}
