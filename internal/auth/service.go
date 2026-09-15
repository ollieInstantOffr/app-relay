// Package auth implements Relay's authentication and account management:
// password + TOTP + passkey sign-in, cookie sessions, REST/MCP API tokens,
// users, the first-run setup wizard, the admin network guard and the general
// and security settings hooks.
package auth

import (
	"context"
	"sync"
	"time"

	_ "time/tzdata" // IANA timezone validation works in minimal containers

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	// CookieName is the session cookie.
	CookieName = "relay_session"
	// MinPasswordLength is the minimum account password length.
	MinPasswordLength = 10

	bcryptCost       = 12
	settingsCacheTTL = 3 * time.Second
	aclCacheTTL      = 5 * time.Second
	touchInterval    = time.Minute

	SurfaceMCP  = "mcp"
	SurfaceREST = "rest"
)

// Service implements core.Auth.
type Service struct {
	app *core.App
	now func() time.Time

	mu    sync.Mutex
	sec   model.SecuritySettings
	secAt time.Time
	acls  map[string]aclEntry

	gates      sync.Map // session id → gateEntry (set by Authenticate, read by the guard)
	tokenTouch sync.Map // token id → time.Time of last last_used_at write
	notices    sync.Map // throttle audit key → time.Time
	requests   sync.Map // user id → time.Time of last access request

	totpMu    sync.Mutex
	totpSteps map[string]int64 // user id → last accepted TOTP time step (replay protection)

	ceremonies *ceremonyStore
	publicIP   publicIPCache
}

type aclEntry struct {
	rules []model.IPRule
	name  string
	found bool
	at    time.Time
}

func New(app *core.App) *Service {
	return &Service{
		app:        app,
		now:        time.Now,
		acls:       map[string]aclEntry{},
		totpSteps:  map[string]int64{},
		ceremonies: newCeremonyStore(),
	}
}

func serviceOf(app *core.App) *Service {
	if app == nil {
		return nil
	}
	s, _ := app.Auth.(*Service)
	return s
}

func (s *Service) Start(ctx context.Context) error {
	go s.housekeeping(ctx)
	return nil
}

func (s *Service) housekeeping(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	s.cleanup(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.cleanup(ctx)
		}
	}
}

func (s *Service) cleanup(ctx context.Context) {
	now := s.now()
	st := s.app.Store
	if err := st.DeleteExpiredSessions(ctx, now); err != nil && ctx.Err() == nil {
		s.app.Log.Warn("auth: prune sessions", "err", err)
	}
	if err := st.PruneLoginAttempts(ctx, now.Add(-7*24*time.Hour), now.Add(-400*24*time.Hour)); err != nil && ctx.Err() == nil {
		s.app.Log.Warn("auth: prune login attempts", "err", err)
	}
	expire := func(m *sync.Map, age time.Duration) {
		m.Range(func(k, v any) bool {
			var at time.Time
			switch x := v.(type) {
			case time.Time:
				at = x
			case gateEntry:
				at = x.at
			}
			if now.Sub(at) > age {
				m.Delete(k)
			}
			return true
		})
	}
	expire(&s.gates, 15*time.Minute)
	expire(&s.tokenTouch, 5*time.Minute)
	expire(&s.notices, 24*time.Hour)
	expire(&s.requests, time.Hour)
	s.ceremonies.prune(now)
}

// security returns the security settings (cached for a few seconds).
func (s *Service) security(ctx context.Context) model.SecuritySettings {
	s.mu.Lock()
	if !s.secAt.IsZero() && s.now().Sub(s.secAt) < settingsCacheTTL {
		v := s.sec
		s.mu.Unlock()
		return v
	}
	s.mu.Unlock()
	v, err := store.LoadSettings[model.SecuritySettings](ctx, s.app.Store, model.SettingsSecurity)
	if err != nil {
		s.app.Log.Warn("auth: load security settings", "err", err)
		v = store.DefaultSecurity()
	}
	normalizeSecurity(&v)
	s.mu.Lock()
	s.sec, s.secAt = v, s.now()
	s.mu.Unlock()
	return v
}

func normalizeSecurity(v *model.SecuritySettings) {
	d := store.DefaultSecurity()
	if v.SessionTTLHours <= 0 {
		v.SessionTTLHours = d.SessionTTLHours
	}
	if v.LoginMaxAttempts <= 0 {
		v.LoginMaxAttempts = d.LoginMaxAttempts
	}
	if v.LoginLockoutMinutes <= 0 {
		v.LoginLockoutMinutes = d.LoginLockoutMinutes
	}
}

func (s *Service) general(ctx context.Context) model.GeneralSettings {
	v, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral)
	if err != nil {
		s.app.Log.Warn("auth: load general settings", "err", err)
		return store.DefaultGeneral()
	}
	return v
}

// invalidate drops cached settings and access lists.
func (s *Service) invalidate() {
	s.mu.Lock()
	s.secAt = time.Time{}
	s.acls = map[string]aclEntry{}
	s.mu.Unlock()
}

func sessionTTL(sec model.SecuritySettings) time.Duration {
	return time.Duration(sec.SessionTTLHours) * time.Hour
}

func lockout(sec model.SecuritySettings) time.Duration {
	return time.Duration(sec.LoginLockoutMinutes) * time.Minute
}
