package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func TestIPAllowedByRules(t *testing.T) {
	lanOnly := []model.IPRule{
		{Action: "deny", CIDR: "192.168.1.66"},
		{Action: "allow", CIDR: "192.168.1.0/24"},
		{Action: "allow", CIDR: "127.0.0.1"},
		{Action: "allow", CIDR: "fd00::/8"},
		{Action: "allow", CIDR: "not-a-cidr"}, // ignored
		{Action: "deny", CIDR: "all"},
	}
	cases := []struct {
		rules []model.IPRule
		ip    string
		want  bool
	}{
		{lanOnly, "192.168.1.24", true},
		{lanOnly, "192.168.1.66", false}, // first match wins
		{lanOnly, "192.168.2.1", false},
		{lanOnly, "10.0.0.5", false},
		{lanOnly, "127.0.0.1", true},
		{lanOnly, "::ffff:192.168.1.24", true}, // IPv4-mapped IPv6
		{lanOnly, "fd12:3456::1", true},
		{lanOnly, "2001:db8::1", false},
		{nil, "203.0.113.7", true},                                               // no rules → allow (nginx)
		{[]model.IPRule{{Action: "allow", CIDR: "10.0.0.0/8"}}, "8.8.8.8", true}, // no match → allow (nginx)
		{[]model.IPRule{{Action: "deny", CIDR: "all"}}, "10.1.2.3", false},
	}
	for _, c := range cases {
		if got := ipAllowedByRules(c.rules, netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("ipAllowedByRules(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestNetworkGuard(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	h := testRouter(s)

	get := func(path, remote string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.home.lan"+path, nil)
		r.RemoteAddr = remote
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// Unrestricted by default.
	if rec := get("/", "203.0.113.9:4000", nil); rec.Code != http.StatusOK {
		t.Fatalf("unrestricted: %d", rec.Code)
	}

	al := &model.AccessList{Name: "lan-only", Rules: []model.IPRule{
		{ID: "a", Action: "allow", CIDR: "192.168.1.0/24"},
		{ID: "b", Action: "deny", CIDR: "all"},
	}}
	if err := s.app.Store.AccessLists().Create(ctx, al); err != nil {
		t.Fatal(err)
	}
	sec := store.DefaultSecurity()
	sec.AdminAccessListID = al.ID
	if err := s.app.Store.PutSettings(ctx, model.SettingsSecurity, sec); err != nil {
		t.Fatal(err)
	}
	s.invalidate()

	if rec := get("/api/auth/session", "203.0.113.9:4000", nil); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "network_restricted") {
		t.Fatalf("outside API request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/", "203.0.113.9:4000", nil); rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("outside UI request: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get("/api/auth/session", "192.168.1.24:4000", nil); rec.Code != http.StatusOK {
		t.Fatalf("LAN request: %d", rec.Code)
	}
	if rec := get("/", "127.0.0.1:4000", nil); rec.Code != http.StatusOK {
		t.Fatalf("loopback request: %d", rec.Code)
	}
	// Proxied through the local nginx: judged by the forwarded client address.
	if rec := get("/", "127.0.0.1:4000", map[string]string{"X-Real-IP": "203.0.113.9"}); rec.Code != http.StatusForbidden {
		t.Fatalf("proxied outside request: %d", rec.Code)
	}
	if rec := get("/", "127.0.0.1:4000", map[string]string{"X-Real-IP": "192.168.1.30"}); rec.Code != http.StatusOK {
		t.Fatalf("proxied LAN request: %d", rec.Code)
	}
	// Forwarded headers from a non-loopback peer are ignored.
	if rec := get("/", "203.0.113.9:4000", map[string]string{"X-Real-IP": "192.168.1.30"}); rec.Code != http.StatusForbidden {
		t.Fatalf("spoofed X-Real-IP accepted: %d", rec.Code)
	}
}

func TestAccountGate(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	h := testRouter(s)
	u := createTestUser(t, s, "alex", core.RoleViewer, "correct-horse-battery")
	u.MustChangePassword = true
	if err := s.app.Store.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if _, err := s.createSession(ctx, rec, httptest.NewRequest(http.MethodPost, "http://proxy.home.lan/", nil), u, false); err != nil {
		t.Fatal(err)
	}
	c := sessionCookie(t, rec)
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://proxy.home.lan"+path, nil)
		r.AddCookie(c)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := get("/api/hosts"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "password_change_required") {
		t.Fatalf("gated API call: %d %s", rec.Code, rec.Body.String())
	}
	rec = get("/api/auth/session")
	if rec.Code != http.StatusOK {
		t.Fatalf("session endpoint must stay reachable: %d", rec.Code)
	}
	var sess sessionDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if !sess.Authenticated || sess.User == nil || !sess.User.MustChangePassword {
		t.Fatalf("session: %+v", sess)
	}
}
