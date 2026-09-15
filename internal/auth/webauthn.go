package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/store"
)

const (
	ceremonyCookie = "relay_webauthn"
	ceremonyPath   = "/api/auth/passkeys"
	ceremonyTTL    = 5 * time.Minute
	maxCeremonies  = 5000
)

// ceremony is the server-side state between a WebAuthn begin and finish call.
// It lives in memory, keyed by a random id in a short-lived HttpOnly cookie.
type ceremony struct {
	kind    string // register | login
	userID  string
	data    webauthn.SessionData
	rpID    string
	origin  string
	expires time.Time
}

type ceremonyStore struct {
	mu sync.Mutex
	m  map[string]*ceremony
}

func newCeremonyStore() *ceremonyStore { return &ceremonyStore{m: map[string]*ceremony{}} }

func (c *ceremonyStore) put(now time.Time, cer *ceremony) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	if len(c.m) >= maxCeremonies {
		return "", errors.New("too many pending passkey requests — try again in a few minutes")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	cer.expires = now.Add(ceremonyTTL)
	c.m[id] = cer
	return id, nil
}

// take removes and returns an unexpired ceremony of the given kind.
func (c *ceremonyStore) take(now time.Time, id, kind string) *ceremony {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[id]
	if !ok {
		return nil
	}
	delete(c.m, id)
	if now.After(cer.expires) || cer.kind != kind {
		return nil
	}
	return cer
}

func (c *ceremonyStore) prune(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
}

func (c *ceremonyStore) pruneLocked(now time.Time) {
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
}

// relyingParty derives the WebAuthn RP ID (host without port) and origin from the request.
func (s *Service) relyingParty(r *http.Request) (rpID, origin string, err error) {
	host := strings.ToLower(requestHost(r))
	if host == "" {
		return "", "", errors.New("missing Host header")
	}
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	hostname = strings.Trim(hostname, "[]")
	if net.ParseIP(hostname) != nil {
		return "", "", errors.New("Passkeys need a domain name — open Relay by its domain instead of an IP address")
	}
	scheme := "http"
	if isSecureRequest(r) {
		scheme = "https"
	}
	origin = scheme + "://" + host
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
			oh := strings.ToLower(u.Hostname())
			if stripDefaultPort(u.Scheme, u.Host) == stripDefaultPort(u.Scheme, host) || (s.app.Config.DevMode && oh == hostname) {
				origin = u.Scheme + "://" + strings.ToLower(u.Host)
			}
		}
	}
	return hostname, origin, nil
}

func newWebAuthn(rpID, origin string) (*webauthn.WebAuthn, error) {
	t := webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL, TimeoutUVD: ceremonyTTL}
	return webauthn.New(&webauthn.Config{
		RPID: rpID, RPDisplayName: "Relay", RPOrigins: []string{origin},
		Timeouts: webauthn.TimeoutsConfig{Login: t, Registration: t},
	})
}

type waUser struct {
	u     *store.User
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return []byte(w.u.ID) }
func (w *waUser) WebAuthnName() string                       { return w.u.Username }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.Username }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *Service) loadWAUser(ctx context.Context, u *store.User) (*waUser, error) {
	rows, err := s.app.Store.ListWebAuthnCredentials(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(row.Data), &c); err != nil {
			s.app.Log.Warn("auth: undecodable passkey", "id", row.ID, "err", err)
			continue
		}
		creds = append(creds, c)
	}
	return &waUser{u: u, creds: creds}, nil
}

func credentialID(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func setCeremonyCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: id, Path: ceremonyPath, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: isSecureRequest(r), MaxAge: int(ceremonyTTL.Seconds())})
}

func clearCeremonyCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: "", Path: ceremonyPath, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: isSecureRequest(r), MaxAge: -1})
}

func (s *Service) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) *ceremony {
	c, err := r.Cookie(ceremonyCookie)
	clearCeremonyCookie(w, r)
	if err != nil || c.Value == "" {
		return nil
	}
	return s.ceremonies.take(s.now(), c.Value, kind)
}

func webauthnErrMsg(err error) string {
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.Details != "" {
		return truncate(perr.Details, 200)
	}
	return truncate(err.Error(), 200)
}

type passkeyDTO struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

func (s *Service) handlePasskeysList(w http.ResponseWriter, r *http.Request, u *store.User) {
	rows, err := s.app.Store.ListWebAuthnCredentials(r.Context(), u.ID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]passkeyDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, passkeyDTO{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request, u *store.User) {
	rpID, origin, err := s.relyingParty(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_unavailable", err.Error())
		return
	}
	wa, err := newWebAuthn(rpID, origin)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_unavailable", webauthnErrMsg(err))
		return
	}
	user, err := s.loadWAUser(r.Context(), u)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	creation, sd, err := wa.BeginRegistration(user,
		webauthn.WithExclusions(webauthn.Credentials(user.creds).CredentialDescriptors()),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}),
	)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_unavailable", webauthnErrMsg(err))
		return
	}
	id, err := s.ceremonies.put(s.now(), &ceremony{kind: "register", userID: u.ID, data: *sd, rpID: rpID, origin: origin})
	if err != nil {
		httpx.WriteError(w, http.StatusTooManyRequests, "busy", err.Error())
		return
	}
	setCeremonyCookie(w, r, id)
	httpx.WriteJSON(w, http.StatusOK, creation)
}

func passkeyName(q string) string {
	q = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(q))
	if rs := []rune(q); len(rs) > 64 {
		q = string(rs[:64])
	}
	if q == "" {
		return "Passkey"
	}
	return q
}

func (s *Service) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request, u *store.User) {
	cer := s.takeCeremony(w, r, "register")
	if cer == nil || cer.userID != u.ID {
		httpx.WriteError(w, http.StatusBadRequest, "ceremony_expired", "The passkey request expired — try again")
		return
	}
	ctx := r.Context()
	wa, err := newWebAuthn(cer.rpID, cer.origin)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_failed", webauthnErrMsg(err))
		return
	}
	user, err := s.loadWAUser(ctx, u)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cred, err := wa.FinishRegistration(user, cer.data, r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_failed", "Passkey registration failed: "+webauthnErrMsg(err))
		return
	}
	data, err := json.Marshal(cred)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	row := &store.WebAuthnCredentialRow{ID: credentialID(cred.ID), UserID: u.ID, Name: passkeyName(r.URL.Query().Get("name")),
		Data: string(data), CreatedAt: s.now().UTC()}
	if err := s.app.Store.CreateWebAuthnCredential(ctx, row); err != nil {
		if errors.Is(err, store.ErrConflict) {
			httpx.WriteError(w, http.StatusConflict, "passkey_exists", "This passkey is already registered")
			return
		}
		httpx.Fail(w, r, err)
		return
	}
	_ = s.app.Store.SetSessionMFAPending(ctx, u.ID, false)
	s.app.Audit(ctx, core.AuditEntry{Action: "auth.passkey_added", Target: u.Username, Detail: row.Name})
	httpx.WriteJSON(w, http.StatusCreated, passkeyDTO{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt})
}

func (s *Service) handlePasskeyDelete(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if u.Role == core.RoleAdmin && !u.TOTPEnabled && s.security(ctx).Require2FAForAdmins {
		if pk, err := s.app.Store.CountWebAuthnCredentials(ctx, u.ID); err != nil {
			httpx.Fail(w, r, err)
			return
		} else if pk <= 1 {
			httpx.WriteError(w, http.StatusConflict, "2fa_required", "Admins must keep 2FA on — set up an authenticator app or add another passkey first")
			return
		}
	}
	rows, err := s.app.Store.ListWebAuthnCredentials(ctx, u.ID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	name := ""
	for _, row := range rows {
		if row.ID == id {
			name = row.Name
		}
	}
	if err := s.app.Store.DeleteWebAuthnCredential(ctx, u.ID, id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "auth.passkey_removed", Target: u.Username, Detail: name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		httpx.WriteError(w, http.StatusForbidden, "cross_origin", "cross-origin sign-in refused")
		return
	}
	ctx := r.Context()
	if n, err := s.app.Store.CountUsers(ctx); err != nil {
		httpx.Fail(w, r, err)
		return
	} else if n == 0 {
		httpx.WriteError(w, http.StatusConflict, "setup_required", "Relay has no accounts yet — finish setup first")
		return
	}
	ip := core.ClientIP(r)
	if c, err := s.failureCounts(ctx, ip, ""); err == nil && c.IP >= s.security(ctx).LoginMaxAttempts*ipFailureMultiplier {
		s.writeThrottled(ctx, w)
		return
	}
	rpID, origin, err := s.relyingParty(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_unavailable", err.Error())
		return
	}
	wa, err := newWebAuthn(rpID, origin)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_unavailable", webauthnErrMsg(err))
		return
	}
	assertion, sd, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_unavailable", webauthnErrMsg(err))
		return
	}
	id, err := s.ceremonies.put(s.now(), &ceremony{kind: "login", data: *sd, rpID: rpID, origin: origin})
	if err != nil {
		httpx.WriteError(w, http.StatusTooManyRequests, "busy", err.Error())
		return
	}
	setCeremonyCookie(w, r, id)
	httpx.WriteJSON(w, http.StatusOK, assertion)
}

// handlePasskeyLoginFinish completes a discoverable passkey sign-in. A passkey
// (with user verification) satisfies 2FA on its own. Query: remember=1.
func (s *Service) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		httpx.WriteError(w, http.StatusForbidden, "cross_origin", "cross-origin sign-in refused")
		return
	}
	ctx := r.Context()
	ip := core.ClientIP(r)
	cer := s.takeCeremony(w, r, "login")
	if cer == nil {
		httpx.WriteError(w, http.StatusBadRequest, "ceremony_expired", "The passkey request expired — try again")
		return
	}
	wa, err := newWebAuthn(cer.rpID, cer.origin)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "passkey_failed", webauthnErrMsg(err))
		return
	}
	var found *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := s.app.Store.GetUser(ctx, string(userHandle))
		if err != nil {
			return nil, err
		}
		wu, err := s.loadWAUser(ctx, u)
		if err != nil {
			return nil, err
		}
		found = wu
		return wu, nil
	}
	_, cred, err := wa.FinishPasskeyLogin(handler, cer.data, r)
	var u *store.User
	uname := ""
	if found != nil {
		u = found.u
		uname = strings.ToLower(u.Username)
	}
	if err == nil && cred != nil && cred.Authenticator.CloneWarning {
		err = errors.New("signature counter went backwards (possible cloned authenticator)")
	}
	if err != nil || u == nil {
		reason := "passkey rejected"
		if err != nil {
			reason += " (" + webauthnErrMsg(err) + ")"
		}
		s.loginFailed(ctx, ip, uname, u, reason)
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_passkey", "That passkey wasn't accepted")
		return
	}
	if s.throttled(ctx, ip, uname) {
		s.auditBlocked(ctx, ip, uname)
		s.writeThrottled(ctx, w)
		return
	}
	if u.Disabled {
		s.loginFailed(ctx, ip, uname, u, "account disabled")
		httpx.WriteError(w, http.StatusForbidden, "account_disabled", "This account is disabled. Ask an admin to re-enable it.")
		return
	}
	if data, err := json.Marshal(cred); err == nil {
		_ = s.app.Store.UpdateWebAuthnCredentialUse(ctx, credentialID(cred.ID), string(data), s.now())
	}
	s.completeLogin(w, r, u, "passkey", r.URL.Query().Get("remember") == "1")
}
