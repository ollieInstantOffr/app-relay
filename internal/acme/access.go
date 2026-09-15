package acme

// Access lists: delete protection, IP test, denials, htpasswd import/export.

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func (h *handlers) registerAccessHooks() {
	httpx.AccessListHooks.BeforeSave = func(r *http.Request, prev, next *model.AccessList) error {
		lists, err := h.app.Store.AccessLists().List(r.Context())
		if err != nil {
			return err
		}
		for _, l := range lists {
			if l.ID != next.ID && strings.EqualFold(l.Name, next.Name) {
				return (model.Errs{"name": "An access list named " + l.Name + " already exists"}).Err()
			}
		}
		if next.BasicAuth.Realm == "" {
			next.BasicAuth.Realm = "Restricted"
		}
		return nil
	}
	httpx.AccessListHooks.BeforeDelete = func(r *http.Request, cur *model.AccessList) error {
		usage, err := h.accessUsage(r.Context())
		if err != nil {
			return err
		}
		u := usage[cur.ID]
		var names []string
		for _, x := range u.Hosts {
			names = append(names, x.Domain)
		}
		names = append(names, u.Other...)
		if len(names) > 0 {
			return httpx.InUse("Access list "+cur.Name, names)
		}
		return nil
	}
}

func (h *handlers) accessRoutes(r chi.Router) {
	r.Get("/access-lists/usage", func(w http.ResponseWriter, r *http.Request) {
		u, err := h.accessUsage(r.Context())
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, u)
	})
	r.Post("/access-lists/{id}/test", h.testAccessIP)
	r.Get("/access-lists/{id}/denials", h.accessDenials)
	r.Post("/access-lists/{id}/htpasswd", h.importHtpasswd)
	r.Get("/access-lists/{id}/htpasswd", httpx.RequireAdmin(h.exportHtpasswd))
}

type accessHostRef struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Via    string `json:"via"` // host | location | rate-limit exemption
}

type accessUsage struct {
	Hosts []accessHostRef `json:"hosts"`
	Other []string        `json:"other"`
}

func (h *handlers) accessUsage(ctx context.Context) (map[string]*accessUsage, error) {
	out := map[string]*accessUsage{}
	get := func(id string) *accessUsage {
		if out[id] == nil {
			out[id] = &accessUsage{Hosts: []accessHostRef{}, Other: []string{}}
		}
		return out[id]
	}
	lists, err := h.app.Store.AccessLists().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range lists {
		get(l.ID)
	}
	hosts, err := h.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, x := range hosts {
		seen := map[string]bool{}
		add := func(id, via string) {
			if id == "" || seen[id] {
				return
			}
			seen[id] = true
			get(id).Hosts = append(get(id).Hosts, accessHostRef{ID: x.ID, Domain: firstDomain(x.Domains), Via: via})
		}
		add(x.AccessListID, "host")
		for _, loc := range x.Locations {
			add(loc.AccessListID, "location "+loc.Path)
		}
		if x.RateLimit.Enabled {
			add(x.RateLimit.ExemptAccessListID, "rate-limit exemption")
		}
	}
	addOther := func(id, what string) {
		if id != "" {
			get(id).Other = append(get(id).Other, what)
		}
	}
	if s, err := store.LoadSettings[model.SecuritySettings](ctx, h.app.Store, model.SettingsSecurity); err == nil {
		addOther(s.AdminAccessListID, "Admin UI access")
	}
	if s, err := store.LoadSettings[model.MCPSettings](ctx, h.app.Store, model.SettingsMCP); err == nil {
		addOther(s.AccessListID, "MCP server")
	}
	if s, err := store.LoadSettings[model.DockerSettings](ctx, h.app.Store, model.SettingsDocker); err == nil {
		addOther(s.DefaultAccessListID, "Docker discovery defaults")
	}
	if s, err := store.LoadSettings[model.HAProxySettings](ctx, h.app.Store, model.SettingsHAProxy); err == nil {
		addOther(s.StatsAccessList, "load balancer stats page")
	}
	if s, err := store.LoadSettings[model.GeneralSettings](ctx, h.app.Store, model.SettingsGeneral); err == nil {
		addOther(s.Defaults.AccessListID, "new host defaults")
	}
	return out, nil
}

// AccessDecision is the result of evaluating a client IP against a list the
// way nginx does (allow/deny first match, then auth_basic and satisfy).
type AccessDecision struct {
	Allowed      bool          `json:"allowed"`
	RequiresAuth bool          `json:"requiresAuth"`
	MatchedRule  *model.IPRule `json:"matchedRule"`
	Explanation  string        `json:"explanation"`
}

func evaluateAccess(l *model.AccessList, ip netip.Addr) AccessDecision {
	ip = ip.Unmap()
	var matched *model.IPRule
	for i := range l.Rules {
		p, all, err := model.ParseAccessRule(l.Rules[i].CIDR)
		if err != nil {
			continue
		}
		if all || p.Contains(ip) {
			matched = &l.Rules[i]
			break
		}
	}
	basic := l.BasicAuth.Enabled && len(l.BasicAuth.Users) > 0
	d := AccessDecision{MatchedRule: matched}
	switch {
	case matched != nil && matched.Action == "deny" && (!l.SatisfyAny || !basic):
		d.Explanation = "Denied by rule " + matched.CIDR + " · 403"
	case matched != nil && matched.Action == "allow" && l.SatisfyAny:
		d.Allowed = true
		d.Explanation = "Allowed by rule " + matched.CIDR + " · no password needed (satisfy any)"
	case basic:
		d.RequiresAuth = true
		if matched != nil && matched.Action == "allow" {
			d.Explanation = "IP allowed by " + matched.CIDR + " · then asks for username & password"
		} else {
			d.Explanation = "No allow rule matched · passes with a valid username & password (satisfy any)"
			if !l.SatisfyAny {
				d.Explanation = "No rule matched · asks for username & password"
			}
		}
	case matched != nil:
		d.Allowed = true
		d.Explanation = "Allowed by rule " + matched.CIDR
	default:
		d.Allowed = true
		d.Explanation = "No rule matched · nginx allows by default — add \"deny all\" at the end to block"
	}
	return d
}

func (h *handlers) testAccessIP(w http.ResponseWriter, r *http.Request) {
	l, err := h.app.Store.AccessLists().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var body struct {
		IP   string            `json:"ip"`
		List *model.AccessList `json:"list,omitempty"` // evaluate unsaved edits
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(body.IP))
	if err != nil {
		httpx.Fail(w, r, (model.Errs{"ip": "Not a valid IP address"}).Err())
		return
	}
	if body.List != nil {
		draft := *body.List
		// passwords are irrelevant for evaluation; keep users so auth is known
		for i := range draft.BasicAuth.Users {
			draft.BasicAuth.Users[i].PasswordHash = "x"
		}
		l = &draft
	}
	httpx.WriteJSON(w, http.StatusOK, evaluateAccess(l, ip))
}

func parseSince(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && n > 0 && n <= 90 {
			return time.Duration(n) * 24 * time.Hour
		}
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= 90*24*time.Hour {
		return d
	}
	return def
}

func (h *handlers) accessDenials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.app.Store.AccessLists().Get(r.Context(), id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	usage, err := h.accessUsage(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hosts, err := h.app.Store.Hosts().List(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var ids, names []string
	using := map[string]bool{}
	for _, ref := range usage[id].Hosts {
		if ref.Via != "rate-limit exemption" {
			using[ref.ID] = true
		}
	}
	for _, x := range hosts {
		if using[x.ID] {
			ids = append(ids, x.ID)
			for _, d := range x.Domains {
				if !model.IsWildcardDomain(d) {
					names = append(names, d)
				}
			}
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	since := time.Now().Add(-parseSince(r.URL.Query().Get("since"), 24*time.Hour))
	rows, err := h.app.Store.CertsAccessDenials(r.Context(), ids, names, since, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
}

// parseHtpasswd parses "user:hash" lines, accepting only bcrypt hashes.
func parseHtpasswd(content string) ([]model.BasicAuthUser, error) {
	var users []model.BasicAuthUser
	var problems []string
	seen := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, hash, ok := strings.Cut(line, ":")
		user = strings.TrimSpace(user)
		hash = strings.TrimSpace(hash)
		if i := strings.IndexByte(hash, ':'); i >= 0 { // htdigest / extra fields
			hash = hash[:i]
		}
		if !ok || user == "" || hash == "" {
			problems = append(problems, fmt.Sprintf("line %d: expected user:hash", lineNo))
			continue
		}
		where := fmt.Sprintf("line %d (%s)", lineNo, user)
		switch {
		case strings.HasPrefix(hash, "$2y$"), strings.HasPrefix(hash, "$2a$"), strings.HasPrefix(hash, "$2b$"):
			if _, err := bcrypt.Cost([]byte(hash)); err != nil {
				problems = append(problems, where+": malformed bcrypt hash")
				continue
			}
		case strings.HasPrefix(hash, "$apr1$"):
			problems = append(problems, where+": APR1-MD5 hashes aren't supported — recreate with htpasswd -B (bcrypt)")
			continue
		case strings.HasPrefix(hash, "{SHA}"):
			problems = append(problems, where+": SHA-1 hashes aren't supported — recreate with htpasswd -B (bcrypt)")
			continue
		case strings.HasPrefix(hash, "$1$"), strings.HasPrefix(hash, "$5$"), strings.HasPrefix(hash, "$6$"):
			problems = append(problems, where+": crypt(3) MD5/SHA hashes aren't supported — recreate with htpasswd -B (bcrypt)")
			continue
		default:
			problems = append(problems, where+": plain-text or DES-crypt passwords aren't supported — recreate with htpasswd -B (bcrypt)")
			continue
		}
		if i, dup := seen[user]; dup {
			users[i].PasswordHash = hash
			continue
		}
		seen[user] = len(users)
		users = append(users, model.BasicAuthUser{Username: user, PasswordHash: hash})
	}
	if len(problems) > 0 {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "invalid_htpasswd", "Only bcrypt ($2y$) entries can be imported. "+strings.Join(problems, "; "))
	}
	if len(users) == 0 {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "invalid_htpasswd", "No user:hash lines found")
	}
	return users, nil
}

func (h *handlers) importHtpasswd(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Content string `json:"content"`
		Replace bool   `json:"replace"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	imported, err := parseHtpasswd(body.Content)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	repo := h.app.Store.AccessLists()
	l, err := repo.Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Replace {
		l.BasicAuth.Users = nil
	}
	added, updated := 0, 0
	for _, u := range imported {
		found := false
		for i := range l.BasicAuth.Users {
			if l.BasicAuth.Users[i].Username == u.Username {
				l.BasicAuth.Users[i].PasswordHash = u.PasswordHash
				found = true
				updated++
			}
		}
		if !found {
			l.BasicAuth.Users = append(l.BasicAuth.Users, u)
			added++
		}
	}
	l.BasicAuth.Enabled = true
	if l.BasicAuth.Realm == "" {
		l.BasicAuth.Realm = "Restricted"
	}
	if err := l.Validate(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := repo.Update(r.Context(), l); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.app.Audit(r.Context(), core.AuditEntry{Action: "access_list.import_htpasswd", Target: l.Name, Detail: fmt.Sprintf("%d added · %d updated", added, updated), Result: "saved"})
	h.app.Changed(r.Context(), model.KindAccessList, l.ID, l.Name, core.ActionUpdated)
	l.Redact()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"list": l, "added": added, "updated": updated})
}

func (h *handlers) exportHtpasswd(w http.ResponseWriter, r *http.Request) {
	l, err := h.app.Store.AccessLists().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var b strings.Builder
	for _, u := range l.BasicAuth.Users {
		if u.PasswordHash != "" {
			fmt.Fprintf(&b, "%s:%s\n", u.Username, u.PasswordHash)
		}
	}
	h.app.Audit(r.Context(), core.AuditEntry{Action: "access_list.export_htpasswd", Target: l.Name, Result: "ok"})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.htpasswd"`, unsafeFileChars.ReplaceAllString(l.Name, "_")))
	w.Write([]byte(b.String()))
}
