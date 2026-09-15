package auth

// Relay login: the built-in login page that protects apps. The proxy engine
// serves /.relay/* on every protected host from Relay (render.PrepareHost) and
// asks /.relay/verify before each request. Sessions are separate from admin UI
// sessions, and their cookie belongs to the app's domain.

import (
	"context"
	"errors"
	"html"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

const (
	// PortalCookieName is the Relay login session cookie on app domains.
	PortalCookieName = "relay_login"
	portalCacheTTL   = 15 * time.Second
	portalTouchEvery = time.Minute
)

// liveHosts is implemented by the engine slice: hosts as currently served.
type liveHosts interface {
	LiveHost(ctx context.Context, id string) (*model.ProxyHost, error)
}

type portalSessionEntry struct {
	sess *store.PortalSession
	at   time.Time
}

type portalHostEntry struct {
	host *model.ProxyHost
	at   time.Time
}

var (
	portalSessions sync.Map // session id → portalSessionEntry
	portalHosts    sync.Map // host id → portalHostEntry
)

// PortalRoutes mounts the Relay login endpoints on the root router (outside
// /api). They are reached through the proxy engine on app domains.
func PortalRoutes(app *core.App, r chi.Router) {
	s := serviceOf(app)
	if s == nil {
		return
	}
	r.Get("/.relay/verify", s.handlePortalVerify)
	r.Get("/.relay/login", s.handlePortalLoginPage)
	r.Post("/.relay/login", s.handlePortalLogin)
	r.Get("/.relay/logout", s.handlePortalLogout)
	r.Post("/.relay/logout", s.handlePortalLogout)
}

// ---------------------------------------------------------------- verify

// handlePortalVerify answers the proxy engine's auth subrequest: 200 with
// Remote-* headers when signed in and allowed, 401 to send the visitor to the
// login page, 403 when the account may not use this app.
func (s *Service) handlePortalVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !remoteIsLoopback(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	ctx := r.Context()
	h := s.portalHost(ctx, r.URL.Query().Get("host"))
	if h == nil || !render.RelayLogin(h) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	sess := s.portalSession(ctx, r)
	if sess == nil || !strings.EqualFold(sess.Domain, hostOnly(requestHost(r))) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !portalAllowed(h, sess.UserID) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	hdr := w.Header()
	hdr.Set("Remote-User", sess.Username)
	hdr.Set("Remote-Name", sess.Username)
	hdr.Set("Remote-Email", sess.Email)
	hdr.Set("Remote-Groups", sess.Role)
	w.WriteHeader(http.StatusOK)
}

func portalAllowed(h *model.ProxyHost, userID string) bool {
	return len(h.ForwardAuth.AllowedUsers) == 0 || slices.Contains(h.ForwardAuth.AllowedUsers, userID)
}

// portalHost returns a host as served (cached briefly).
func (s *Service) portalHost(ctx context.Context, id string) *model.ProxyHost {
	if id == "" {
		return nil
	}
	now := s.now()
	if v, ok := portalHosts.Load(id); ok {
		if e := v.(portalHostEntry); now.Sub(e.at) < portalCacheTTL {
			return e.host
		}
	}
	var h *model.ProxyHost
	if lh, ok := s.app.Engine.(liveHosts); ok {
		if got, err := lh.LiveHost(ctx, id); err == nil {
			h = got
		} else if !errors.Is(err, store.ErrNotFound) {
			s.app.Log.Warn("relay login: load host", "host", id, "err", err)
			return nil
		}
	} else if got, err := s.app.Store.Hosts().Get(ctx, id); err == nil {
		h = got
	}
	portalHosts.Store(id, portalHostEntry{host: h, at: now})
	return h
}

// portalSession returns the valid session of the request's cookie, or nil.
func (s *Service) portalSession(ctx context.Context, r *http.Request) *store.PortalSession {
	c, err := r.Cookie(PortalCookieName)
	if err != nil || len(c.Value) != sessionTokenLen {
		return nil
	}
	id := hashToken(c.Value)
	now := s.now()
	var sess *store.PortalSession
	if v, ok := portalSessions.Load(id); ok {
		if e := v.(portalSessionEntry); now.Sub(e.at) < portalCacheTTL {
			sess = e.sess
		}
	}
	if sess == nil {
		got, err := s.app.Store.GetPortalSession(ctx, id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.app.Log.Warn("relay login: load session", "err", err)
			}
			return nil
		}
		if now.Sub(got.LastSeenAt) >= portalTouchEvery {
			got.LastSeenAt = now
			_ = s.app.Store.TouchPortalSession(context.WithoutCancel(ctx), id, now.UTC())
		}
		sess = got
		portalSessions.Store(id, portalSessionEntry{sess: sess, at: now})
	}
	if !now.Before(sess.ExpiresAt) || sess.Disabled {
		portalSessions.Delete(id)
		if !now.Before(sess.ExpiresAt) {
			_ = s.app.Store.DeletePortalSession(context.WithoutCancel(ctx), id)
		}
		return nil
	}
	return sess
}

// ---------------------------------------------------------------- login page

type portalView struct {
	HostID   string
	RD       string
	Username string
	Error    string
}

func (s *Service) handlePortalLoginPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := portalView{HostID: r.URL.Query().Get("host"), RD: portalRD(r)}
	if h := s.portalHost(ctx, v.HostID); h != nil {
		if sess := s.portalSession(ctx, r); sess != nil && strings.EqualFold(sess.Domain, hostOnly(requestHost(r))) {
			if portalAllowed(h, sess.UserID) {
				http.Redirect(w, r, safeRedirect(r, v.RD), http.StatusSeeOther)
				return
			}
			v.Error = "You're signed in as " + sess.Username + ", but that account doesn't have access to this app."
		}
	}
	s.writePortalPage(w, r, http.StatusOK, v)
}

func (s *Service) handlePortalLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.originOK(r) {
		s.writePortalPage(w, r, http.StatusForbidden, portalView{Error: "This sign-in form was submitted from another site. Open the app again and sign in there."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.writePortalPage(w, r, http.StatusBadRequest, portalView{Error: "Something went wrong with the form. Please try again."})
		return
	}
	v := portalView{HostID: r.PostForm.Get("host"), RD: r.PostForm.Get("rd"), Username: strings.TrimSpace(r.PostForm.Get("username"))}
	password := r.PostForm.Get("password")
	code := strings.TrimSpace(r.PostForm.Get("code"))
	fail := func(status int, msg string) {
		v.Error = msg
		s.writePortalPage(w, r, status, v)
	}

	uname := normUsername(v.Username)
	if uname == "" || password == "" {
		fail(http.StatusBadRequest, "Enter your username and password.")
		return
	}
	ip := core.ClientIP(r)
	if s.throttled(ctx, ip, uname) {
		s.auditBlocked(ctx, ip, uname)
		fail(http.StatusTooManyRequests, s.throttleMessage(ctx))
		return
	}
	u, err := s.app.Store.GetUserByUsername(ctx, uname)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(http.StatusInternalServerError, "Sign-in is unavailable right now. Please try again shortly.")
		return
	}
	wrong := func(reason string) {
		s.loginFailed(ctx, ip, uname, u, reason+" (Relay login)")
		if s.throttled(ctx, ip, uname) {
			fail(http.StatusTooManyRequests, s.throttleMessage(ctx))
			return
		}
		fail(http.StatusUnauthorized, "Wrong username or password.")
	}
	if u == nil {
		burnPasswordCheck(password)
		wrong("unknown user")
		return
	}
	if !checkPassword(u.PasswordHash, password) {
		wrong("wrong password")
		return
	}
	if u.Disabled {
		s.loginFailed(ctx, ip, uname, u, "account disabled (Relay login)")
		fail(http.StatusForbidden, "This account is disabled. Ask an admin to re-enable it.")
		return
	}
	method := "password"
	if u.TOTPEnabled {
		if code == "" {
			fail(http.StatusUnauthorized, "Enter the 6-digit code from your authenticator app.")
			return
		}
		if !s.verifyTOTP(u.ID, u.TOTPSecret, code) {
			s.loginFailed(ctx, ip, uname, u, "wrong 2FA code (Relay login)")
			if s.throttled(ctx, ip, uname) {
				fail(http.StatusTooManyRequests, s.throttleMessage(ctx))
				return
			}
			fail(http.StatusUnauthorized, "That code didn't work. Check the time on your authenticator device.")
			return
		}
		method = "totp"
	} else if pk, err := s.app.Store.CountWebAuthnCredentials(ctx, u.ID); err == nil && pk > 0 {
		fail(http.StatusUnauthorized, "This account signs in with a passkey, which Relay login doesn't support. Add an authenticator app to your account to use it here.")
		return
	}
	h := s.portalHost(ctx, v.HostID)
	if h != nil && !portalAllowed(h, u.ID) {
		actor := userActor(u, ip, "")
		s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "auth.app_login", Target: u.Username, Detail: hostOnly(requestHost(r)) + " · " + ip + " · not allowed", Result: "denied"})
		fail(http.StatusForbidden, "Your account doesn't have access to this app.")
		return
	}

	sec := s.security(ctx)
	now := s.now().UTC()
	raw, id := newSessionToken()
	ttl := sessionTTL(sec)
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	domain := hostOnly(requestHost(r))
	sess := &store.PortalSession{ID: id, UserID: u.ID, Domain: domain, CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now,
		IP: ip, UserAgent: truncate(r.UserAgent(), 512)}
	if err := s.app.Store.CreatePortalSession(ctx, sess); err != nil {
		fail(http.StatusInternalServerError, "Sign-in is unavailable right now. Please try again shortly.")
		return
	}
	_ = s.app.Store.RecordLoginAttempt(ctx, s.now(), ip, uname, true)
	_ = s.app.Store.TouchUserActive(ctx, u.ID, now)
	actor := userActor(u, ip, "")
	s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "auth.app_login", Target: u.Username, Detail: domain + " · " + loginDetail(ip, method)})
	http.SetCookie(w, &http.Cookie{
		Name: PortalCookieName, Value: raw, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: isSecureRequest(r), Expires: sess.ExpiresAt, MaxAge: int(ttl.Seconds()),
	})
	http.Redirect(w, r, safeRedirect(r, v.RD), http.StatusSeeOther)
}

func (s *Service) handlePortalLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if c, err := r.Cookie(PortalCookieName); err == nil && len(c.Value) == sessionTokenLen {
		id := hashToken(c.Value)
		if sess, err := s.app.Store.GetPortalSession(ctx, id); err == nil {
			_ = s.app.Store.DeletePortalSession(ctx, id)
			actor := core.Actor{Type: core.ActorUser, ID: sess.UserID, Name: sess.Username, IP: core.ClientIP(r)}
			s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "auth.app_logout", Target: sess.Username, Detail: sess.Domain + " · " + actor.IP})
		}
		portalSessions.Delete(id)
	}
	http.SetCookie(w, &http.Cookie{Name: PortalCookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecureRequest(r), MaxAge: -1})
	body := `<h1>You're signed out</h1><p>You have been signed out of <span class="host">` + html.EscapeString(hostOnly(requestHost(r))) + `</span>.</p>` +
		`<form method="get" action="/"><button type="submit">Sign in again</button></form>`
	s.writePortalHTML(w, r, http.StatusOK, "Signed out", body)
}

func (s *Service) throttleMessage(ctx context.Context) string {
	mins := s.security(ctx).LoginLockoutMinutes
	if mins <= 0 {
		return "Too many attempts. Please wait a few minutes and try again."
	}
	unit := "minutes"
	if mins == 1 {
		unit = "minute"
	}
	return "Too many attempts. Please wait " + strconv.Itoa(mins) + " " + unit + " and try again."
}

// ---------------------------------------------------------------- rendering

func (s *Service) writePortalPage(w http.ResponseWriter, r *http.Request, status int, v portalView) {
	domain := hostOnly(requestHost(r))
	var b strings.Builder
	b.WriteString(`<h1>Sign in</h1><p>to continue to <span class="host">` + html.EscapeString(domain) + `</span></p>`)
	if v.Error != "" {
		b.WriteString(`<div class="error" role="alert">` + html.EscapeString(v.Error) + `</div>`)
	}
	b.WriteString(`<form method="post" action="/.relay/login">`)
	b.WriteString(`<input type="hidden" name="host" value="` + html.EscapeString(v.HostID) + `">`)
	b.WriteString(`<input type="hidden" name="rd" value="` + html.EscapeString(v.RD) + `">`)
	b.WriteString(`<label>Username<input name="username" autocomplete="username" autocapitalize="none" spellcheck="false" required value="` + html.EscapeString(v.Username) + `"` + map[bool]string{true: " autofocus", false: ""}[v.Username == ""] + `></label>`)
	b.WriteString(`<label>Password<input type="password" name="password" autocomplete="current-password" required` + map[bool]string{true: " autofocus", false: ""}[v.Username != ""] + `></label>`)
	b.WriteString(`<label>Authenticator code<input name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" placeholder="Only if your account uses 2FA"></label>`)
	b.WriteString(`<button type="submit">Sign in</button></form>`)
	s.writePortalHTML(w, r, status, "Sign in · "+domain, b.String())
}

func (s *Service) writePortalHTML(w http.ResponseWriter, r *http.Request, status int, title, body string) {
	ctx := r.Context()
	set, _ := store.LoadSettings[model.ErrorPagesSettings](ctx, s.app.Store, model.SettingsErrorPages)
	brand := set.BrandName
	if brand == "" {
		if gen, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral); err == nil {
			brand = gen.InstanceName
		}
	}
	if brand == "" {
		brand = "Relay"
	}
	page := render.PageShell(title, render.Accent(set), "Protected by "+brand, body, 0)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Referrer-Policy", "same-origin")
	hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(page))
}

// ---------------------------------------------------------------- helpers

// hostOnly strips the port from a host[:port].
func hostOnly(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(h, "[]")
}

// safeRedirect returns rd when it stays on the request's host, else "/".
func safeRedirect(r *http.Request, rd string) string {
	rd = strings.TrimSpace(rd)
	if rd == "" {
		return "/"
	}
	u, err := url.Parse(rd)
	if err != nil {
		return "/"
	}
	path := u.EscapedPath()
	if u.Scheme == "" && u.Host == "" {
		if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, "/\\") {
			return "/"
		}
	} else if (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(hostOnly(u.Host), hostOnly(requestHost(r))) {
		return "/"
	}
	if strings.HasPrefix(path, render.PortalPrefix) {
		return "/"
	}
	return rd
}

// portalRD returns the page to return to after signing in. nginx appends it
// unencoded as the last parameter ("…&rd=https://app/x?a=1&b=2"), Relay Edge
// encodes it, so everything after "rd=" is taken.
func portalRD(r *http.Request) string {
	raw := r.URL.RawQuery
	i := strings.Index(raw, "rd=")
	if i < 0 || (i > 0 && raw[i-1] != '&') {
		return r.URL.Query().Get("rd")
	}
	v := raw[i+3:]
	if strings.Contains(v, "://") || strings.HasPrefix(v, "/") {
		return v
	}
	if u, err := url.QueryUnescape(v); err == nil {
		return u
	}
	return v
}
