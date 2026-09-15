package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// pendingNote is appended to every config write result.
const pendingNote = "Saved to pending changes — not live until apply_changes runs (or someone clicks Apply in Relay)."

func boolPtr(b bool) *bool { return &b }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// plain strips the **bold** markers used in approval summaries.
func plain(s string) string { return strings.ReplaceAll(s, "**", "") }

func bold(s string) string { return "**" + s + "**" }

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

func containsFold(ss []string, sub string) bool {
	sub = strings.ToLower(sub)
	for _, s := range ss {
		if strings.Contains(strings.ToLower(s), sub) {
			return true
		}
	}
	return false
}

func hasString(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// unavailable turns "not implemented" errors from services that are not
// running (yet) into a readable tool error.
func unavailable(err error, what string) error {
	if errors.Is(err, core.ErrNotImplemented) {
		return fmt.Errorf("%s is not available on this Relay instance yet", what)
	}
	return err
}

// friendlyClient maps well-known MCP clientInfo names to display names.
func friendlyClient(name string) string {
	name = sanitizeLabel(name, 64)
	switch strings.ToLower(name) {
	case "claude-ai", "claude desktop", "claude-desktop":
		return "Claude Desktop"
	case "claude-code":
		return "Claude Code"
	case "cursor-vscode", "cursor":
		return "Cursor"
	case "mcp-inspector", "inspector-client":
		return "MCP Inspector"
	}
	return name
}

// sanitizeLabel removes control characters and caps the length.
func sanitizeLabel(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// ---------------------------------------------------------------- lookups

func (s *Service) findHost(ctx context.Context, ref string) (*model.ProxyHost, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("host is required (a host id or one of its domains)")
	}
	if h, err := s.app.Store.Hosts().Get(ctx, ref); err == nil {
		return h, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	want := model.HostNormalizeDomain(ref)
	for i := range hosts {
		for _, d := range hosts[i].Domains {
			if strings.EqualFold(d, want) {
				return &hosts[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no proxy host matches %q — use list_hosts to find it", ref)
}

func (s *Service) findBackend(ctx context.Context, ref string) (*model.Backend, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("backend is required (a backend name or id)")
	}
	items, err := s.app.Store.Backends().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == ref {
			return &items[i], nil
		}
	}
	for i := range items {
		if strings.EqualFold(items[i].Name, ref) {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("no backend named %q — use list_backends to see them", ref)
}

// findServer matches a server by id, haproxy name, address:port or address.
func findServer(b *model.Backend, ref string) (*model.Server, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("server is required (address, address:port, server name or id)")
	}
	for i := range b.Servers {
		sv := &b.Servers[i]
		if sv.ID == ref || strings.EqualFold(sv.Name, ref) || net.JoinHostPort(sv.Address, strconv.Itoa(sv.Port)) == ref {
			return sv, nil
		}
	}
	var matches []*model.Server
	for i := range b.Servers {
		if strings.EqualFold(b.Servers[i].Address, ref) {
			matches = append(matches, &b.Servers[i])
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		addrs := make([]string, 0, len(b.Servers))
		for _, sv := range b.Servers {
			addrs = append(addrs, net.JoinHostPort(sv.Address, strconv.Itoa(sv.Port)))
		}
		return nil, fmt.Errorf("backend %s has no server %q (servers: %s)", b.Name, ref, strings.Join(addrs, ", "))
	}
	return nil, fmt.Errorf("%s matches %d servers in backend %s — pass address:port or the server name", ref, len(matches), b.Name)
}

func (s *Service) findAccessList(ctx context.Context, ref string) (*model.AccessList, error) {
	items, err := s.app.Store.AccessLists().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == ref || strings.EqualFold(items[i].Name, ref) {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("no access list named %q — use list_access_lists to see them", ref)
}

func (s *Service) findCertificate(ctx context.Context, ref string) (*model.Certificate, error) {
	items, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == ref {
			return &items[i], nil
		}
	}
	for i := range items {
		if strings.EqualFold(items[i].Name, ref) {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("no certificate %q — use list_certificates to see them, or pass \"auto\"", ref)
}

func (s *Service) findDNSProvider(ctx context.Context, ref string) (*model.DNSProvider, error) {
	items, err := s.app.Store.DNSProviders().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].ID == ref || strings.EqualFold(items[i].Name, ref) {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("no DNS provider named %q", ref)
}

// certCovers reports whether a certificate name covers a host domain
// (exact match, or a wildcard covering exactly one extra label).
func certCovers(certDomain, domain string) bool {
	cd, d := strings.ToLower(certDomain), strings.ToLower(domain)
	if cd == d {
		return true
	}
	if strings.HasPrefix(cd, "*.") {
		suffix := cd[1:]
		if strings.HasSuffix(d, suffix) {
			label := strings.TrimSuffix(d, suffix)
			return label != "" && !strings.Contains(label, ".") && label != "*"
		}
	}
	return false
}

// matchCertificate picks the valid certificate covering every domain that
// expires last.
func matchCertificate(certs []model.Certificate, domains []string, now time.Time) *model.Certificate {
	var best *model.Certificate
	for i := range certs {
		c := &certs[i]
		if c.Status != model.CertStatusValid || (c.NotAfter != nil && !c.NotAfter.After(now)) {
			continue
		}
		ok := len(domains) > 0
		for _, d := range domains {
			covered := false
			for _, cd := range c.Domains {
				if certCovers(cd, d) {
					covered = true
					break
				}
			}
			if !covered {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if best == nil || (c.NotAfter != nil && (best.NotAfter == nil || c.NotAfter.After(*best.NotAfter))) {
			best = c
		}
	}
	return best
}

func daysLeft(t *time.Time, now time.Time) *int {
	if t == nil {
		return nil
	}
	d := int(math.Floor(t.Sub(now).Hours() / 24))
	return &d
}

// parseUpstream accepts "http://10.0.0.21:3000", "10.0.0.21:3000",
// "https://nas.lan" (default ports) or "http://app:8080/sub".
func parseUpstream(raw string) (model.Upstream, error) {
	s := strings.TrimSpace(raw)
	bad := fmt.Errorf("upstream %q must look like http://10.0.0.21:3000", raw)
	if s == "" {
		return model.Upstream{}, errors.New("upstream is required, e.g. http://10.0.0.21:3000")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return model.Upstream{}, bad
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return model.Upstream{}, fmt.Errorf("upstream scheme must be http or https, not %q", u.Scheme)
	}
	port := 80
	if scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return model.Upstream{}, bad
		}
		port = n
	}
	host := u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := u.EscapedPath()
	if path == "/" {
		path = ""
	}
	return model.Upstream{Scheme: scheme, Host: host, Port: port, Path: path}, nil
}

func upstreamString(u model.Upstream) string {
	if u.BackendID != "" && u.Host == "" {
		return "backend " + u.BackendID
	}
	port := ""
	if u.Port > 0 {
		port = ":" + strconv.Itoa(u.Port)
	}
	return u.Scheme + "://" + u.Host + port + u.Path
}

// parseSince parses "15m", "1h", "7d" (default 1h, max 31 days).
func parseSince(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return time.Hour, nil
	}
	var d time.Duration
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil {
			return 0, fmt.Errorf("since %q: use a duration like 15m, 1h or 7d", v)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, fmt.Errorf("since %q: use a duration like 15m, 1h or 7d", v)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("since must be positive")
	}
	if d > 31*24*time.Hour {
		d = 31 * 24 * time.Hour
	}
	return d, nil
}

func durationLabel(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return d.String()
}

// statusLabel renders a status filter the way the audit log shows it ("≥500").
func statusLabel(expr string) string {
	r := strings.NewReplacer(">=", "≥", "<=", "≤")
	return r.Replace(strings.TrimSpace(expr))
}

// ---------------------------------------------------------------- previews

func prettyLines(v any) []string {
	if v == nil {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return []string{fmt.Sprint(v)}
	}
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// diffJSON renders a line diff of the pretty JSON of before and after, with
// "+ ", "- " and "  " prefixes (the approvals UI renders it as a diff).
func diffJSON(before, after any) string {
	return diffLines(prettyLines(before), prettyLines(after))
}

func diffLines(a, b []string) string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, "  "+a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "- "+a[i])
			i++
		default:
			out = append(out, "+ "+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+a[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+b[j])
	}
	return strings.Join(out, "\n")
}

// argTarget extracts a best-effort target label from raw tool arguments
// (used when a call is denied before its arguments are resolved).
func argTarget(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"host", "backend", "domain", "name"} {
		if v, ok := m[k].(string); ok && v != "" {
			if k == "backend" {
				if sv, ok := m["server"].(string); ok && sv != "" {
					return v + " / " + sv
				}
			}
			return v
		}
	}
	if ds, ok := m["domains"].([]any); ok && len(ds) > 0 {
		if d, ok := ds[0].(string); ok {
			return d
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
