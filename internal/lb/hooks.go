package lb

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render/haproxy"
	"github.com/instantoffr/relay/internal/store"
)

func registerHooks(app *core.App) {
	httpx.BackendHooks.BeforeSave = func(r *http.Request, prev, next *model.Backend) error {
		return beforeSaveBackend(r.Context(), app, prev, next)
	}
	httpx.BackendHooks.BeforeDelete = func(r *http.Request, cur *model.Backend) error {
		snap, err := app.Store.Snapshot(r.Context())
		if err != nil {
			return err
		}
		if names := backendDependents(snap, cur.ID); len(names) > 0 {
			return httpx.InUse("Backend "+cur.Name, names)
		}
		return nil
	}
	httpx.FrontendHooks.BeforeSave = func(r *http.Request, prev, next *model.Frontend) error {
		return beforeSaveFrontend(r.Context(), app, prev, next)
	}
	httpx.FrontendHooks.BeforeDelete = func(r *http.Request, cur *model.Frontend) error {
		snap, err := app.Store.Snapshot(r.Context())
		if err != nil {
			return err
		}
		if names := frontendDependents(snap, cur); len(names) > 0 {
			return httpx.InUse("Frontend "+cur.Name, names)
		}
		return nil
	}
	httpx.SettingsHooks[model.SettingsHAProxy] = &httpx.SettingsHook{
		BeforeSave: func(r *http.Request, _, next any) error {
			s, ok := next.(*model.HAProxySettings)
			if !ok {
				return nil
			}
			return beforeSaveSettings(r.Context(), app, s)
		},
	}
}

// ---------------------------------------------------------------- normalisation

func haproxySettings(ctx context.Context, app *core.App) model.HAProxySettings {
	s, err := store.LoadSettings[model.HAProxySettings](ctx, app.Store, model.SettingsHAProxy)
	if err != nil {
		return store.DefaultHAProxy()
	}
	return s
}

// normalizeBackend fills defaults for missing fields and assigns stable server
// ids and names. prev (may be nil) supplies admin states the client omitted.
func normalizeBackend(s model.HAProxySettings, prev, b *model.Backend) {
	b.Name = strings.TrimSpace(b.Name)
	if b.Mode == "" {
		b.Mode = "http"
	}
	if b.Algorithm == "" {
		b.Algorithm = "roundrobin"
	}
	if b.Source == "" {
		if prev != nil && prev.Source != "" {
			b.Source = prev.Source
		} else {
			b.Source = model.SourceManual
		}
	}
	hc := &b.HealthCheck
	if hc.Type == "" {
		if b.Mode == "http" {
			hc.Type = "http"
		} else {
			hc.Type = "tcp"
		}
	}
	if hc.Type == "http" {
		hc.Method = strings.ToUpper(strings.TrimSpace(hc.Method))
		if hc.Method == "" {
			hc.Method = "GET"
		}
		hc.Path = strings.TrimSpace(hc.Path)
		if hc.Path == "" {
			hc.Path = "/"
		}
		if strings.TrimSpace(hc.ExpectStatus) == "" {
			hc.ExpectStatus = "2xx"
		}
	}
	hc.Interval = strings.TrimSpace(hc.Interval)
	if hc.Rise <= 0 {
		hc.Rise = s.Rise
	}
	if hc.Fall <= 0 {
		hc.Fall = s.Fall
	}
	if strings.TrimSpace(b.Sticky.CookieName) == "" {
		b.Sticky.CookieName = "SRVID"
	}
	if b.Sticky.Mode == "" || (b.Mode == "tcp" && b.Sticky.Mode != "source") {
		if b.Mode == "http" {
			b.Sticky.Mode = "insert"
		} else {
			b.Sticky.Mode = "source"
		}
	}
	if !b.TLSReencrypt {
		b.TLSVerify = false
	}
	b.Timeouts.Connect = strings.TrimSpace(b.Timeouts.Connect)
	b.Timeouts.Server = strings.TrimSpace(b.Timeouts.Server)
	b.Timeouts.Queue = strings.TrimSpace(b.Timeouts.Queue)

	prevState := map[string]string{}
	if prev != nil {
		for _, srv := range prev.Servers {
			prevState[srv.ID] = srv.State
		}
	}
	if b.Servers == nil {
		b.Servers = []model.Server{}
	}
	used := map[string]bool{}
	for _, srv := range b.Servers {
		if n := strings.TrimSpace(srv.Name); n != "" {
			used[strings.ToLower(n)] = true
		}
	}
	next := 1
	for i := range b.Servers {
		srv := &b.Servers[i]
		if srv.ID == "" {
			srv.ID = store.NewID()
		}
		srv.Address = strings.TrimSpace(srv.Address)
		srv.Name = strings.TrimSpace(srv.Name)
		if srv.Name == "" && b.Name != "" {
			for {
				cand := fmt.Sprintf("%s-%d", b.Name, next)
				next++
				if !used[strings.ToLower(cand)] {
					srv.Name = cand
					used[strings.ToLower(cand)] = true
					break
				}
			}
		}
		if srv.Role == "" {
			srv.Role = model.ServerActive
		}
		if srv.Weight == 0 {
			srv.Weight = 100
		}
		if st, ok := prevState[srv.ID]; ok && st != "" {
			// Admin state is owned by the state endpoint (runtime + persist);
			// config saves from a possibly stale editor keep the stored state.
			srv.State = st
		} else if srv.State == "" {
			srv.State = model.ServerStateReady
		}
	}
}

func normalizeFrontend(prev, f *model.Frontend) {
	f.Name = strings.TrimSpace(f.Name)
	f.Bind = strings.TrimSpace(f.Bind)
	if f.Mode == "" {
		f.Mode = "http"
	}
	if f.Source == "" {
		if prev != nil && prev.Source != "" {
			f.Source = prev.Source
		} else {
			f.Source = model.SourceManual
		}
	}
	if prev != nil && f.HostID == "" {
		f.HostID = prev.HostID
	}
	if f.Rules == nil {
		f.Rules = []model.FrontendRule{}
	}
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.ID == "" {
			r.ID = store.NewID()
		}
		for j := range r.Conditions {
			c := &r.Conditions[j]
			c.Value = strings.TrimSpace(c.Value)
			c.Name = strings.TrimSpace(c.Name)
			if c.Type == model.CondHost || c.Type == model.CondSNI {
				c.Value = strings.ToLower(c.Value)
			}
		}
	}
}

// ---------------------------------------------------------------- save hooks

func beforeSaveBackend(ctx context.Context, app *core.App, prev, next *model.Backend) error {
	normalizeBackend(haproxySettings(ctx, app), prev, next)
	snap, err := app.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	e := model.Errs{}
	for _, b := range snap.Backends {
		if b.ID != next.ID && strings.EqualFold(b.Name, next.Name) {
			e.Add("name", "A backend named %s already exists", b.Name)
		}
	}
	if next.Mode == "tcp" && next.ID != "" {
		for _, f := range snap.Frontends {
			if f.Mode == "http" && frontendUsesBackend(&f, next.ID) {
				e.Add("mode", "HTTP frontend %s routes to this backend; it must stay in HTTP mode", f.Name)
			}
		}
	}
	return e.Err()
}

func beforeSaveFrontend(ctx context.Context, app *core.App, prev, next *model.Frontend) error {
	normalizeFrontend(prev, next)
	snap, err := app.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	return checkFrontend(app, snap, next)
}

// checkFrontend runs the cross-entity checks for a frontend draft.
func checkFrontend(app *core.App, snap *model.Snapshot, next *model.Frontend) error {
	e := model.Errs{}
	for _, f := range snap.Frontends {
		if f.ID != next.ID && strings.EqualFold(f.Name, next.Name) {
			e.Add("name", "A frontend named %s already exists", f.Name)
		}
	}
	backend := func(id string) *model.Backend {
		for i := range snap.Backends {
			if snap.Backends[i].ID == id {
				return &snap.Backends[i]
			}
		}
		return nil
	}
	checkRef := func(field, id string) {
		if id == "" {
			return
		}
		b := backend(id)
		if b == nil {
			e.Add(field, "Backend no longer exists")
			return
		}
		if next.Mode == "http" && b.Mode != "http" {
			e.Add(field, "%s is a TCP backend; an HTTP frontend can only use HTTP backends", b.Name)
		}
	}
	checkRef("defaultBackendId", next.DefaultBackendID)
	for i, r := range next.Rules {
		checkRef(fmt.Sprintf("rules.%d.backendId", i), r.BackendID)
	}
	if next.Enabled {
		if addr, port, err := model.SplitBind(next.Bind); err == nil {
			if owner := portOwner(app, snap, next.ID, addr, port); owner != "" {
				e.Add("bind", "Port %d is already used by %s", port, owner)
			}
		}
	}
	return e.Err()
}

func beforeSaveSettings(ctx context.Context, app *core.App, s *model.HAProxySettings) error {
	s.StatsBind = strings.TrimSpace(s.StatsBind)
	e := model.Errs{}
	snap, err := app.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	if s.StatsEnabled {
		if addr, port, err := model.SplitBind(s.StatsBind); err == nil {
			snap.HAProxy.StatsEnabled = false // don't compare with the previous stats bind
			if owner := portOwner(app, snap, "", addr, port); owner != "" {
				e.Add("statsBind", "Port %d is already used by %s", port, owner)
			}
		}
	}
	if s.StatsAccessList != "" {
		found := false
		for _, al := range snap.AccessLists {
			if al.ID == s.StatsAccessList {
				found = true
			}
		}
		if !found {
			e.Add("statsAccessListId", "Access list no longer exists")
		}
	}
	return e.Err()
}

// ---------------------------------------------------------------- dependencies

func frontendUsesBackend(f *model.Frontend, id string) bool {
	if f.DefaultBackendID == id {
		return true
	}
	for _, r := range f.Rules {
		if r.BackendID == id {
			return true
		}
	}
	return false
}

func backendDependents(snap *model.Snapshot, id string) []string {
	var names []string
	for i := range snap.Frontends {
		if frontendUsesBackend(&snap.Frontends[i], id) {
			names = append(names, "frontend "+snap.Frontends[i].Name)
		}
	}
	for _, h := range snap.Hosts {
		uses := h.Upstream.BackendID == id
		for _, l := range h.Locations {
			if l.Upstream.BackendID == id {
				uses = true
			}
		}
		if uses {
			names = append(names, firstDomain(h.Domains))
		}
	}
	for _, st := range snap.Streams {
		if st.BackendID == id {
			names = append(names, "stream "+st.Name)
		}
	}
	return names
}

// frontendDependents lists proxy hosts whose upstream is this frontend.
func frontendDependents(snap *model.Snapshot, f *model.Frontend) []string {
	addr, port, err := model.SplitBind(f.Bind)
	if err != nil {
		return nil
	}
	targets := func(u model.Upstream) bool {
		if u.Port != port {
			return false
		}
		h := strings.Trim(u.Host, "[]")
		if h == "localhost" {
			h = "127.0.0.1"
		}
		ip := net.ParseIP(h)
		if ip == nil {
			return false
		}
		return addrOverlap(addr, ip.String()) || (ip.IsLoopback() && (addr == "" || net.ParseIP(addr).IsLoopback()))
	}
	var names []string
	for _, h := range snap.Hosts {
		uses := targets(h.Upstream)
		for _, l := range h.Locations {
			if l.Kind == model.LocationProxy && targets(l.Upstream) {
				uses = true
			}
		}
		if uses {
			names = append(names, firstDomain(h.Domains))
		}
	}
	return names
}

func firstDomain(d []string) string {
	if len(d) == 0 {
		return "(unnamed host)"
	}
	return d[0]
}

// ---------------------------------------------------------------- ports

func isWildcard(addr string) bool {
	return addr == "" || addr == "*" || addr == "0.0.0.0" || addr == "::" || addr == "[::]"
}

func addrOverlap(a, b string) bool {
	if isWildcard(a) || isWildcard(b) {
		return true
	}
	ia, ib := net.ParseIP(strings.Trim(a, "[]")), net.ParseIP(strings.Trim(b, "[]"))
	if ia != nil && ib != nil {
		return ia.Equal(ib)
	}
	return a == b
}

// portInSpec reports whether port is in a stream port spec ("25565", "2456-2458", "80,443").
func portInSpec(spec string, port int) bool {
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi := part, part
		if i := strings.Index(part, "-"); i > 0 {
			lo, hi = part[:i], part[i+1:]
		}
		l, err1 := strconv.Atoi(strings.TrimSpace(lo))
		h, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 == nil && err2 == nil && port >= l && port <= h {
			return true
		}
	}
	return false
}

// portOwner returns a description of whatever already binds addr:port
// (excluding frontend selfID), or "".
func portOwner(app *core.App, snap *model.Snapshot, selfID, addr string, port int) string {
	for _, f := range snap.Frontends {
		if f.ID == selfID || !f.Enabled {
			continue
		}
		if a, p, err := model.SplitBind(f.Bind); err == nil && p == port && addrOverlap(a, addr) {
			return "frontend " + f.Name
		}
	}
	if snap.HAProxy.StatsEnabled {
		if a, p, err := model.SplitBind(snap.HAProxy.StatsBind); err == nil && p == port && addrOverlap(a, addr) {
			return "the HAProxy stats endpoint"
		}
	}
	for _, st := range snap.Streams {
		if !st.Enabled || st.Protocol == "udp" {
			continue
		}
		if portInSpec(st.ListenPorts, port) && addrOverlap(st.ListenAddress, addr) {
			return "stream " + st.Name
		}
	}
	g := snap.General
	proxy := "nginx"
	if app != nil {
		proxy = core.ProxyEngineLabel(app.ProxyEngine(context.Background()))
	}
	if g.HTTPPort == port {
		return proxy + " (HTTP)"
	}
	if g.HTTPSPort == port {
		return proxy + " (HTTPS)"
	}
	if g.AdminPort == port {
		return "the Relay admin UI"
	}
	if app != nil {
		if _, p, err := net.SplitHostPort(app.Config.Listen); err == nil {
			if lp, _ := strconv.Atoi(p); lp == port {
				return "the Relay API"
			}
		}
	}
	return ""
}

// serverIndex finds a server by id.
func serverIndex(b *model.Backend, serverID string) int {
	for i := range b.Servers {
		if b.Servers[i].ID == serverID {
			return i
		}
	}
	return -1
}

// serverName resolves the haproxy name of the server at index i.
func serverName(b *model.Backend, i int) string { return haproxy.ServerName(b, i) }
