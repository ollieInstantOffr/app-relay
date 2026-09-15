package auth

import (
	"context"
	"net/http"
	"net/netip"
	"strings"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

// The API server installs httpx.NetworkGuard when it builds its router, which
// happens before slice routes are registered — so the guard is set at package
// initialisation rather than from PublicRoutes.
func init() { httpx.NetworkGuard = networkGuard }

// networkGuard enforces, for the UI, the API and /mcp:
//   - security.adminAccessListId: clients whose address the access list does
//     not allow get 403 (loopback is always allowed, so a shell on the host can
//     recover). Rules follow nginx semantics: first match wins, no match allows.
//   - account gates: sessions that must change their password or enrol 2FA may
//     only call /api/auth/* (and /api/setup/*) until they do.
func networkGuard(app *core.App, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := serviceOf(app)
		// Relay login is served to app visitors through the proxy engine.
		if s == nil || strings.HasPrefix(r.URL.Path, render.PortalPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		ip := core.ClientIP(r)
		if !s.networkAllowed(r.Context(), ip) {
			if isAPIPath(r.URL.Path) {
				httpx.WriteError(w, http.StatusForbidden, "network_restricted", "Relay's admin interface isn't reachable from "+ip)
			} else {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("403 Forbidden\n\nRelay's admin interface is restricted to allowed networks.\nYour address: " + ip + "\n"))
			}
			return
		}
		if a := core.ActorFrom(r.Context()); a.Type == core.ActorUser && a.SessionID != "" &&
			strings.HasPrefix(r.URL.Path, "/api/") && !gateExempt(r.URL.Path) {
			g := s.gateFor(a.SessionID)
			switch {
			case g.MustChangePassword:
				httpx.WriteError(w, http.StatusForbidden, "password_change_required", "Choose a new password to continue")
				return
			case g.MustEnroll2FA:
				httpx.WriteError(w, http.StatusForbidden, "mfa_enrollment_required", "Set up two-factor authentication to continue")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") || p == "/mcp" || strings.HasPrefix(p, "/mcp/")
}

func gateExempt(p string) bool {
	return strings.HasPrefix(p, "/api/auth/") || strings.HasPrefix(p, "/api/setup/") || p == "/api/health"
}

// networkAllowed reports whether ip may reach the admin interface.
func (s *Service) networkAllowed(ctx context.Context, ip string) bool {
	sec := s.security(ctx)
	if sec.AdminAccessListID == "" {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() {
		return true
	}
	e := s.accessList(ctx, sec.AdminAccessListID)
	if !e.found {
		// A dangling reference must not lock everyone out; the security
		// settings hook refuses to save one, so this only follows a deletion.
		return true
	}
	return ipAllowedByRules(e.rules, addr)
}

func (s *Service) accessList(ctx context.Context, id string) aclEntry {
	now := s.now()
	s.mu.Lock()
	e, ok := s.acls[id]
	s.mu.Unlock()
	if ok && now.Sub(e.at) < aclCacheTTL {
		return e
	}
	al, err := s.app.Store.AccessLists().Get(ctx, id)
	switch {
	case err == nil:
		e = aclEntry{rules: al.Rules, name: al.Name, found: true, at: now}
	case err == store.ErrNotFound:
		s.app.Log.Warn("auth: admin access list not found; admin UI is unrestricted", "id", id)
		e = aclEntry{found: false, at: now}
	default:
		s.app.Log.Warn("auth: load admin access list", "err", err)
		if ok {
			return e // keep the previous rules on transient errors
		}
		e = aclEntry{found: true, at: now} // no rules → nginx semantics allow; avoids lockout on DB errors
	}
	s.mu.Lock()
	s.acls[id] = e
	s.mu.Unlock()
	return e
}

// ipAllowedByRules evaluates access-list IP rules like nginx allow/deny:
// the first rule whose CIDR (or "all") contains addr decides; when no rule
// matches, access is allowed.
func ipAllowedByRules(rules []model.IPRule, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, rule := range rules {
		p, all, err := model.ParseAccessRule(strings.TrimSpace(rule.CIDR))
		if err != nil {
			continue
		}
		if all || p.Contains(addr) {
			return rule.Action == "allow"
		}
	}
	return true
}
