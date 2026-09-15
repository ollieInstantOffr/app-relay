package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/store"
)

// ipFailureMultiplier: an address is throttled after max×5 failures across
// all usernames (password spraying), or max failures for one username.
const ipFailureMultiplier = 5

type throttleCounts struct {
	Pair int // failures for (ip, username) since that pair's last success
	IP   int // failures from ip for any username
}

func isThrottled(c throttleCounts, maxAttempts int) bool {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return c.Pair >= maxAttempts || c.IP >= maxAttempts*ipFailureMultiplier
}

// normUsername canonicalises a submitted username for throttling and lookup.
func normUsername(u string) string {
	return strings.ToLower(truncate(strings.TrimSpace(u), 64))
}

// displayName makes attacker-supplied usernames safe for the audit log.
func displayName(u string) string {
	u = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(u))
	if r := []rune(u); len(r) > 64 {
		u = string(r[:64])
	}
	if u == "" {
		return "(empty)"
	}
	return u
}

func (s *Service) failureCounts(ctx context.Context, ip, uname string) (throttleCounts, error) {
	sec := s.security(ctx)
	pair, byIP, err := s.app.Store.LoginFailures(ctx, ip, uname, s.now().Add(-lockout(sec)))
	return throttleCounts{Pair: pair, IP: byIP}, err
}

// throttled reports whether sign-in for (ip, uname) is currently blocked.
func (s *Service) throttled(ctx context.Context, ip, uname string) bool {
	c, err := s.failureCounts(ctx, ip, uname)
	if err != nil {
		s.app.Log.Warn("auth: count login failures", "err", err)
		return false
	}
	return isThrottled(c, s.security(ctx).LoginMaxAttempts)
}

func (s *Service) writeThrottled(ctx context.Context, w http.ResponseWriter) {
	sec := s.security(ctx)
	w.Header().Set("Retry-After", strconv.Itoa(sec.LoginLockoutMinutes*60))
	httpx.WriteError(w, http.StatusTooManyRequests, "throttled",
		fmt.Sprintf("Too many failed sign-ins from this address · IP throttled %d min", sec.LoginLockoutMinutes))
}

// loginFailed records a failed attempt and audits it ("… · wrong password ×3 · IP throttled 15 min").
func (s *Service) loginFailed(ctx context.Context, ip, uname string, u *store.User, reason string) {
	ctx = context.WithoutCancel(ctx)
	now := s.now()
	if err := s.app.Store.RecordLoginAttempt(ctx, now, ip, uname, false); err != nil {
		s.app.Log.Warn("auth: record login attempt", "err", err)
	}
	sec := s.security(ctx)
	c, _ := s.failureCounts(ctx, ip, uname)
	detail := ip + " · " + reason
	if c.Pair > 1 {
		detail += fmt.Sprintf(" ×%d", c.Pair)
	}
	result := "failed"
	if isThrottled(c, sec.LoginMaxAttempts) {
		result = "blocked"
		detail += fmt.Sprintf(" · IP throttled %d min", sec.LoginLockoutMinutes)
		s.notices.Store(ip+"\x00"+uname, now)
	}
	actor := core.Actor{Type: core.ActorUser, Name: displayName(uname), IP: ip}
	if u != nil {
		actor = userActor(u, ip, "")
	}
	s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "auth.login_failed", Target: actor.Name, Detail: detail, Result: result})
}

// auditBlocked audits a refused attempt while throttled, at most once per lockout window.
func (s *Service) auditBlocked(ctx context.Context, ip, uname string) {
	sec := s.security(ctx)
	key := ip + "\x00" + uname
	now := s.now()
	if v, ok := s.notices.Load(key); ok && now.Sub(v.(time.Time)) < lockout(sec) {
		return
	}
	s.notices.Store(key, now)
	actor := core.Actor{Type: core.ActorUser, Name: displayName(uname), IP: ip}
	s.app.Audit(context.WithoutCancel(ctx), core.AuditEntry{Actor: &actor, Action: "auth.login_failed", Target: actor.Name,
		Detail: fmt.Sprintf("%s · sign-in refused · IP throttled %d min", ip, sec.LoginLockoutMinutes), Result: "blocked"})
}
