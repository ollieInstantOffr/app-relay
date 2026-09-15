package acme

// Streams: port clash detection and the port usage overview.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// PortEntry is one bound (or configured) port.
type PortEntry struct {
	Port      int    `json:"port"`
	Proto     string `json:"proto"`   // tcp | udp
	Address   string `json:"address"` // 0.0.0.0, 127.0.0.1 …
	Owner     string `json:"owner"`   // nginx | edge (Relay Edge) | haproxy | relay (admin UI) | other
	Kind      string `json:"kind"`    // http | https | stream | frontend | admin | stats | ""
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"` // stream / frontend id
	Enabled   bool   `json:"enabled"`
	Listening *bool  `json:"listening"` // nil when the host listeners are unknown
}

func protoList(p string) []string {
	if p == "both" {
		return []string{"tcp", "udp"}
	}
	return []string{p}
}

// addrsOverlap reports whether two listen addresses can clash. nginx binds
// IPv6 wildcards with ipv6only=on, so families never clash with each other.
func addrsOverlap(a, b string) bool {
	pa, errA := netip.ParseAddr(normAddr(a))
	pb, errB := netip.ParseAddr(normAddr(b))
	if errA != nil || errB != nil {
		return a == b
	}
	pa, pb = pa.Unmap(), pb.Unmap()
	if pa.Is4() != pb.Is4() {
		return false
	}
	return pa == pb || pa.IsUnspecified() || pb.IsUnspecified()
}

func normAddr(a string) string {
	a = strings.Trim(a, "[]")
	switch a {
	case "", "*":
		return "0.0.0.0"
	}
	return a
}

// splitBind parses "127.0.0.1:10080", ":8080", "*:80" or "[::]:443".
func splitBind(bind string) (string, int, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(bind))
	if err != nil {
		return "", 0, false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", 0, false
	}
	return normAddr(host), p, true
}

func streamEntries(s *model.Stream) []PortEntry {
	lo, hi, err := model.ParseStreamPorts(s.ListenPorts)
	if err != nil {
		return nil
	}
	var out []PortEntry
	for port := lo; port <= hi; port++ {
		for _, proto := range protoList(s.Protocol) {
			out = append(out, PortEntry{Port: port, Proto: proto, Address: normAddr(s.ListenAddress), Owner: "nginx", Kind: "stream", Name: s.Name, ID: s.ID, Enabled: s.Enabled})
		}
	}
	return out
}

// configPorts lists every port Relay's desired configuration binds.
func (h *handlers) configPorts(ctx context.Context) ([]PortEntry, error) {
	g, err := store.LoadSettings[model.GeneralSettings](ctx, h.app.Store, model.SettingsGeneral)
	if err != nil {
		return nil, err
	}
	out := []PortEntry{
		{Port: g.HTTPPort, Proto: "tcp", Address: "0.0.0.0", Owner: "nginx", Kind: "http", Name: "HTTP", Enabled: true},
		{Port: g.HTTPSPort, Proto: "tcp", Address: "0.0.0.0", Owner: "nginx", Kind: "https", Name: "HTTPS", Enabled: true},
	}
	if g.HTTP3 {
		out = append(out, PortEntry{Port: g.HTTPSPort, Proto: "udp", Address: "0.0.0.0", Owner: "nginx", Kind: "https", Name: "HTTP/3 (QUIC)", Enabled: true})
	}
	adminAddr, adminPort := "0.0.0.0", g.AdminPort
	if host, _, ok := splitBind(h.app.Config.Listen); ok {
		adminAddr = host
	}
	if h.app.AdminListener != nil {
		if ports := h.app.AdminListener.Ports(); len(ports) > 0 {
			adminPort = ports[0]
		}
	}
	if adminPort > 0 {
		out = append(out, PortEntry{Port: adminPort, Proto: "tcp", Address: adminAddr, Owner: "relay", Kind: "admin", Name: "Relay admin UI", Enabled: true})
	}
	streams, err := h.app.Store.Streams().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range streams {
		out = append(out, streamEntries(&streams[i])...)
	}
	frontends, err := h.app.Store.Frontends().List(ctx)
	if err != nil {
		return nil, err
	}
	// Frontends and the stats listener belong to the active load balancer
	// engine: haproxy | balancer.
	lbOwner := h.app.LBEngine(ctx)
	for _, f := range frontends {
		if host, port, ok := splitBind(f.Bind); ok {
			out = append(out, PortEntry{Port: port, Proto: "tcp", Address: host, Owner: lbOwner, Kind: "frontend", Name: f.Name, ID: f.ID, Enabled: f.Enabled})
		}
	}
	hp, err := store.LoadSettings[model.HAProxySettings](ctx, h.app.Store, model.SettingsHAProxy)
	if err == nil && hp.StatsEnabled {
		if host, port, ok := splitBind(hp.StatsBind); ok {
			out = append(out, PortEntry{Port: port, Proto: "tcp", Address: host, Owner: lbOwner, Kind: "stats", Name: core.ProxyEngineLabel(lbOwner) + " stats", Enabled: len(frontends) > 0 || hasBackends(ctx, h.app.Store)})
		}
	}
	// Proxy ports and streams belong to whichever proxy engine is active.
	if engine := h.app.ProxyEngine(ctx); engine != "nginx" {
		for i := range out {
			if out[i].Owner == "nginx" {
				out[i].Owner = engine
			}
		}
	}
	return out, nil
}

func hasBackends(ctx context.Context, st *store.Store) bool {
	b, err := st.Backends().List(ctx)
	return err == nil && len(b) > 0
}

// withListeners marks configured entries as listening and appends host
// sockets that no configured entry explains (owner "other").
func (h *handlers) withListeners(ctx context.Context, entries []PortEntry) ([]PortEntry, bool) {
	pc, _ := h.app.Proxy(ctx)
	if pc == nil {
		return entries, false
	}
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := pc.Listeners(lctx)
	if err != nil || resp == nil {
		return entries, false
	}
	f, t := false, true
	for i := range entries {
		entries[i].Listening = &f
	}
	seen := map[string]bool{}
	for _, l := range resp.Listeners {
		proto := strings.ToLower(l.Proto)
		proto = strings.TrimSuffix(strings.TrimSuffix(proto, "6"), "4")
		matched := false
		for i := range entries {
			e := &entries[i]
			if e.Port == l.Port && e.Proto == proto && addrsOverlap(e.Address, l.Address) {
				e.Listening = &t
				matched = true
			}
		}
		key := fmt.Sprintf("%s/%d/%s", proto, l.Port, normAddr(l.Address))
		if matched || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, PortEntry{Port: l.Port, Proto: proto, Address: normAddr(l.Address), Owner: "other", Name: l.Process, Enabled: true, Listening: &t})
	}
	return entries, true
}

// portConflicts returns entries that clash with a candidate listen spec.
func portConflicts(entries []PortEntry, address string, lo, hi int, protocol, excludeID string) []PortEntry {
	protos := protoList(protocol)
	conflicts := []PortEntry{}
	seen := map[string]bool{}
	for _, e := range entries {
		if excludeID != "" && e.ID == excludeID && e.Kind == "stream" {
			continue
		}
		if e.Port < lo || e.Port > hi || !addrsOverlap(address, e.Address) {
			continue
		}
		for _, p := range protos {
			if p != e.Proto {
				continue
			}
			key := fmt.Sprintf("%s/%d/%s/%s", e.Proto, e.Port, e.Owner, e.Name)
			if !seen[key] {
				seen[key] = true
				conflicts = append(conflicts, e)
			}
		}
	}
	sort.SliceStable(conflicts, func(i, j int) bool { return conflicts[i].Port < conflicts[j].Port })
	return conflicts
}

func describeConflict(e PortEntry) string {
	who := e.Name
	switch e.Kind {
	case "stream":
		who = "stream " + e.Name
	case "frontend":
		who = "HAProxy frontend " + e.Name
		if e.Owner == "balancer" {
			who = "Relay Balancer frontend " + e.Name
		}
	case "http", "https":
		who = "nginx " + e.Name
	case "":
		who = firstNonEmpty(e.Name, "another process") + " on this machine"
	}
	return fmt.Sprintf("Port %d/%s is used by %s", e.Port, e.Proto, who)
}

func (h *handlers) registerStreamHooks() {
	httpx.StreamHooks.BeforeSave = func(r *http.Request, prev, next *model.Stream) error {
		ctx := r.Context()
		if prev == nil && next.IdleTimeout == "" {
			next.IdleTimeout = "10m"
		}
		if next.BackendID != "" {
			if err := h.checkStreamBackend(ctx, next.BackendID); err != nil {
				return err
			}
		}
		lo, hi, err := model.ParseStreamPorts(next.ListenPorts)
		if err != nil || net.ParseIP(strings.TrimSpace(next.ListenAddress)) == nil {
			return nil // Validate reports field errors
		}
		entries, err := h.configPorts(ctx)
		if err != nil {
			return err
		}
		if c := portConflicts(entries, next.ListenAddress, lo, hi, next.Protocol, next.ID); len(c) > 0 {
			msg := describeConflict(c[0])
			if len(c) > 1 {
				msg += fmt.Sprintf(" (+%d more)", len(c)-1)
			}
			return (model.Errs{"listenPorts": msg}).Err()
		}
		return nil
	}
}

func (h *handlers) checkStreamBackend(ctx context.Context, backendID string) error {
	b, err := h.app.Store.Backends().Get(ctx, backendID)
	if err != nil {
		return (model.Errs{"backendId": "This backend no longer exists"}).Err()
	}
	frontends, err := h.app.Store.Frontends().List(ctx)
	if err != nil {
		return err
	}
	for _, f := range frontends {
		host, _, ok := splitBind(f.Bind)
		if ok && f.Enabled && f.Mode == "tcp" && f.DefaultBackendID == backendID && host == "127.0.0.1" {
			return nil
		}
	}
	return (model.Errs{"backendId": "Backend " + b.Name + " has no enabled TCP frontend on 127.0.0.1 — add one in Load balancer → Frontends"}).Err()
}

func (h *handlers) streamRoutes(r chi.Router) {
	r.Get("/ports", func(w http.ResponseWriter, r *http.Request) {
		entries, err := h.configPorts(r.Context())
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		entries, _ = h.withListeners(r.Context(), entries)
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].Port != entries[j].Port {
				return entries[i].Port < entries[j].Port
			}
			return entries[i].Proto < entries[j].Proto
		})
		httpx.WriteJSON(w, http.StatusOK, entries)
	})
	r.Post("/streams/check-ports", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ListenAddress string `json:"listenAddress"`
			ListenPorts   string `json:"listenPorts"`
			Protocol      string `json:"protocol"`
			ExcludeID     string `json:"excludeId"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		lo, hi, err := model.ParseStreamPorts(body.ListenPorts)
		if err != nil {
			httpx.Fail(w, r, (model.Errs{"listenPorts": err.Error()}).Err())
			return
		}
		if body.ListenAddress == "" {
			body.ListenAddress = "0.0.0.0"
		}
		if net.ParseIP(body.ListenAddress) == nil {
			httpx.Fail(w, r, (model.Errs{"listenAddress": "Not a valid IP address"}).Err())
			return
		}
		if body.Protocol == "" {
			body.Protocol = "tcp"
		}
		entries, err := h.configPorts(r.Context())
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		entries, known := h.withListeners(r.Context(), entries)
		conflicts := portConflicts(entries, body.ListenAddress, lo, hi, body.Protocol, body.ExcludeID)
		messages := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			messages = append(messages, describeConflict(c))
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"free":           len(conflicts) == 0,
			"conflicts":      conflicts,
			"messages":       messages,
			"ports":          hi - lo + 1,
			"listenersKnown": known,
		})
	})
}
