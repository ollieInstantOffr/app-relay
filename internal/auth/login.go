package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// sessionUserDTO extends web/src/lib/types.ts SessionUser with
// passkeyCount, mustEnroll2fa and sessionExpiresAt.
type sessionUserDTO struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	Email              string    `json:"email"`
	Role               string    `json:"role"`
	TOTPEnabled        bool      `json:"totpEnabled"`
	PasskeyCount       int       `json:"passkeyCount"`
	MustChangePassword bool      `json:"mustChangePassword"`
	MustEnroll2FA      bool      `json:"mustEnroll2fa"`
	SessionExpiresAt   time.Time `json:"sessionExpiresAt"`
}

// sessionDTO is GET /api/auth/session. Extra fields beyond the shared
// contract: setupDone (authenticated only) and sessionTtlHours.
type sessionDTO struct {
	Authenticated   bool            `json:"authenticated"`
	SetupRequired   bool            `json:"setupRequired"`
	SetupDone       *bool           `json:"setupDone,omitempty"`
	User            *sessionUserDTO `json:"user,omitempty"`
	Version         string          `json:"version"`
	InstanceName    string          `json:"instanceName"`
	SessionTTLHours int             `json:"sessionTtlHours"`
}

func (s *Service) handleSession(w http.ResponseWriter, r *http.Request) {
	s.writeSession(w, r, http.StatusOK)
}

func (s *Service) writeSession(w http.ResponseWriter, r *http.Request, status int) {
	ctx := r.Context()
	n, err := s.app.Store.CountUsers(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	g := s.general(ctx)
	sec := s.security(ctx)
	out := sessionDTO{SetupRequired: n == 0, Version: s.app.Config.Version, InstanceName: g.InstanceName, SessionTTLHours: sec.SessionTTLHours}
	if a := core.ActorFrom(ctx); a.Type == core.ActorUser && a.SessionID != "" {
		u, err1 := s.app.Store.GetUser(ctx, a.ID)
		sess, err2 := s.app.Store.GetSession(ctx, a.SessionID)
		if err1 == nil && err2 == nil && !u.Disabled {
			pk, _ := s.app.Store.CountWebAuthnCredentials(ctx, u.ID)
			gt := computeGate(u, sec, pk)
			done := g.SetupDone
			out.Authenticated = true
			out.SetupDone = &done
			out.User = &sessionUserDTO{
				ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role, TOTPEnabled: u.TOTPEnabled, PasskeyCount: pk,
				MustChangePassword: gt.MustChangePassword, MustEnroll2FA: gt.MustEnroll2FA, SessionExpiresAt: sess.ExpiresAt,
			}
			// Persistent cookies slide with the server-side expiry.
			if c, err := r.Cookie(CookieName); err == nil && sess.Remember && hashToken(c.Value) == sess.ID {
				s.setSessionCookie(w, r, c.Value, true, sess.LastSeenAt.Add(sessionTTL(sec)))
			}
		}
	}
	httpx.WriteJSON(w, status, out)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
	Remember bool   `json:"remember"`
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		httpx.WriteError(w, http.StatusForbidden, "cross_origin", "cross-origin sign-in refused")
		return
	}
	var req loginRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	n, err := s.app.Store.CountUsers(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if n == 0 {
		httpx.WriteError(w, http.StatusConflict, "setup_required", "Relay has no accounts yet — finish setup first")
		return
	}
	uname := normUsername(req.Username)
	if uname == "" || req.Password == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing_credentials", "Enter your username and password")
		return
	}
	ip := core.ClientIP(r)
	if s.throttled(ctx, ip, uname) {
		s.auditBlocked(ctx, ip, uname)
		s.writeThrottled(ctx, w)
		return
	}
	u, err := s.app.Store.GetUserByUsername(ctx, uname)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, err)
		return
	}
	if u == nil {
		burnPasswordCheck(req.Password)
		s.loginFailed(ctx, ip, uname, nil, "unknown user")
		s.writeInvalidCredentials(ctx, w, ip, uname)
		return
	}
	if !checkPassword(u.PasswordHash, req.Password) {
		s.loginFailed(ctx, ip, uname, u, "wrong password")
		s.writeInvalidCredentials(ctx, w, ip, uname)
		return
	}
	if u.Disabled {
		s.loginFailed(ctx, ip, uname, u, "account disabled")
		httpx.WriteError(w, http.StatusForbidden, "account_disabled", "This account is disabled. Ask an admin to re-enable it.")
		return
	}
	if u.Role == core.RoleMember {
		httpx.WriteError(w, http.StatusForbidden, "app_access_only", appAccessOnlyMessage)
		return
	}
	method := "password"
	if u.TOTPEnabled {
		code := strings.TrimSpace(req.TOTP)
		if code == "" {
			httpx.WriteError(w, http.StatusUnauthorized, "totp_required", "Enter the 6-digit code from your authenticator app")
			return
		}
		if !s.verifyTOTP(u.ID, u.TOTPSecret, code) {
			s.loginFailed(ctx, ip, uname, u, "wrong 2FA code")
			if s.throttled(ctx, ip, uname) {
				s.writeThrottled(ctx, w)
				return
			}
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_totp", "That code didn't work — check the time on your authenticator device")
			return
		}
		method = "totp"
	} else if pk, err := s.app.Store.CountWebAuthnCredentials(ctx, u.ID); err != nil {
		httpx.Fail(w, r, err)
		return
	} else if pk > 0 {
		// Passkeys are this account's second factor: a password alone isn't enough.
		httpx.WriteError(w, http.StatusUnauthorized, "passkey_required", "This account signs in with a passkey — use “Use a passkey”")
		return
	}
	s.completeLogin(w, r, u, method, req.Remember)
}

func (s *Service) writeInvalidCredentials(ctx context.Context, w http.ResponseWriter, ip, uname string) {
	if s.throttled(ctx, ip, uname) {
		s.writeThrottled(ctx, w)
		return
	}
	httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong username or password")
}

func loginDetail(ip, method string) string {
	switch method {
	case "password":
		return ip + " · password"
	case "setup":
		return ip + " · first-run setup"
	}
	return ip + " · 2FA " + method
}

// completeLogin creates the session, audits, notifies about new addresses and
// writes the Session response.
func (s *Service) completeLogin(w http.ResponseWriter, r *http.Request, u *store.User, method string, remember bool) {
	ctx := r.Context()
	ip := core.ClientIP(r)
	uname := strings.ToLower(u.Username)
	hadAny, fromIP, err := s.app.Store.LoginHistory(ctx, uname, ip)
	if err != nil {
		s.app.Log.Warn("auth: login history", "err", err)
	}
	sess, err := s.createSession(ctx, w, r, u, remember)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := s.app.Store.RecordLoginAttempt(ctx, s.now(), ip, uname, true); err != nil {
		s.app.Log.Warn("auth: record login", "err", err)
	}
	actor := userActor(u, ip, sess.ID)
	s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "auth.login", Target: u.Username, Detail: loginDetail(ip, method)})
	if hadAny && !fromIP && s.app.Notify != nil {
		label := "password"
		if method != "password" {
			label = "2FA " + method
		}
		s.app.Notify.Notify(context.WithoutCancel(ctx), core.Notification{
			Event: model.EventUnknownSignIn, Level: "warn",
			Title:   "Sign-in from a new address",
			Message: fmt.Sprintf("%s signed in to Relay from %s (%s). If this wasn't them, reset the password and revoke sessions.", u.Username, ip, label),
			URL:     "/settings/users",
		})
	}
	s.writeSession(w, r.WithContext(core.WithActor(ctx, actor)), http.StatusOK)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		httpx.WriteError(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
		return
	}
	ctx := r.Context()
	if c, err := r.Cookie(CookieName); err == nil && len(c.Value) == sessionTokenLen {
		id := hashToken(c.Value)
		if sess, err := s.app.Store.GetSession(ctx, id); err == nil {
			_ = s.app.Store.DeleteSession(ctx, id)
			s.gates.Delete(id)
			actor := core.Actor{Type: core.ActorUser, ID: sess.UserID, Name: sess.Username, IP: core.ClientIP(r)}
			s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "auth.logout", Target: sess.Username, Detail: actor.IP})
		}
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// requireUser wraps handlers for the signed-in user's own account. These
// routes live in PublicRoutes so viewers (blocked from writes in the
// protected group) can still manage their password, 2FA and sessions.
func (s *Service) requireUser(h func(w http.ResponseWriter, r *http.Request, u *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := httpx.Actor(r)
		if a.Type != core.ActorUser || a.ID == "" {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		u, err := s.app.Store.GetUser(r.Context(), a.ID)
		if err != nil || u.Disabled {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		h(w, r, u)
	}
}

type changePasswordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Service) handleChangePassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req changePasswordRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	ip := core.ClientIP(r)
	uname := strings.ToLower(u.Username)
	if s.throttled(ctx, ip, uname) {
		s.writeThrottled(ctx, w)
		return
	}
	if !checkPassword(u.PasswordHash, req.Current) {
		s.loginFailed(ctx, ip, uname, u, "wrong current password (password change)")
		httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"current": "Current password is wrong"}})
		return
	}
	e := model.Errs{}
	if msg := passwordError(req.New, u.Username); msg != "" {
		e.Add("new", "%s", msg)
	} else if req.New == req.Current {
		e.Add("new", "Choose a different password")
	}
	if err := e.Err(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hash, err := HashPassword(req.New)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u.PasswordHash = hash
	u.MustChangePassword = false
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	a := httpx.Actor(r)
	n := s.revokeUserSessions(ctx, u.ID, a.SessionID)
	s.gates.Delete(a.SessionID)
	s.app.Audit(ctx, core.AuditEntry{Action: "auth.password_changed", Target: u.Username, Detail: fmt.Sprintf("%d other session(s) signed out", n)})
	s.writeSession(w, r, http.StatusOK)
}

type requestAccessRequest struct {
	What string `json:"what"`
	Path string `json:"path"`
}

// handleRequestAccess lets a viewer ask admins for more access (audit entry).
func (s *Service) handleRequestAccess(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req requestAccessRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	now := s.now()
	if v, ok := s.requests.Load(u.ID); ok && now.Sub(v.(time.Time)) < 10*time.Minute {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"requested": true, "duplicate": true})
		return
	}
	s.requests.Store(u.ID, now)
	what := displayName(truncate(req.What, 120))
	detail := u.Username + " (" + u.Role + ") asked for access"
	if p := strings.TrimSpace(req.Path); p != "" {
		detail += " · " + truncate(p, 200)
	}
	s.app.Audit(r.Context(), core.AuditEntry{Action: "auth.request_access", Target: what, Detail: detail, Result: "pending"})
	s.app.Activity(r.Context(), "auth.request_access", "info", u.Username+" requested access", what, detail)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"requested": true})
}

const appAccessOnlyMessage = "This account can only sign in to apps protected by Relay login, not to the Relay admin UI."
