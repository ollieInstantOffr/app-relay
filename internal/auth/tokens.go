package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	// PrefixMCP marks tokens usable only by MCP clients.
	PrefixMCP = "rl_mcp_"
	// PrefixAPI marks tokens usable on the REST API (and MCP when both surfaces are allowed).
	PrefixAPI = "rl_api_"

	tokenRandomLength = 32
	base62Alphabet    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// randomBase62 returns n uniformly random base62 characters (rejection sampling).
func randomBase62(n int) string {
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic("crypto/rand: " + err.Error())
		}
		for _, b := range buf {
			if b >= 248 { // 248 = 62×4, keeps b%62 unbiased
				continue
			}
			out = append(out, base62Alphabet[b%62])
			if len(out) == n {
				break
			}
		}
	}
	return string(out)
}

func tokenPrefixFor(surfaces []string) string {
	if len(surfaces) == 1 && surfaces[0] == SurfaceMCP {
		return PrefixMCP
	}
	return PrefixAPI
}

// newAPIToken returns a raw token for the given surfaces.
func newAPIToken(surfaces []string) (raw, prefix string) {
	prefix = tokenPrefixFor(surfaces)
	return prefix + randomBase62(tokenRandomLength), prefix
}

func validTokenShape(raw string) bool {
	var rest string
	switch {
	case strings.HasPrefix(raw, PrefixMCP):
		rest = raw[len(PrefixMCP):]
	case strings.HasPrefix(raw, PrefixAPI):
		rest = raw[len(PrefixAPI):]
	default:
		return false
	}
	if len(rest) != tokenRandomLength {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if !strings.ContainsRune(base62Alphabet, rune(rest[i])) {
			return false
		}
	}
	return true
}

// AuthenticateToken validates a raw API token for a surface ("mcp" | "rest").
// Write tokens act as editors, read tokens as viewers. A token stops working
// when revoked, expired, or when the user who created it is disabled or
// deleted; tokens created by viewers are limited to read.
func (s *Service) AuthenticateToken(ctx context.Context, raw, surface string) (core.Actor, error) {
	raw = strings.TrimSpace(raw)
	if !validTokenShape(raw) {
		return core.Actor{}, core.ErrUnauthenticated
	}
	h := hashToken(raw)
	t, err := s.app.Store.GetAPITokenByHash(ctx, h)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return core.Actor{}, core.ErrUnauthenticated
		}
		return core.Actor{}, err
	}
	if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) != 1 {
		return core.Actor{}, core.ErrUnauthenticated
	}
	now := s.now()
	if !tokenActive(t, now) || !slices.Contains(t.Surfaces, surface) {
		return core.Actor{}, core.ErrUnauthenticated
	}
	scope := t.Scope
	if t.CreatedBy != "" {
		u, err := s.app.Store.GetUser(ctx, t.CreatedBy)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return core.Actor{}, core.ErrUnauthenticated
			}
			return core.Actor{}, err
		}
		if u.Disabled {
			return core.Actor{}, core.ErrUnauthenticated
		}
		if u.Role == core.RoleViewer {
			scope = core.ScopeRead
		}
	}
	s.touchToken(ctx, t.ID, now)
	role := core.RoleViewer
	if scope == core.ScopeWrite {
		role = core.RoleEditor
	}
	typ := core.ActorToken
	if surface == SurfaceMCP {
		typ = core.ActorMCP
	}
	return core.Actor{Type: typ, ID: t.ID, Name: t.Name, Role: role, TokenID: t.ID, Scope: scope}, nil
}

func tokenActive(t *store.APITokenRow, now time.Time) bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || now.Before(*t.ExpiresAt))
}

// touchToken updates last_used_at at most once a minute per token.
func (s *Service) touchToken(ctx context.Context, id string, now time.Time) {
	if v, ok := s.tokenTouch.Load(id); ok && now.Sub(v.(time.Time)) < touchInterval {
		return
	}
	s.tokenTouch.Store(id, now)
	if err := s.app.Store.TouchAPIToken(context.WithoutCancel(ctx), id, now); err != nil {
		s.app.Log.Warn("auth: touch token", "err", err)
	}
}

// TokenLimits returns the "limit to" patterns (domain globs, backend names)
// stored on an API token, for enforcement by the MCP slice. An empty result
// means the token is unrestricted (or unknown).
func TokenLimits(ctx context.Context, st *store.Store, tokenID string) []string {
	t, err := st.GetAPIToken(ctx, tokenID)
	if err != nil {
		return nil
	}
	return t.LimitTo
}

// ---------------------------------------------------------------- HTTP

type tokenDTO struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Last4      string     `json:"last4"`
	Scope      string     `json:"scope"`
	Surfaces   []string   `json:"surfaces"`
	LimitTo    []string   `json:"limitTo"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	Token      string     `json:"token,omitempty"`
}

func toTokenDTO(t *store.APITokenRow, names map[string]string) tokenDTO {
	by := names[t.CreatedBy]
	if by == "" {
		by = t.CreatedBy
	}
	return tokenDTO{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Last4: t.Last4, Scope: t.Scope, Surfaces: t.Surfaces, LimitTo: t.LimitTo,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedBy: by, CreatedAt: t.CreatedAt, RevokedAt: t.RevokedAt}
}

func (s *Service) usernames(ctx context.Context) map[string]string {
	users, err := s.app.Store.ListUsers(ctx)
	out := map[string]string{}
	if err != nil {
		return out
	}
	for _, u := range users {
		out[u.ID] = u.Username
	}
	return out
}

func (s *Service) handleTokensList(w http.ResponseWriter, r *http.Request) {
	a := httpx.Actor(r)
	if a.Type != core.ActorUser {
		httpx.WriteError(w, http.StatusForbidden, "user_only", "sign in as a user to manage tokens")
		return
	}
	owner := a.ID
	if a.IsAdmin() {
		owner = ""
	}
	rows, err := s.app.Store.ListAPITokens(r.Context(), owner)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	surface := r.URL.Query().Get("surface")
	names := s.usernames(r.Context())
	out := []tokenDTO{}
	for i := range rows {
		if surface != "" && !slices.Contains(rows[i].Surfaces, surface) {
			continue
		}
		out = append(out, toTokenDTO(&rows[i], names))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type createTokenRequest struct {
	Name          string   `json:"name"`
	Scope         string   `json:"scope"`
	Surfaces      []string `json:"surfaces"`
	ExpiresInDays int      `json:"expiresInDays"`
	LimitTo       []string `json:"limitTo"`
}

func (s *Service) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	a := httpx.Actor(r)
	if a.Type != core.ActorUser {
		httpx.WriteError(w, http.StatusForbidden, "user_only", "API tokens can't create other tokens")
		return
	}
	var req createTokenRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	e := model.Errs{}
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		e.Add("name", "Name is required")
	case len([]rune(req.Name)) > 64:
		e.Add("name", "At most 64 characters")
	case strings.IndexFunc(req.Name, unicode.IsControl) >= 0:
		e.Add("name", "Control characters aren't allowed")
	}
	if req.Scope != core.ScopeRead && req.Scope != core.ScopeWrite {
		e.Add("scope", "Pick read-only or read + write")
	}
	surfaces := []string{}
	for _, sf := range []string{SurfaceMCP, SurfaceREST} {
		if slices.Contains(req.Surfaces, sf) {
			surfaces = append(surfaces, sf)
		}
	}
	for _, sf := range req.Surfaces {
		if sf != SurfaceMCP && sf != SurfaceREST {
			e.Add("surfaces", "Unknown surface %q", sf)
		}
	}
	if len(surfaces) == 0 {
		e.Add("surfaces", "Pick MCP, REST or both")
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		e.Add("expiresInDays", "Between 0 (never) and 3650 days")
	}
	limit := []string{}
	for i, l := range req.LimitTo {
		l = strings.TrimSpace(l)
		if l == "" || slices.Contains(limit, l) {
			continue
		}
		if len(l) > 253 || strings.IndexFunc(l, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			e.Add("limitTo."+itoa(i), "Use a domain, *.domain or backend name without spaces")
			continue
		}
		limit = append(limit, l)
	}
	if len(limit) > 50 {
		e.Add("limitTo", "At most 50 entries")
	}
	if err := e.Err(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	now := s.now().UTC()
	raw, prefix := newAPIToken(surfaces)
	row := &store.APITokenRow{
		ID: store.NewID(), Name: req.Name, Prefix: prefix, Last4: raw[len(raw)-4:], Hash: hashToken(raw),
		Scope: req.Scope, Surfaces: surfaces, LimitTo: limit, CreatedBy: a.ID, CreatedAt: now,
	}
	if req.ExpiresInDays > 0 {
		exp := now.Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		row.ExpiresAt = &exp
	}
	if err := s.app.Store.CreateAPIToken(r.Context(), row); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	detail := req.Scope + " · " + strings.Join(surfaces, " + ")
	if len(limit) > 0 {
		detail += " · limited to " + strings.Join(limit, ", ")
	}
	s.app.Audit(r.Context(), core.AuditEntry{Action: "token.create", Target: req.Name, Detail: detail})
	dto := toTokenDTO(row, map[string]string{a.ID: a.Name})
	dto.Token = raw
	httpx.WriteJSON(w, http.StatusCreated, dto)
}

func (s *Service) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	a := httpx.Actor(r)
	ctx := r.Context()
	t, err := s.app.Store.GetAPIToken(ctx, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if a.Type != core.ActorUser || (!a.IsAdmin() && t.CreatedBy != a.ID) {
		httpx.Fail(w, r, store.ErrNotFound)
		return
	}
	if tokenActive(t, s.now()) {
		if err := s.app.Store.RevokeAPIToken(ctx, t.ID, s.now()); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "token.revoke", Target: t.Name, Detail: t.Prefix + "…" + t.Last4})
	} else {
		if err := s.app.Store.DeleteAPIToken(ctx, t.ID); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "token.delete", Target: t.Name, Detail: t.Prefix + "…" + t.Last4})
	}
	w.WriteHeader(http.StatusNoContent)
}
