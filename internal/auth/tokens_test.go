package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/store"
)

func TestTokenFormat(t *testing.T) {
	cases := []struct {
		surfaces []string
		prefix   string
	}{
		{[]string{"mcp"}, PrefixMCP},
		{[]string{"rest"}, PrefixAPI},
		{[]string{"mcp", "rest"}, PrefixAPI},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		for i := 0; i < 200; i++ {
			raw, prefix := newAPIToken(c.surfaces)
			if prefix != c.prefix || !strings.HasPrefix(raw, c.prefix) {
				t.Fatalf("surfaces %v: got prefix %q raw %q", c.surfaces, prefix, raw)
			}
			if len(raw) != len(c.prefix)+tokenRandomLength {
				t.Fatalf("token length %d: %q", len(raw), raw)
			}
			for _, ch := range raw[len(prefix):] {
				if !strings.ContainsRune(base62Alphabet, ch) {
					t.Fatalf("non-base62 character %q in %q", ch, raw)
				}
			}
			if !validTokenShape(raw) {
				t.Fatalf("validTokenShape rejected %q", raw)
			}
			if seen[raw] {
				t.Fatalf("duplicate token %q", raw)
			}
			seen[raw] = true
		}
	}
	h := hashToken("rl_api_abc")
	if len(h) != 64 || h != hashToken("rl_api_abc") || h == hashToken("rl_api_abd") {
		t.Fatalf("hashToken not a stable sha256 hex: %q", h)
	}
}

func TestValidTokenShapeRejects(t *testing.T) {
	for _, raw := range []string{
		"", "rl_api_", "rl_mcp_short",
		"rl_xyz_" + strings.Repeat("a", 32),
		"rl_api_" + strings.Repeat("a", 31) + "-",
		"rl_api_" + strings.Repeat("a", 33),
		"Bearer rl_api_" + strings.Repeat("a", 32),
	} {
		if validTokenShape(raw) {
			t.Errorf("validTokenShape(%q) = true", raw)
		}
	}
}

func TestAuthenticateToken(t *testing.T) {
	s, clock := newTestService(t)
	ctx := context.Background()
	editor := createTestUser(t, s, "mira", core.RoleEditor, "correct-horse-battery")

	mk := func(scope string, surfaces []string, expires *time.Time, limit []string) string {
		raw, prefix := newAPIToken(surfaces)
		row := &store.APITokenRow{ID: store.NewID(), Name: "home-assistant", Prefix: prefix, Last4: raw[len(raw)-4:], Hash: hashToken(raw),
			Scope: scope, Surfaces: surfaces, LimitTo: limit, ExpiresAt: expires, CreatedBy: editor.ID, CreatedAt: clock.now()}
		if err := s.app.Store.CreateAPIToken(ctx, row); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	write := mk(core.ScopeWrite, []string{"mcp", "rest"}, nil, []string{"*.home.lan", "web-app"})
	a, err := s.AuthenticateToken(ctx, write, SurfaceREST)
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != core.ActorToken || a.Role != core.RoleEditor || a.Scope != core.ScopeWrite || a.TokenID == "" || a.Name != "home-assistant" {
		t.Fatalf("unexpected actor %+v", a)
	}
	if got := TokenLimits(ctx, s.app.Store, a.TokenID); len(got) != 2 || got[0] != "*.home.lan" {
		t.Fatalf("TokenLimits = %v", got)
	}
	if a, err := s.AuthenticateToken(ctx, write, SurfaceMCP); err != nil || a.Type != core.ActorMCP {
		t.Fatalf("mcp surface: %+v %v", a, err)
	}

	restOnly := mk(core.ScopeRead, []string{"rest"}, nil, nil)
	if _, err := s.AuthenticateToken(ctx, restOnly, SurfaceMCP); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("rest-only token accepted on mcp: %v", err)
	}
	if a, err := s.AuthenticateToken(ctx, restOnly, SurfaceREST); err != nil || a.Role != core.RoleViewer || a.CanWrite() {
		t.Fatalf("read token: %+v %v", a, err)
	}

	// Unknown and tampered tokens.
	if _, err := s.AuthenticateToken(ctx, PrefixAPI+strings.Repeat("A", 32), SurfaceREST); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("unknown token: %v", err)
	}

	// Expiry.
	exp := clock.now().Add(time.Hour)
	expiring := mk(core.ScopeRead, []string{"rest"}, &exp, nil)
	if _, err := s.AuthenticateToken(ctx, expiring, SurfaceREST); err != nil {
		t.Fatalf("not yet expired: %v", err)
	}
	clock.advance(2 * time.Hour)
	if _, err := s.AuthenticateToken(ctx, expiring, SurfaceREST); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("expired token accepted: %v", err)
	}

	// Revocation.
	if err := s.app.Store.RevokeAPIToken(ctx, a.TokenID, clock.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateToken(ctx, write, SurfaceREST); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked token accepted: %v", err)
	}

	// Creator demoted to viewer → read only; disabled → rejected.
	fresh := mk(core.ScopeWrite, []string{"rest"}, nil, nil)
	editor.Role = core.RoleViewer
	if err := s.app.Store.UpdateUser(ctx, editor); err != nil {
		t.Fatal(err)
	}
	if a, err := s.AuthenticateToken(ctx, fresh, SurfaceREST); err != nil || a.Scope != core.ScopeRead {
		t.Fatalf("viewer-owned write token: %+v %v", a, err)
	}
	editor.Disabled = true
	if err := s.app.Store.UpdateUser(ctx, editor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateToken(ctx, fresh, SurfaceREST); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("token of disabled user accepted: %v", err)
	}
}
