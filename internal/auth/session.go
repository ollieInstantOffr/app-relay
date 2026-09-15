package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// gate lists what a signed-in user must do before using the rest of the API.
type gate struct {
	MustEnroll2FA      bool
	MustChangePassword bool
}

type gateEntry struct {
	gate
	at time.Time
}

func (g gate) blocked() bool { return g.MustEnroll2FA || g.MustChangePassword }

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newSessionToken returns a random cookie value and its storage id (sha256).
func newSessionToken() (raw, id string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashToken(raw)
}

const sessionTokenLen = 43 // base64url(32 bytes)

func remoteIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isSecureRequest reports whether the browser reached Relay over HTTPS
// (directly, or through the local nginx which sets X-Forwarded-Proto).
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return remoteIsLoopback(r) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// requestHost is the host the browser used (X-Forwarded-Host is trusted from loopback only).
func requestHost(r *http.Request) string {
	if remoteIsLoopback(r) {
		if h := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); h != "" {
			return h
		}
	}
	return r.Host
}

func stripDefaultPort(scheme, host string) string {
	host = strings.ToLower(host)
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		return host[:strings.LastIndexByte(host, ':')]
	}
	return host
}

// sameOrigin rejects cross-site state-changing requests. Browsers always send
// Origin on cross-origin fetches and form posts; requests without Origin are
// non-browser clients or same-origin navigations.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	o := stripDefaultPort(u.Scheme, u.Host)
	for _, h := range []string{r.Host, requestHost(r)} {
		if o == stripDefaultPort(u.Scheme, h) {
			return true
		}
	}
	return false
}

func (s *Service) originOK(r *http.Request) bool {
	return s.app.Config.DevMode || sameOrigin(r)
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *Service) setSessionCookie(w http.ResponseWriter, r *http.Request, raw string, remember bool, expires time.Time) {
	c := &http.Cookie{
		Name:     CookieName,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
	}
	if remember {
		c.Expires = expires.UTC()
		c.MaxAge = int(time.Until(expires).Seconds())
		if c.MaxAge <= 0 {
			c.MaxAge = 1
		}
	}
	http.SetCookie(w, c)
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecureRequest(r), MaxAge: -1})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// createSession stores a new session for u and sets the cookie.
func (s *Service) createSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u *store.User, remember bool) (*store.AuthSession, error) {
	sec := s.security(ctx)
	now := s.now().UTC()
	raw, id := newSessionToken()
	pk, err := s.app.Store.CountWebAuthnCredentials(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	g := computeGate(u, sec, pk)
	sess := &store.AuthSession{
		ID: id, UserID: u.ID, Username: u.Username,
		CreatedAt: now, ExpiresAt: now.Add(sessionTTL(sec)), LastSeenAt: now,
		IP: core.ClientIP(r), UserAgent: truncate(r.UserAgent(), 512),
		MFAPending: g.MustEnroll2FA, Remember: remember,
	}
	if err := s.app.Store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	_ = s.app.Store.TouchUserActive(ctx, u.ID, now)
	s.gates.Store(id, gateEntry{g, now})
	s.setSessionCookie(w, r, raw, remember, sess.ExpiresAt)
	return sess, nil
}

// computeGate decides whether u must enrol 2FA or change the password first.
func computeGate(u *store.User, sec model.SecuritySettings, passkeys int) gate {
	return gate{
		MustChangePassword: u.MustChangePassword,
		MustEnroll2FA:      u.Role == core.RoleAdmin && sec.Require2FAForAdmins && !u.TOTPEnabled && passkeys == 0,
	}
}

func userActor(u *store.User, ip, sessionID string) core.Actor {
	return core.Actor{Type: core.ActorUser, ID: u.ID, Name: u.Username, Role: u.Role, IP: ip, SessionID: sessionID}
}

// Authenticate resolves the session cookie, then an `Authorization: Bearer
// rl_api_…` REST token.
func (s *Service) Authenticate(r *http.Request) (core.Actor, error) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		if a, err := s.authenticateSession(r, c.Value); err == nil {
			return a, nil
		}
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		raw := strings.TrimSpace(h[7:])
		if strings.HasPrefix(raw, PrefixAPI) {
			return s.AuthenticateToken(r.Context(), raw, SurfaceREST)
		}
	}
	return core.Actor{}, core.ErrUnauthenticated
}

func (s *Service) authenticateSession(r *http.Request, raw string) (core.Actor, error) {
	if len(raw) != sessionTokenLen {
		return core.Actor{}, core.ErrUnauthenticated
	}
	// Cookies ride along on cross-site requests in some cases (same-site
	// subdomains); never let them authorise a cross-origin write.
	if !isSafeMethod(r.Method) && !s.originOK(r) {
		return core.Actor{}, core.ErrUnauthenticated
	}
	ctx := r.Context()
	id := hashToken(raw)
	sess, err := s.app.Store.GetSession(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.app.Log.Warn("auth: load session", "err", err)
		}
		return core.Actor{}, core.ErrUnauthenticated
	}
	sec := s.security(ctx)
	now := s.now().UTC()
	if sessionExpired(sess, sec, now) {
		_ = s.app.Store.DeleteSession(context.WithoutCancel(ctx), id)
		s.gates.Delete(id)
		return core.Actor{}, core.ErrUnauthenticated
	}
	u, err := s.app.Store.GetUser(ctx, sess.UserID)
	if err != nil || u.Disabled {
		return core.Actor{}, core.ErrUnauthenticated
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval {
		bg := context.WithoutCancel(ctx)
		_ = s.app.Store.TouchSession(bg, id, now, now.Add(sessionTTL(sec)))
		_ = s.app.Store.TouchUserActive(bg, u.ID, now)
	}
	pk := 0
	if u.Role == core.RoleAdmin && sec.Require2FAForAdmins && !u.TOTPEnabled {
		if pk, err = s.app.Store.CountWebAuthnCredentials(ctx, u.ID); err != nil {
			return core.Actor{}, err
		}
	}
	s.gates.Store(id, gateEntry{computeGate(u, sec, pk), now})
	return userActor(u, core.ClientIP(r), id), nil
}

// sessionExpired: past its sliding expiry, or idle longer than the current TTL
// (the TTL may have been shortened since the session was last touched).
func sessionExpired(sess *store.AuthSession, sec model.SecuritySettings, now time.Time) bool {
	return !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) > sessionTTL(sec)
}

// gateFor returns the gate computed for a session during this request.
func (s *Service) gateFor(sessionID string) gate {
	if v, ok := s.gates.Load(sessionID); ok {
		return v.(gateEntry).gate
	}
	return gate{}
}

// revokeUserSessions deletes a user's sessions (except keepID) and forgets their gates.
func (s *Service) revokeUserSessions(ctx context.Context, userID, keepID string) int64 {
	sessions, _ := s.app.Store.ListSessions(ctx, userID, time.Time{})
	n, err := s.app.Store.DeleteUserSessions(ctx, userID, keepID)
	if err != nil {
		s.app.Log.Warn("auth: revoke sessions", "user", userID, "err", err)
	}
	for _, x := range sessions {
		if x.ID != keepID {
			s.gates.Delete(x.ID)
		}
	}
	return n
}
