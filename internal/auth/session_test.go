package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

func TestSessionLifecycle(t *testing.T) {
	s, clock := newTestService(t)
	ctx := context.Background()
	u := createTestUser(t, s, "jonas", core.RoleAdmin, "correct-horse-battery")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.home.lan/api/auth/login", nil)
	sess, err := s.createSession(ctx, rec, req, u, false)
	if err != nil {
		t.Fatal(err)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 0 {
		t.Fatalf("unexpected cookie attributes: %+v", c)
	}
	if c.Secure {
		t.Fatal("cookie must not be Secure over plain HTTP")
	}
	if sess.ID == c.Value || sess.ID != hashToken(c.Value) {
		t.Fatal("database must store the sha256 of the cookie, not the cookie")
	}

	authed := func(method string) (core.Actor, error) {
		r := httptest.NewRequest(method, "http://proxy.home.lan/api/hosts", nil)
		r.AddCookie(&http.Cookie{Name: CookieName, Value: c.Value})
		return s.Authenticate(r)
	}
	a, err := authed(http.MethodGet)
	if err != nil || a.Type != core.ActorUser || a.ID != u.ID || a.Role != core.RoleAdmin || a.SessionID != sess.ID {
		t.Fatalf("Authenticate: %+v %v", a, err)
	}

	// Sliding expiry: activity after a minute pushes expires_at forward.
	clock.advance(2 * time.Hour)
	if _, err := authed(http.MethodGet); err != nil {
		t.Fatal(err)
	}
	slid, _ := s.app.Store.GetSession(ctx, sess.ID)
	if !slid.ExpiresAt.After(sess.ExpiresAt) {
		t.Fatalf("expiry did not slide: %v → %v", sess.ExpiresAt, slid.ExpiresAt)
	}

	// Cross-origin writes never authenticate by cookie.
	r := httptest.NewRequest(http.MethodPost, "http://proxy.home.lan/api/hosts", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: c.Value})
	r.Header.Set("Origin", "https://evil.example")
	if _, err := s.Authenticate(r); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("cross-origin POST authenticated: %v", err)
	}
	r.Header.Set("Origin", "http://proxy.home.lan")
	if _, err := s.Authenticate(r); err != nil {
		t.Fatalf("same-origin POST rejected: %v", err)
	}

	// Idle past the TTL (default 7 days) → signed out and the row is removed.
	clock.advance(8 * 24 * time.Hour)
	if _, err := authed(http.MethodGet); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("expired session accepted: %v", err)
	}
	if _, err := s.app.Store.GetSession(ctx, sess.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session not deleted: %v", err)
	}

	// Disabled users are rejected even with a live session.
	rec = httptest.NewRecorder()
	if _, err := s.createSession(ctx, rec, req, u, true); err != nil {
		t.Fatal(err)
	}
	c = sessionCookie(t, rec)
	if c.MaxAge <= 0 {
		t.Fatal("remembered session needs a persistent cookie")
	}
	if _, err := authed(http.MethodGet); err != nil {
		t.Fatal(err)
	}
	u.Disabled = true
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := authed(http.MethodGet); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("disabled user authenticated: %v", err)
	}

	// Garbage cookies.
	r = httptest.NewRequest(http.MethodGet, "http://proxy.home.lan/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: strings.Repeat("x", sessionTokenLen)})
	if _, err := s.Authenticate(r); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("garbage cookie: %v", err)
	}
}

func TestShortenedTTLExpiresIdleSessions(t *testing.T) {
	s, clock := newTestService(t)
	ctx := context.Background()
	u := createTestUser(t, s, "mira", core.RoleEditor, "correct-horse-battery")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.home.lan/", nil)
	if _, err := s.createSession(ctx, rec, req, u, false); err != nil {
		t.Fatal(err)
	}
	c := sessionCookie(t, rec)
	sec := store.DefaultSecurity()
	sec.SessionTTLHours = 1
	if err := s.app.Store.PutSettings(ctx, model.SettingsSecurity, sec); err != nil {
		t.Fatal(err)
	}
	s.invalidate()
	clock.advance(90 * time.Minute)
	r := httptest.NewRequest(http.MethodGet, "http://proxy.home.lan/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: c.Value})
	if _, err := s.Authenticate(r); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("idle session outlived the shortened TTL: %v", err)
	}
}

func TestSecureCookieBehindProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://proxy.home.lan/", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("X-Forwarded-Proto", "https")
	if !isSecureRequest(r) {
		t.Fatal("X-Forwarded-Proto https from loopback should be secure")
	}
	r.RemoteAddr = "192.168.1.24:50000"
	if isSecureRequest(r) {
		t.Fatal("X-Forwarded-Proto must only be trusted from loopback")
	}
}

func TestMustEnrollGate(t *testing.T) {
	u := &store.User{Role: core.RoleAdmin}
	sec := store.DefaultSecurity()
	if computeGate(u, sec, 0).MustEnroll2FA {
		t.Fatal("2FA not required by settings")
	}
	sec.Require2FAForAdmins = true
	if !computeGate(u, sec, 0).MustEnroll2FA {
		t.Fatal("admin without 2FA must enrol")
	}
	if computeGate(u, sec, 1).MustEnroll2FA {
		t.Fatal("a passkey satisfies 2FA")
	}
	u.TOTPEnabled = true
	if computeGate(u, sec, 0).MustEnroll2FA {
		t.Fatal("TOTP satisfies 2FA")
	}
	if computeGate(&store.User{Role: core.RoleEditor}, sec, 0).MustEnroll2FA {
		t.Fatal("editors are not required to enrol")
	}
}
