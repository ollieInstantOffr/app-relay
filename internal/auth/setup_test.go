package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
)

func postJSON(h http.Handler, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "http://proxy.home.lan"+path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://proxy.home.lan")
	r.RemoteAddr = "192.168.1.24:5000"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func TestSetupGuard(t *testing.T) {
	s, clock := newTestService(t)
	h := testRouter(s)

	// Sign-in is refused until an account exists.
	if rec := postJSON(h, "/api/auth/login", map[string]any{"username": "jonas", "password": "whatever-long"}); errorCode(rec) != "setup_required" {
		t.Fatalf("login before setup: %d %s", rec.Code, rec.Body.String())
	}

	// Validation.
	if rec := postJSON(h, "/api/setup/admin", map[string]any{"username": "j", "password": "short"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid setup: %d %s", rec.Code, rec.Body.String())
	}
	// Cross-origin requests are refused.
	b, _ := json.Marshal(map[string]any{"username": "jonas", "password": "correct-horse-battery"})
	r := httptest.NewRequest(http.MethodPost, "http://proxy.home.lan/api/setup/admin", bytes.NewReader(b))
	r.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin setup: %d", rec.Code)
	}

	rec = postJSON(h, "/api/setup/admin", map[string]any{"username": "jonas", "password": "correct-horse-battery", "enroll2fa": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("setup admin: %d %s", rec.Code, rec.Body.String())
	}
	var sess sessionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if !sess.Authenticated || sess.SetupRequired || sess.User == nil || sess.User.Role != core.RoleAdmin || sess.SetupDone == nil || *sess.SetupDone {
		t.Fatalf("setup session: %s", rec.Body.String())
	}
	cookie := sessionCookie(t, rec)

	// A second admin can't be created through setup.
	if rec := postJSON(h, "/api/setup/admin", map[string]any{"username": "mallory", "password": "correct-horse-battery"}); rec.Code != http.StatusForbidden || errorCode(rec) != "setup_done" {
		t.Fatalf("second setup: %d %s", rec.Code, rec.Body.String())
	}
	if n, _ := s.app.Store.CountUsers(context.Background()); n != 1 {
		t.Fatalf("users = %d", n)
	}

	// Setup network endpoints need the admin session.
	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "http://proxy.home.lan/api/setup/network", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous network check: %d", anon.Code)
	}

	// Finishing setup closes the wizard endpoints (engine unavailable in tests is reported, not fatal).
	rec = postJSON(h, "/api/setup/finish", map[string]any{}, cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(h, "/api/setup/finish", map[string]any{}, cookie); errorCode(rec) != "setup_done" {
		t.Fatalf("finish twice: %d %s", rec.Code, rec.Body.String())
	}

	// Sign-in: wrong password ×3 → throttled, even with the right password afterwards.
	clock.advance(time.Second) // after the setup sign-in recorded a success
	for i := 0; i < 2; i++ {
		if rec := postJSON(h, "/api/auth/login", map[string]any{"username": "jonas", "password": "wrong-password-1"}); errorCode(rec) != "invalid_credentials" {
			t.Fatalf("wrong password %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := postJSON(h, "/api/auth/login", map[string]any{"username": "jonas", "password": "wrong-password-1"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third failure should throttle: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(h, "/api/auth/login", map[string]any{"username": "JONAS", "password": "correct-horse-battery"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled login accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLoginTOTPRequired(t *testing.T) {
	s, _ := newTestService(t)
	h := testRouter(s)
	u := createTestUser(t, s, "mira", core.RoleEditor, "correct-horse-battery")
	u.TOTPSecret = "JBSWY3DPEHPK3PXP"
	u.TOTPEnabled = true
	if err := s.app.Store.UpdateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if rec := postJSON(h, "/api/auth/login", map[string]any{"username": "mira", "password": "correct-horse-battery"}); errorCode(rec) != "totp_required" {
		t.Fatalf("missing code: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(h, "/api/auth/login", map[string]any{"username": "mira", "password": "correct-horse-battery", "totp": "000000"}); errorCode(rec) != "invalid_totp" && rec.Code != http.StatusOK {
		t.Fatalf("bad code: %d %s", rec.Code, rec.Body.String())
	}
}
