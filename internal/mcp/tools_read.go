package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

// ---------------------------------------------------------------- views

type refView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type tlsView struct {
	Enabled         bool       `json:"enabled"`
	CertificateID   string     `json:"certificateId,omitempty"`
	CertificateName string     `json:"certificateName,omitempty"`
	Status          string     `json:"status,omitempty"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	DaysLeft        *int       `json:"daysLeft,omitempty"`
	ForceHTTPS      bool       `json:"forceHttps"`
}

type healthView struct {
	Status     string     `json:"status"`
	LatencyMs  int64      `json:"latencyMs,omitempty"`
	HTTPStatus int        `json:"httpStatus,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	CheckedAt  *time.Time `json:"checkedAt,omitempty"`
}

type hostView struct {
	ID            string     `json:"id"`
	Domains       []string   `json:"domains"`
	Enabled       bool       `json:"enabled"`
	Upstream      string     `json:"upstream"`
	BackendID     string     `json:"backendId,omitempty"`
	Websockets    bool       `json:"websockets"`
	BlockExploits bool       `json:"blockExploits"`
	CacheAssets   bool       `json:"cacheAssets"`
	HTTP2         bool       `json:"http2"`
	AccessList    *refView   `json:"accessList,omitempty"`
	TLS           tlsView    `json:"tls"`
	Health        healthView `json:"health"`
	Locations     int        `json:"locations"`
	Source        string     `json:"source"`
	System        bool       `json:"system,omitempty"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

type lookups struct {
	certs map[string]*model.Certificate
	lists map[string]*model.AccessList
}

func (s *Service) lookups(ctx context.Context) (*lookups, error) {
	l := &lookups{certs: map[string]*model.Certificate{}, lists: map[string]*model.AccessList{}}
	certs, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range certs {
		l.certs[certs[i].ID] = &certs[i]
	}
	lists, err := s.app.Store.AccessLists().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range lists {
		l.lists[lists[i].ID] = &lists[i]
	}
	return l, nil
}

func (s *Service) health(target string) healthView {
	if s.app.Health == nil {
		return healthView{Status: core.HealthUnknown}
	}
	st, ok := s.app.Health.Get(target)
	if !ok {
		return healthView{Status: core.HealthUnknown}
	}
	hv := healthView{Status: st.Status, LatencyMs: st.LatencyMs, HTTPStatus: st.HTTPStatus, Detail: st.Detail}
	if !st.CheckedAt.IsZero() {
		t := st.CheckedAt
		hv.CheckedAt = &t
	}
	return hv
}

func (s *Service) hostView(h *model.ProxyHost, l *lookups) hostView {
	v := hostView{
		ID: h.ID, Domains: h.Domains, Enabled: h.Enabled, Upstream: upstreamString(h.Upstream), BackendID: h.Upstream.BackendID,
		Websockets: h.Websockets, BlockExploits: h.BlockExploits, CacheAssets: h.CacheAssets, HTTP2: h.HTTP2,
		Locations: len(h.Locations), Source: h.Source, System: h.System, UpdatedAt: h.UpdatedAt,
		TLS:    tlsView{Enabled: h.CertificateID != "", CertificateID: h.CertificateID, ForceHTTPS: h.ForceHTTPS},
		Health: s.health(core.HostTarget(h.ID)),
	}
	if v.Domains == nil {
		v.Domains = []string{}
	}
	if h.AccessListID != "" {
		ref := &refView{ID: h.AccessListID, Name: "(missing)"}
		if al := l.lists[h.AccessListID]; al != nil {
			ref.Name = al.Name
		}
		v.AccessList = ref
	}
	if c := l.certs[h.CertificateID]; c != nil {
		v.TLS.CertificateName = c.Name
		v.TLS.Status = c.Status
		v.TLS.ExpiresAt = c.NotAfter
		v.TLS.DaysLeft = daysLeft(c.NotAfter, s.now())
	} else if h.CertificateID != "" {
		v.TLS.Status = "missing"
	}
	if !h.Enabled {
		v.Health.Status = core.HealthDisabled
	}
	return v
}

func hostLine(v hostView) string {
	parts := []string{strings.Join(v.Domains, ", ") + " → " + v.Upstream}
	if !v.Enabled {
		parts = append(parts, "disabled")
	} else {
		hs := v.Health.Status
		if v.Health.LatencyMs > 0 {
			hs += fmt.Sprintf(" %d ms", v.Health.LatencyMs)
		}
		parts = append(parts, hs)
	}
	switch {
	case !v.TLS.Enabled:
		parts = append(parts, "HTTP only")
	case v.TLS.DaysLeft != nil:
		parts = append(parts, fmt.Sprintf("TLS %s (%d d left)", v.TLS.CertificateName, *v.TLS.DaysLeft))
	default:
		parts = append(parts, fmt.Sprintf("TLS %s (%s)", v.TLS.CertificateName, v.TLS.Status))
	}
	if v.AccessList != nil {
		parts = append(parts, "access "+v.AccessList.Name)
	}
	if v.Websockets {
		parts = append(parts, "websockets")
	}
	return "- " + strings.Join(parts, " · ") + "  [id " + v.ID + "]"
}

// ---------------------------------------------------------------- args

type listHostsArgs struct {
	Domain  string `json:"domain,omitempty" jsonschema:"Only hosts with a domain containing this text (case-insensitive), e.g. graf or home.lan"`
	Enabled *bool  `json:"enabled,omitempty" jsonschema:"Only enabled (true) or disabled (false) hosts"`
}

type getHostArgs struct {
	Host string `json:"host" jsonschema:"Host id or any of the host's domains, e.g. grafana.home.lan"`
}

type queryLogsArgs struct {
	Host   string `json:"host,omitempty" jsonschema:"Host domain or host id to filter by"`
	Status string `json:"status,omitempty" jsonschema:"Status filter: an exact code (502), a class (5xx) or a comparison (>=500)"`
	IP     string `json:"ip,omitempty" jsonschema:"Client IP address to filter by"`
	Q      string `json:"q,omitempty" jsonschema:"Free-text search over path, user agent and referer"`
	Since  string `json:"since,omitempty" jsonschema:"How far back to look: a duration like 15m, 1h (default) or 7d (max 31d)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum entries to return, newest first (default 50, max 200)"`
}

type nameFilterArgs struct {
	Name string `json:"name,omitempty" jsonschema:"Only items whose name contains this text (case-insensitive)"`
}

type backendStatusArgs struct {
	Backend string `json:"backend,omitempty" jsonschema:"Backend name or id; omit for all backends"`
}

type listCertsArgs struct {
	Domain             string `json:"domain,omitempty" jsonschema:"Only certificates with a domain containing this text"`
	Status             string `json:"status,omitempty" jsonschema:"Only certificates with this status"`
	ExpiringWithinDays *int   `json:"expiringWithinDays,omitempty" jsonschema:"Only certificates that expire within this many days (already expired included)"`
}

type noArgs struct{}

type manageUsersArgs struct {
	Action string `json:"action,omitempty" jsonschema:"What to do. Only list is supported: user management changes are not exposed over MCP"`
}

// ---------------------------------------------------------------- registration

func (s *Service) registerReadTools() {
	addRead(s, toolInfo{Name: "list_hosts", Title: "List proxy hosts",
		Description: "List reverse proxy (nginx or Relay Edge) hosts with their upstream, health (healthy/degraded/down/disabled/unknown), TLS certificate and days until expiry, access list and websockets flag. Filter by domain substring or enabled state. Use get_host for the full configuration of one host."},
		nil, s.toolListHosts)
	addRead(s, toolInfo{Name: "get_host", Title: "Get a proxy host",
		Description: "Get one proxy host by id or domain: summary (health, TLS, access list) plus the complete stored configuration including custom locations, forward auth, rate limiting and advanced proxy settings (custom nginx snippets only apply with nginx)."},
		nil, s.toolGetHost)
	addRead(s, toolInfo{Name: "query_logs", Title: "Query access logs",
		Description: "Search reverse proxy (nginx or Relay Edge) access logs, newest first. Filter by host, status (502, 5xx, >=500), client IP, free text and time window (since: 15m, 1h, 7d). Returns method, path, status, upstream status, client IP, request time and user agent; at most 200 entries."},
		nil, s.toolQueryLogs)
	addRead(s, toolInfo{Name: "list_backends", Title: "List load balancer backends",
		Description: "List HAProxy backends with mode, balancing algorithm, health check and each server's address, weight, role and admin state (ready/drain/maint), merged with live status when HAProxy is running."},
		nil, s.toolListBackends)
	addRead(s, toolInfo{Name: "get_backend_status", Title: "Get backend status",
		Description: "Live HAProxy statistics for one backend (or all): backend status, sessions per second, queue, errors, p95 response time and per-server status (UP/DOWN/DRAIN/MAINT), check detail, traffic share and uptime."},
		nil, s.toolBackendStatus)
	addRead(s, toolInfo{Name: "list_certificates", Title: "List TLS certificates",
		Description: "List TLS certificates with domains, provider, status (valid/pending/failed/expired), expiry date, days left, auto-renew and last error. Filter by domain, status or expiringWithinDays."},
		map[string][]any{"status": {model.CertStatusValid, model.CertStatusPending, model.CertStatusFailed, model.CertStatusExpired}}, s.toolListCertificates)
	addRead(s, toolInfo{Name: "list_access_lists", Title: "List access lists",
		Description: "List access lists (IP allow/deny rules and whether basic auth is on) with the hosts using each. Pass an access list name to create_host or update_host."},
		nil, s.toolListAccessLists)
	addRead(s, toolInfo{Name: "list_streams", Title: "List TCP/UDP streams",
		Description: "List TCP/UDP stream forwards served by the reverse proxy (nginx or Relay Edge): protocol, listen address and ports, forward target, PROXY protocol, enabled flag and health."},
		nil, s.toolListStreams)
	addRead(s, toolInfo{Name: "get_pending_changes", Title: "Get pending changes",
		Description: "List configuration changes that are saved but not yet live (created/updated/deleted hosts, backends, settings…) and the live config version. apply_changes makes them live."},
		nil, s.toolPending)
	addRead(s, toolInfo{Name: "manage_users", Title: "List Relay users",
		Description: "List Relay user accounts with role (admin/editor/viewer), two-factor status, disabled flag and last activity. Creating, changing or deleting users is not available over MCP."},
		map[string][]any{"action": {"list"}}, s.toolManageUsers)
	s.registerExtReadTools()
	s.registerLogTools()
}

// ---------------------------------------------------------------- implementations

func (s *Service) toolListHosts(ctx context.Context, c *call, in listHostsArgs) (*readResult, error) {
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	l, err := s.lookups(ctx)
	if err != nil {
		return nil, err
	}
	out := []hostView{}
	for i := range hosts {
		h := &hosts[i]
		if !c.scope.allowsAll(h.Domains) {
			continue
		}
		if in.Domain != "" && !containsFold(h.Domains, in.Domain) {
			continue
		}
		if in.Enabled != nil && h.Enabled != *in.Enabled {
			continue
		}
		out = append(out, s.hostView(h, l))
	}
	var b strings.Builder
	if len(out) == 0 {
		b.WriteString("No proxy hosts match.")
	} else {
		fmt.Fprintf(&b, "%d proxy host(s):\n", len(out))
		for _, v := range out {
			b.WriteString(hostLine(v) + "\n")
		}
	}
	detail := fmt.Sprintf("%d hosts", len(out))
	if in.Domain != "" {
		detail = "domain~" + in.Domain + " · " + detail
	}
	return &readResult{Text: strings.TrimRight(b.String(), "\n"), Structured: map[string]any{"hosts": out, "count": len(out)}, Detail: detail}, nil
}

func (s *Service) toolGetHost(ctx context.Context, c *call, in getHostArgs) (*readResult, error) {
	h, err := s.findHost(ctx, in.Host)
	if err != nil {
		return nil, err
	}
	if !c.scope.allowsAll(h.Domains) {
		return nil, fmt.Errorf("no proxy host matches %q — use list_hosts to find it", in.Host)
	}
	l, err := s.lookups(ctx)
	if err != nil {
		return nil, err
	}
	v := s.hostView(h, l)
	text := hostLine(v)
	if len(h.Locations) > 0 {
		lines := []string{}
		for _, loc := range h.Locations {
			switch loc.Kind {
			case model.LocationProxy:
				lines = append(lines, fmt.Sprintf("  location %s → %s", loc.Path, upstreamString(loc.Upstream)))
			default:
				lines = append(lines, fmt.Sprintf("  location %s (%s)", loc.Path, loc.Kind))
			}
		}
		text += "\n" + strings.Join(lines, "\n")
	}
	return &readResult{Text: text, Structured: map[string]any{"host": v, "config": h}, Target: first(h.Domains)}, nil
}

type logEntryView struct {
	ID             int64     `json:"id"`
	TS             time.Time `json:"ts"`
	Host           string    `json:"host"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	Status         int       `json:"status"`
	ClientIP       string    `json:"clientIp"`
	UpstreamAddr   string    `json:"upstreamAddr,omitempty"`
	UpstreamStatus string    `json:"upstreamStatus,omitempty"`
	RequestTimeMs  int64     `json:"requestTimeMs"`
	BytesSent      int64     `json:"bytesSent"`
	UserAgent      string    `json:"userAgent,omitempty"`
	Referer        string    `json:"referer,omitempty"`
}

func (s *Service) toolQueryLogs(ctx context.Context, c *call, in queryLogsArgs) (*readResult, error) {
	if s.app.Logs == nil {
		return nil, unavailable(core.ErrNotImplemented, "access log search")
	}
	since, err := parseSince(in.Since)
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	status := strings.NewReplacer("≥", ">=", "≤", "<=").Replace(strings.TrimSpace(in.Status))
	if in.IP != "" && net.ParseIP(strings.TrimSpace(in.IP)) == nil {
		return nil, fmt.Errorf("ip %q is not an IP address", in.IP)
	}
	q := core.AccessQuery{Status: status, ClientIP: strings.TrimSpace(in.IP), Search: strings.TrimSpace(in.Q), Kind: "http", Since: s.now().Add(-since), Limit: limit}
	hostLabel := strings.TrimSpace(in.Host)
	if hostLabel != "" {
		if h, err := s.findHost(ctx, hostLabel); err == nil {
			if !c.scope.allowsAll(h.Domains) {
				return nil, fmt.Errorf("no proxy host matches %q — use list_hosts to find it", in.Host)
			}
			q.HostID = h.ID
			if hostLabel == h.ID {
				hostLabel = first(h.Domains)
			}
		} else {
			if !c.scope.allows(hostLabel) {
				return nil, fmt.Errorf("host %q is outside this token's scope", in.Host)
			}
			q.Host = hostLabel
		}
	}
	page, err := s.app.Logs.QueryAccess(ctx, q)
	if err != nil {
		return nil, unavailable(err, "access log search")
	}
	entries := []logEntryView{}
	for _, e := range page.Entries {
		if c.scope.limited() && !c.scope.allows(e.Host) {
			continue
		}
		entries = append(entries, logEntryView{
			ID: e.ID, TS: e.TS, Host: e.Host, Method: e.Method, Path: e.Path, Status: e.Status, ClientIP: e.ClientIP,
			UpstreamAddr: e.UpstreamAddr, UpstreamStatus: e.UpstreamStatus, RequestTimeMs: int64(e.RequestTime * 1000),
			BytesSent: e.BytesSent, UserAgent: e.UserAgent, Referer: e.Referer,
		})
	}

	filters := []string{}
	if hostLabel != "" {
		filters = append(filters, "host="+hostLabel)
	}
	if status != "" {
		if strings.ContainsAny(status, "<>") {
			filters = append(filters, "status"+statusLabel(status))
		} else {
			filters = append(filters, "status="+status)
		}
	}
	if q.ClientIP != "" {
		filters = append(filters, "ip="+q.ClientIP)
	}
	if q.Search != "" {
		filters = append(filters, "q="+q.Search)
	}
	detail := joinDetail(strings.Join(filters, " "), durationLabel(since))

	var b strings.Builder
	if len(entries) == 0 {
		fmt.Fprintf(&b, "No matching requests in the last %s.", durationLabel(since))
	} else {
		fmt.Fprintf(&b, "%d request(s) in the last %s, newest first:\n", len(entries), durationLabel(since))
		for i, e := range entries {
			if i == 50 {
				fmt.Fprintf(&b, "… %d more in structured output\n", len(entries)-50)
				break
			}
			up := ""
			if e.UpstreamStatus != "" && e.UpstreamStatus != strconv.Itoa(e.Status) {
				up = " (upstream " + e.UpstreamStatus + ")"
			}
			fmt.Fprintf(&b, "%s %s %s%s %d%s · %s · %d ms\n", e.TS.Local().Format("01-02 15:04:05"), e.Method, e.Host, e.Path, e.Status, up, e.ClientIP, e.RequestTimeMs)
		}
	}
	return &readResult{
		Text:       strings.TrimRight(b.String(), "\n"),
		Structured: map[string]any{"entries": entries, "count": len(entries), "since": s.now().Add(-since)},
		Target:     hostLabel,
		Detail:     detail,
	}, nil
}

type serverView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	Port        int    `json:"port"`
	Weight      int    `json:"weight"`
	Role        string `json:"role"`
	State       string `json:"state"`
	Check       bool   `json:"check"`
	Status      string `json:"status,omitempty"`
	CheckDetail string `json:"checkDetail,omitempty"`
}

type backendView struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Mode        string       `json:"mode"`
	Algorithm   string       `json:"algorithm"`
	HealthCheck string       `json:"healthCheck"`
	Status      string       `json:"status"`
	Servers     []serverView `json:"servers"`
}

func (s *Service) lbStats(ctx context.Context) (*core.LBStats, error) {
	if s.app.LB == nil {
		return nil, core.ErrNotImplemented
	}
	return s.app.LB.Stats(ctx)
}

func (s *Service) toolListBackends(ctx context.Context, c *call, in nameFilterArgs) (*readResult, error) {
	backends, err := s.app.Store.Backends().List(ctx)
	if err != nil {
		return nil, err
	}
	statsByID := map[string]*core.BackendStats{}
	if st, err := s.lbStats(ctx); err == nil && st != nil && st.Running {
		for i := range st.Backends {
			statsByID[st.Backends[i].ID] = &st.Backends[i]
		}
	}
	out := []backendView{}
	var b strings.Builder
	for _, be := range backends {
		if !c.scope.allows(be.Name) || (in.Name != "" && !containsFold([]string{be.Name}, in.Name)) {
			continue
		}
		v := backendView{ID: be.ID, Name: be.Name, Mode: be.Mode, Algorithm: be.Algorithm, HealthCheck: be.HealthCheck.Type, Status: "unknown", Servers: []serverView{}}
		bs := statsByID[be.ID]
		if bs != nil {
			v.Status = bs.Status
		}
		lines := []string{}
		for _, sv := range be.Servers {
			x := serverView{ID: sv.ID, Name: sv.Name, Address: sv.Address, Port: sv.Port, Weight: sv.Weight, Role: sv.Role, State: sv.State, Check: sv.Check}
			if bs != nil {
				for _, ss := range bs.Servers {
					if ss.ID == sv.ID || ss.Name == sv.Name {
						x.Status, x.CheckDetail = ss.Status, ss.CheckDetail
					}
				}
			}
			v.Servers = append(v.Servers, x)
			st := x.State
			if x.Status != "" {
				st = x.Status
			}
			lines = append(lines, fmt.Sprintf("%s %s w%d %s", net.JoinHostPort(sv.Address, strconv.Itoa(sv.Port)), sv.Role, sv.Weight, strings.ToLower(st)))
		}
		out = append(out, v)
		fmt.Fprintf(&b, "- %s (%s, %s) · %s · servers: %s  [id %s]\n", be.Name, be.Mode, be.Algorithm, v.Status, strings.Join(lines, "; "), be.ID)
	}
	text := strings.TrimRight(b.String(), "\n")
	if len(out) == 0 {
		text = "No load balancer backends match."
	} else {
		text = fmt.Sprintf("%d backend(s):\n", len(out)) + text
	}
	return &readResult{Text: text, Structured: map[string]any{"backends": out, "count": len(out)}, Detail: fmt.Sprintf("%d backends", len(out))}, nil
}

func (s *Service) toolBackendStatus(ctx context.Context, c *call, in backendStatusArgs) (*readResult, error) {
	var want *model.Backend
	if strings.TrimSpace(in.Backend) != "" {
		b, err := s.findBackend(ctx, in.Backend)
		if err != nil {
			return nil, err
		}
		if !c.scope.allows(b.Name) {
			return nil, fmt.Errorf("no backend named %q — use list_backends to see them", in.Backend)
		}
		want = b
	}
	st, err := s.lbStats(ctx)
	if err != nil {
		return nil, unavailable(err, "load balancer statistics")
	}
	if st == nil || !st.Running {
		return &readResult{Text: "HAProxy is not running, so there are no live statistics.", Structured: map[string]any{"running": false, "backends": []core.BackendStats{}}, Target: in.Backend, Detail: "haproxy not running"}, nil
	}
	backends := []core.BackendStats{}
	for _, bs := range st.Backends {
		if want != nil && bs.ID != want.ID && !strings.EqualFold(bs.Name, want.Name) {
			continue
		}
		if !c.scope.allows(bs.Name) {
			continue
		}
		backends = append(backends, bs)
	}
	if want != nil && len(backends) == 0 {
		return &readResult{Text: fmt.Sprintf("Backend %s has no live statistics (not applied yet?).", want.Name), Structured: map[string]any{"running": true, "backends": backends}, Target: want.Name, Detail: "backend=" + want.Name}, nil
	}
	var b strings.Builder
	for _, bs := range backends {
		fmt.Fprintf(&b, "%s: %s · %d sess/s · %d current · queue %d · %d errors · p95 %d ms\n", bs.Name, bs.Status, bs.SessRate, bs.Current, bs.Queue, bs.Errors, bs.RespP95Ms)
		for _, sv := range bs.Servers {
			extra := ""
			if sv.CheckDetail != "" {
				extra = " (" + sv.CheckDetail + ")"
			}
			fmt.Fprintf(&b, "  %s %s%s · %d%% share · %d sess/s · %d errors · p95 %d ms\n", sv.Address, sv.Status, extra, sv.SharePct, sv.SessRate, sv.Errors, sv.RespP95Ms)
		}
	}
	target, detail := "", fmt.Sprintf("%d backends", len(backends))
	if want != nil {
		target, detail = want.Name, "backend="+want.Name
	}
	return &readResult{Text: strings.TrimRight(b.String(), "\n"), Structured: map[string]any{"running": true, "backends": backends}, Target: target, Detail: detail}, nil
}

type certView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Domains   []string   `json:"domains"`
	Provider  string     `json:"provider"`
	Challenge string     `json:"challenge,omitempty"`
	Status    string     `json:"status"`
	NotAfter  *time.Time `json:"notAfter,omitempty"`
	DaysLeft  *int       `json:"daysLeft,omitempty"`
	AutoRenew bool       `json:"autoRenew"`
	Issuer    string     `json:"issuer,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	UsedBy    []string   `json:"usedBy"`
}

func (s *Service) toolListCertificates(ctx context.Context, c *call, in listCertsArgs) (*readResult, error) {
	certs, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := []certView{}
	var b strings.Builder
	for _, ce := range certs {
		if !c.scope.allowsAll(ce.Domains) {
			continue
		}
		if in.Domain != "" && !containsFold(ce.Domains, in.Domain) {
			continue
		}
		if in.Status != "" && !strings.EqualFold(ce.Status, in.Status) {
			continue
		}
		dl := daysLeft(ce.NotAfter, now)
		if in.ExpiringWithinDays != nil && (dl == nil || *dl > *in.ExpiringWithinDays) {
			continue
		}
		v := certView{ID: ce.ID, Name: ce.Name, Domains: ce.Domains, Provider: ce.Provider, Challenge: ce.Challenge, Status: ce.Status,
			NotAfter: ce.NotAfter, DaysLeft: dl, AutoRenew: ce.AutoRenew, Issuer: ce.Issuer, LastError: ce.LastError, UsedBy: []string{}}
		for _, h := range hosts {
			if h.CertificateID == ce.ID {
				v.UsedBy = append(v.UsedBy, first(h.Domains))
			}
		}
		out = append(out, v)
		exp := "no expiry yet"
		if dl != nil {
			exp = fmt.Sprintf("expires %s (%d d)", ce.NotAfter.Local().Format("2006-01-02"), *dl)
		}
		line := fmt.Sprintf("- %s [%s] %s · %s · %s", ce.Name, strings.Join(ce.Domains, ", "), ce.Status, ce.Provider, exp)
		if ce.LastError != "" {
			line += " · error: " + ce.LastError
		}
		fmt.Fprintf(&b, "%s  [id %s]\n", line, ce.ID)
	}
	text := strings.TrimRight(b.String(), "\n")
	if len(out) == 0 {
		text = "No certificates match."
	} else {
		text = fmt.Sprintf("%d certificate(s):\n", len(out)) + text
	}
	filters := []string{}
	if in.ExpiringWithinDays != nil {
		filters = append(filters, fmt.Sprintf("expiring≤%dd", *in.ExpiringWithinDays))
	}
	if in.Status != "" {
		filters = append(filters, "status="+in.Status)
	}
	if in.Domain != "" {
		filters = append(filters, "domain~"+in.Domain)
	}
	return &readResult{Text: text, Structured: map[string]any{"certificates": out, "count": len(out)}, Detail: joinDetail(strings.Join(filters, " "), fmt.Sprintf("%d certs", len(out)))}, nil
}

type accessListView struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Rules       []model.IPRule `json:"rules"`
	BasicAuth   bool           `json:"basicAuth"`
	AuthUsers   int            `json:"authUsers"`
	SatisfyAny  bool           `json:"satisfyAny"`
	UsedBy      []string       `json:"usedBy"`
}

func (s *Service) toolListAccessLists(ctx context.Context, c *call, _ noArgs) (*readResult, error) {
	lists, err := s.app.Store.AccessLists().List(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	out := []accessListView{}
	var b strings.Builder
	for _, al := range lists {
		v := accessListView{ID: al.ID, Name: al.Name, Description: al.Description, Rules: al.Rules, BasicAuth: al.BasicAuth.Enabled,
			AuthUsers: len(al.BasicAuth.Users), SatisfyAny: al.SatisfyAny, UsedBy: []string{}}
		if v.Rules == nil {
			v.Rules = []model.IPRule{}
		}
		for _, h := range hosts {
			if h.AccessListID == al.ID && c.scope.allowsAll(h.Domains) {
				v.UsedBy = append(v.UsedBy, first(h.Domains))
			}
		}
		out = append(out, v)
		rules := []string{}
		for _, r := range al.Rules {
			rules = append(rules, r.Action+" "+r.CIDR)
		}
		auth := ""
		if al.BasicAuth.Enabled {
			auth = fmt.Sprintf(" · basic auth (%d users)", len(al.BasicAuth.Users))
		}
		fmt.Fprintf(&b, "- %s: %s%s · used by %d host(s)  [id %s]\n", al.Name, strings.Join(rules, ", "), auth, len(v.UsedBy), al.ID)
	}
	text := strings.TrimRight(b.String(), "\n")
	if len(out) == 0 {
		text = "No access lists yet."
	}
	return &readResult{Text: text, Structured: map[string]any{"accessLists": out, "count": len(out)}, Detail: fmt.Sprintf("%d lists", len(out))}, nil
}

type streamView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Protocol      string     `json:"protocol"`
	Listen        string     `json:"listen"`
	Forward       string     `json:"forward"`
	BackendID     string     `json:"backendId,omitempty"`
	ProxyProtocol bool       `json:"proxyProtocol"`
	Enabled       bool       `json:"enabled"`
	Health        healthView `json:"health"`
}

func (s *Service) toolListStreams(ctx context.Context, c *call, in nameFilterArgs) (*readResult, error) {
	streams, err := s.app.Store.Streams().List(ctx)
	if err != nil {
		return nil, err
	}
	out := []streamView{}
	var b strings.Builder
	for _, st := range streams {
		if !c.scope.allows(st.Name) || (in.Name != "" && !containsFold([]string{st.Name}, in.Name)) {
			continue
		}
		ports := st.ForwardPorts
		if ports == "" {
			ports = st.ListenPorts
		}
		fwd := st.ForwardHost + ":" + ports
		if st.BackendID != "" {
			fwd = "backend " + st.BackendID
		}
		v := streamView{ID: st.ID, Name: st.Name, Protocol: st.Protocol, Listen: st.ListenAddress + ":" + st.ListenPorts, Forward: fwd,
			BackendID: st.BackendID, ProxyProtocol: st.ProxyProtocol, Enabled: st.Enabled, Health: s.health(core.StreamTarget(st.ID))}
		if !st.Enabled {
			v.Health.Status = core.HealthDisabled
		}
		out = append(out, v)
		fmt.Fprintf(&b, "- %s %s %s → %s · %s  [id %s]\n", st.Name, strings.ToUpper(st.Protocol), v.Listen, v.Forward, v.Health.Status, st.ID)
	}
	text := strings.TrimRight(b.String(), "\n")
	if len(out) == 0 {
		text = "No streams match."
	}
	return &readResult{Text: text, Structured: map[string]any{"streams": out, "count": len(out)}, Detail: fmt.Sprintf("%d streams", len(out))}, nil
}

func (s *Service) pending(ctx context.Context) (*core.Pending, error) {
	if s.app.Engine == nil {
		return nil, core.ErrNotImplemented
	}
	p, err := s.app.Engine.Pending(ctx)
	if err != nil {
		return nil, unavailable(err, "pending changes")
	}
	return p, nil
}

func pendingLine(it core.PendingItem) string {
	kind := strings.TrimSuffix(strings.ReplaceAll(it.Kind, "_", " "), "s")
	return fmt.Sprintf("%s %s %s", it.Action, kind, it.Name)
}

func (s *Service) toolPending(ctx context.Context, c *call, _ noArgs) (*readResult, error) {
	p, err := s.pending(ctx)
	if err != nil {
		return nil, err
	}
	items := []core.PendingItem{}
	for _, it := range p.Items {
		if c.scope.allows(it.Name) {
			items = append(items, it)
		}
	}
	var b strings.Builder
	if len(items) == 0 {
		fmt.Fprintf(&b, "No pending changes — live config is v%d.", p.LiveVersion)
	} else {
		fmt.Fprintf(&b, "%d pending change(s) on top of live v%d:\n", len(items), p.LiveVersion)
		for _, it := range items {
			b.WriteString("- " + pendingLine(it) + "\n")
		}
	}
	return &readResult{Text: strings.TrimRight(b.String(), "\n"), Structured: map[string]any{"count": len(items), "items": items, "liveVersion": p.LiveVersion}, Detail: fmt.Sprintf("%d pending", len(items))}, nil
}

func (s *Service) toolManageUsers(ctx context.Context, c *call, in manageUsersArgs) (*readResult, error) {
	if a := strings.TrimSpace(in.Action); a != "" && a != "list" {
		return nil, errors.New("only action=list is available: users can't be created or changed over MCP")
	}
	if c.scope.limited() {
		return nil, errors.New("manage_users is not available to tokens limited to specific hosts or backends")
	}
	users, err := s.app.Store.MCPListUsers(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(users, func(i, j int) bool { return users[i].Role < users[j].Role })
	var b strings.Builder
	for _, u := range users {
		flags := []string{u.Role}
		if u.TOTPEnabled {
			flags = append(flags, "2FA")
		}
		if u.Disabled {
			flags = append(flags, "disabled")
		}
		last := "never active"
		if u.LastActiveAt != nil {
			last = "last active " + u.LastActiveAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "- %s (%s) · %s\n", u.Username, strings.Join(flags, ", "), last)
	}
	text := strings.TrimRight(b.String(), "\n")
	if len(users) == 0 {
		text = "No users."
	}
	return &readResult{Text: text, Structured: map[string]any{"users": users, "count": len(users)}, Detail: fmt.Sprintf("%d users", len(users))}, nil
}
