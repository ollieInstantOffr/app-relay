package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func itoa(i int) string { return strconv.Itoa(i) }

type userDTO struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	Email              string     `json:"email"`
	Role               string     `json:"role"`
	TOTPEnabled        bool       `json:"totpEnabled"`
	Passkeys           int        `json:"passkeys"`
	TwoFactor          bool       `json:"twoFactor"`
	Disabled           bool       `json:"disabled"`
	MustChangePassword bool       `json:"mustChangePassword"`
	CreatedAt          time.Time  `json:"createdAt"`
	LastActiveAt       *time.Time `json:"lastActiveAt,omitempty"`
	Self               bool       `json:"self"`
}

func toUserDTO(u *store.User, passkeys int, selfID string) userDTO {
	return userDTO{
		ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role, TOTPEnabled: u.TOTPEnabled, Passkeys: passkeys,
		TwoFactor: u.TOTPEnabled || passkeys > 0, Disabled: u.Disabled, MustChangePassword: u.MustChangePassword,
		CreatedAt: u.CreatedAt, LastActiveAt: u.LastActiveAt, Self: u.ID == selfID,
	}
}

func validRole(r string) bool {
	return r == core.RoleAdmin || r == core.RoleEditor || r == core.RoleViewer
}

func emailError(e string) string {
	if e == "" {
		return ""
	}
	if len(e) > 254 {
		return "At most 254 characters"
	}
	if addr, err := mail.ParseAddress(e); err != nil || addr.Address != e {
		return "Enter a valid email address"
	}
	return ""
}

func (s *Service) handleUsersList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := s.app.Store.ListUsers(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	counts, err := s.app.Store.CountWebAuthnCredentialsByUser(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	self := httpx.Actor(r).ID
	out := make([]userDTO, 0, len(users))
	for i := range users {
		out = append(out, toUserDTO(&users[i], counts[users[i].ID], self))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Username           string `json:"username"`
	Email              string `json:"email"`
	Role               string `json:"role"`
	Password           string `json:"password"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

func (s *Service) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	e := model.Errs{}
	if msg := usernameError(req.Username); msg != "" {
		e.Add("username", "%s", msg)
	} else if _, err := s.app.Store.GetUserByUsername(ctx, req.Username); err == nil {
		e.Add("username", "That username is taken")
	} else if !errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, err)
		return
	}
	if msg := emailError(req.Email); msg != "" {
		e.Add("email", "%s", msg)
	}
	if !validRole(req.Role) {
		e.Add("role", "Pick admin, editor or viewer")
	}
	if msg := passwordError(req.Password, req.Username); msg != "" {
		e.Add("password", "%s", msg)
	}
	if err := e.Err(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u := &store.User{Username: req.Username, Email: req.Email, Role: req.Role, PasswordHash: hash, MustChangePassword: req.MustChangePassword}
	if err := s.app.Store.CreateUser(ctx, u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"username": "That username is taken"}})
			return
		}
		httpx.Fail(w, r, err)
		return
	}
	detail := "role " + u.Role
	if u.MustChangePassword {
		detail += " · must change password"
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "user.create", Target: u.Username, Detail: detail})
	httpx.WriteJSON(w, http.StatusCreated, toUserDTO(u, 0, httpx.Actor(r).ID))
}

type updateUserRequest struct {
	Email    *string `json:"email"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
}

func (s *Service) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateUserRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	a := httpx.Actor(r)
	u, err := s.app.Store.GetUser(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	next := *u
	e := model.Errs{}
	if req.Email != nil {
		next.Email = strings.TrimSpace(*req.Email)
		if msg := emailError(next.Email); msg != "" {
			e.Add("email", "%s", msg)
		}
	}
	if req.Role != nil {
		next.Role = *req.Role
		if !validRole(next.Role) {
			e.Add("role", "Pick admin, editor or viewer")
		}
	}
	if req.Disabled != nil {
		next.Disabled = *req.Disabled
		if next.Disabled && u.ID == a.ID {
			e.Add("disabled", "You can't disable your own account")
		}
	}
	if err := e.Err(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if u.Role == core.RoleAdmin && !u.Disabled && (next.Role != core.RoleAdmin || next.Disabled) {
		if n, err := s.app.Store.CountActiveAdmins(ctx); err != nil {
			httpx.Fail(w, r, err)
			return
		} else if n <= 1 {
			httpx.WriteError(w, http.StatusConflict, "last_admin", "Relay needs at least one active admin")
			return
		}
	}
	if err := s.app.Store.UpdateUser(ctx, &next); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var changes []string
	if next.Role != u.Role {
		changes = append(changes, "role "+u.Role+" → "+next.Role)
	}
	if next.Email != u.Email {
		changes = append(changes, "email updated")
	}
	if next.Disabled != u.Disabled {
		if next.Disabled {
			n := s.revokeUserSessions(ctx, u.ID, "")
			changes = append(changes, fmt.Sprintf("disabled · %d session(s) signed out", n))
		} else {
			changes = append(changes, "enabled")
		}
	}
	if len(changes) > 0 {
		s.app.Audit(ctx, core.AuditEntry{Action: "user.update", Target: u.Username, Detail: strings.Join(changes, " · ")})
	}
	pk, _ := s.app.Store.CountWebAuthnCredentials(ctx, u.ID)
	httpx.WriteJSON(w, http.StatusOK, toUserDTO(&next, pk, a.ID))
}

func (s *Service) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := httpx.Actor(r)
	u, err := s.app.Store.GetUser(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if u.ID == a.ID {
		httpx.WriteError(w, http.StatusConflict, "self", "You can't delete your own account")
		return
	}
	if u.Role == core.RoleAdmin && !u.Disabled {
		if n, err := s.app.Store.CountActiveAdmins(ctx); err != nil {
			httpx.Fail(w, r, err)
			return
		} else if n <= 1 {
			httpx.WriteError(w, http.StatusConflict, "last_admin", "Relay needs at least one active admin")
			return
		}
	}
	tokens, err := s.app.Store.RevokeUserAPITokens(ctx, u.ID, s.now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.revokeUserSessions(ctx, u.ID, "")
	if err := s.app.Store.DeleteUser(ctx, u.ID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "user.delete", Target: u.Username, Detail: fmt.Sprintf("role %s · %d API token(s) revoked", u.Role, tokens)})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := httpx.Actor(r)
	u, err := s.app.Store.GetUser(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	temp := GeneratePassword(3)
	hash, err := HashPassword(temp)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u.PasswordHash = hash
	u.MustChangePassword = true
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	n := s.revokeUserSessions(ctx, u.ID, "")
	self := u.ID == a.ID
	if self {
		clearSessionCookie(w, r)
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "user.reset_password", Target: u.Username, Detail: fmt.Sprintf("temporary password · %d session(s) signed out", n)})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"password": temp, "signedOut": n, "self": self})
}

// handleUserReset2FA removes a user's authenticator app and passkeys (lost device recovery).
func (s *Service) handleUserReset2FA(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := httpx.Actor(r)
	u, err := s.app.Store.GetUser(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	creds, err := s.app.Store.ListWebAuthnCredentials(ctx, u.ID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u.TOTPEnabled = false
	u.TOTPSecret = ""
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	for _, c := range creds {
		_ = s.app.Store.DeleteWebAuthnCredential(ctx, u.ID, c.ID)
	}
	keep := ""
	if u.ID == a.ID {
		keep = a.SessionID
	}
	n := s.revokeUserSessions(ctx, u.ID, keep)
	s.app.Audit(ctx, core.AuditEntry{Action: "user.reset_2fa", Target: u.Username,
		Detail: fmt.Sprintf("authenticator app and %d passkey(s) removed · %d session(s) signed out", len(creds), n)})
	httpx.WriteJSON(w, http.StatusOK, toUserDTO(u, 0, a.ID))
}

// ---------------------------------------------------------------- sessions

type sessionRowDTO struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	Username   string    `json:"username"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"userAgent"`
	Remember   bool      `json:"remember"`
	Current    bool      `json:"current"`
}

// handleSessionsList: own sessions; admins may pass ?scope=all.
func (s *Service) handleSessionsList(w http.ResponseWriter, r *http.Request, u *store.User) {
	a := httpx.Actor(r)
	owner := u.ID
	if r.URL.Query().Get("scope") == "all" && u.Role == core.RoleAdmin {
		owner = ""
	}
	rows, err := s.app.Store.ListSessions(r.Context(), owner, s.now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]sessionRowDTO, 0, len(rows))
	for _, x := range rows {
		out = append(out, sessionRowDTO{ID: x.ID, UserID: x.UserID, Username: x.Username, CreatedAt: x.CreatedAt, LastSeenAt: x.LastSeenAt,
			ExpiresAt: x.ExpiresAt, IP: x.IP, UserAgent: x.UserAgent, Remember: x.Remember, Current: x.ID == a.SessionID})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) handleSessionDelete(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	a := httpx.Actor(r)
	sess, err := s.app.Store.GetSession(ctx, chi.URLParam(r, "id"))
	if err != nil || (sess.UserID != u.ID && u.Role != core.RoleAdmin) {
		httpx.Fail(w, r, store.ErrNotFound)
		return
	}
	if err := s.app.Store.DeleteSession(ctx, sess.ID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.gates.Delete(sess.ID)
	if sess.ID == a.SessionID {
		clearSessionCookie(w, r)
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "auth.session_revoked", Target: sess.Username, Detail: sess.IP + " · " + truncate(sess.UserAgent, 80)})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- about

type aboutDTO struct {
	Version       string     `json:"version"`
	InstalledAt   *time.Time `json:"installedAt,omitempty"`
	DatabaseBytes int64      `json:"databaseBytes"`
	GoVersion     string     `json:"goVersion"`
}

func (s *Service) handleAbout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	installed, err := s.app.Store.FirstUserCreatedAt(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	size, err := s.app.Store.DatabaseSize(ctx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, aboutDTO{Version: s.app.Config.Version, InstalledAt: installed, DatabaseBytes: size, GoVersion: runtime.Version()})
}
