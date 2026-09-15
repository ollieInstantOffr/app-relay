package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

func portalRouter(s *Service) http.Handler {
	r := chi.NewRouter()
	PortalRoutes(s.app, r)
	return r
}

// appRequest is a request as the proxy engine forwards it for app.example.com.
func appRequest(method, target, domain string, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Forwarded-Host", domain)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Real-IP", "198.51.100.7")
	return req
}

func setPortalHost(s *Service, h *model.ProxyHost) {
	portalHosts.Store(h.ID, portalHostEntry{host: h, at: s.now()})
}

func TestRelayLoginFlow(t *testing.T) {
	s, clock := newTestService(t)
	alice := createTestUser(t, s, "alice", core.RoleMember, "correct horse battery")
	createTestUser(t, s, "bob", core.RoleViewer, "another long password")
	wiki := &model.ProxyHost{Meta: model.Meta{ID: "wiki-flow"}, Domains: []string{"wiki.example.com"}, Enabled: true,
		ForwardAuth: model.ForwardAuth{Enabled: true, Provider: model.ForwardAuthRelay}}
	setPortalHost(s, wiki)
	h := portalRouter(s)
	do := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	verify := func(cookie *http.Cookie, domain string) *httptest.ResponseRecorder {
		req := appRequest(http.MethodGet, "/.relay/verify?host=wiki-flow", domain, "")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		return do(req)
	}

	if rec := verify(nil, "wiki.example.com"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify without cookie = %d", rec.Code)
	}
	remote := appRequest(http.MethodGet, "/.relay/verify?host=wiki-flow", "wiki.example.com", "")
	remote.RemoteAddr = "203.0.113.9:1234"
	if rec := do(remote); rec.Code != http.StatusForbidden {
		t.Fatalf("verify from a non-loopback address = %d", rec.Code)
	}

	// The login page shows the domain and keeps the (unencoded) return URL.
	page := do(appRequest(http.MethodGet, "/.relay/login?host=wiki-flow&rd=https://wiki.example.com/docs?a=1&b=2", "wiki.example.com", ""))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "wiki.example.com") || !strings.Contains(page.Body.String(), `value="https://wiki.example.com/docs?a=1&amp;b=2"`) {
		t.Fatalf("login page = %d %s", page.Code, page.Body)
	}

	form := func(user, pass, rd string) string {
		return url.Values{"username": {user}, "password": {pass}, "host": {"wiki-flow"}, "rd": {rd}}.Encode()
	}
	login := func(body, origin string) *httptest.ResponseRecorder {
		req := appRequest(http.MethodPost, "/.relay/login", "wiki.example.com", body)
		req.Header.Set("Origin", origin)
		return do(req)
	}

	if rec := login(form("alice", "wrong password!!", "/"), "https://wiki.example.com"); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Wrong username or password") {
		t.Fatalf("wrong password = %d %s", rec.Code, rec.Body)
	}
	if rec := login(form("alice", "correct horse battery", "/"), "https://evil.example.net"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d", rec.Code)
	}

	rec := login(form("alice", "correct horse battery", "https://wiki.example.com/docs?a=1"), "https://wiki.example.com")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://wiki.example.com/docs?a=1" {
		t.Fatalf("login = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == PortalCookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.Secure || !cookie.HttpOnly {
		t.Fatalf("session cookie = %+v", cookie)
	}

	ok := verify(cookie, "wiki.example.com")
	if ok.Code != http.StatusOK || ok.Header().Get("Remote-User") != "alice" || ok.Header().Get("Remote-Groups") != core.RoleMember {
		t.Fatalf("verify = %d %v", ok.Code, ok.Header())
	}
	// The cookie belongs to one domain.
	if rec := verify(cookie, "other.example.com"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify on another domain = %d", rec.Code)
	}

	// Only selected users: alice is not on the list.
	restricted := *wiki
	restricted.ForwardAuth.AllowedUsers = []string{"someone-else"}
	setPortalHost(s, &restricted)
	if rec := verify(cookie, "wiki.example.com"); rec.Code != http.StatusForbidden {
		t.Fatalf("verify for a user who isn't allowed = %d", rec.Code)
	}
	if rec := login(form("bob", "another long password", "/"), "https://wiki.example.com"); rec.Code != http.StatusForbidden {
		t.Fatalf("login for a user who isn't allowed = %d", rec.Code)
	}
	restricted.ForwardAuth.AllowedUsers = []string{alice.ID}
	setPortalHost(s, &restricted)
	if rec := verify(cookie, "wiki.example.com"); rec.Code != http.StatusOK {
		t.Fatalf("verify for an allowed user = %d", rec.Code)
	}

	// Redirects never leave the app's domain.
	if rec := login(form("alice", "correct horse battery", "https://evil.example.net/x"), "https://wiki.example.com"); rec.Header().Get("Location") != "/" {
		t.Fatalf("open redirect: %q", rec.Header().Get("Location"))
	}

	// Sign out.
	out := appRequest(http.MethodGet, "/.relay/logout", "wiki.example.com", "")
	out.AddCookie(cookie)
	if rec := do(out); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "signed out") {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	if rec := verify(cookie, "wiki.example.com"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify after logout = %d", rec.Code)
	}

	// Sessions expire.
	rec = login(form("alice", "correct horse battery", "/"), "https://wiki.example.com")
	for _, c := range rec.Result().Cookies() {
		if c.Name == PortalCookieName {
			cookie = c
		}
	}
	setPortalHost(s, wiki)
	clock.advance(24 * 365 * 3600 * 1e9)
	setPortalHost(s, wiki)
	if rec := verify(cookie, "wiki.example.com"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify after expiry = %d", rec.Code)
	}
}

func TestAppOnlyAccountsCantUseAdminUI(t *testing.T) {
	s, _ := newTestService(t)
	createTestUser(t, s, "member1", core.RoleMember, "correct horse battery")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(`{"username":"member1","password":"correct horse battery"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "app_access_only") {
		t.Fatalf("admin login for an app-only account = %d %s", rec.Code, rec.Body)
	}
}

func TestPortalRedirectHelpers(t *testing.T) {
	r := appRequest(http.MethodGet, "/.relay/login", "app.example.com", "")
	for rd, want := range map[string]string{
		"":                                 "/",
		"/dashboard?x=1":                   "/dashboard?x=1",
		"//evil.example.net":               "/",
		"https://app.example.com/a":        "https://app.example.com/a",
		"https://APP.example.com:8443/a":   "https://APP.example.com:8443/a",
		"https://evil.example.net/a":       "/",
		"javascript:alert(1)":              "/",
		"https://app.example.com/.relay/x": "/",
	} {
		if got := safeRedirect(r, rd); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", rd, got, want)
		}
	}
	for raw, want := range map[string]string{
		"host=x&rd=https://a.example/b?c=1&d=2":     "https://a.example/b?c=1&d=2",
		"host=x&rd=https%3A%2F%2Fa.example%2Fb%3Fc": "https://a.example/b?c",
		"host=x":        "",
		"host=x&card=1": "",
	} {
		req := httptest.NewRequest(http.MethodGet, "/.relay/login?"+raw, nil)
		if got := portalRD(req); got != want {
			t.Errorf("portalRD(%q) = %q, want %q", raw, got, want)
		}
	}
}
