package auth

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func (s *Service) registerSettingsHooks() {
	httpx.SettingsHooks[model.SettingsGeneral] = &httpx.SettingsHook{
		BeforeSave: s.generalBeforeSave, AfterSave: s.generalAfterSave, Decorate: s.generalDecorate,
	}
	httpx.SettingsHooks[model.SettingsSecurity] = &httpx.SettingsHook{
		BeforeSave: s.securityBeforeSave, AfterSave: s.securityAfterSave,
	}
}

// ---------------------------------------------------------------- general

func (s *Service) generalDecorate(r *http.Request, v any) any {
	if g, ok := v.(*model.GeneralSettings); ok {
		if ip := s.cachedPublicIP(); ip != "" {
			g.PublicIP = ip
		}
		s.refreshPublicIPAsync()
	}
	return v
}

func adminDomainError(d string) string {
	if strings.Contains(d, "*") {
		return "Use a single domain, not a wildcard"
	}
	return model.HostDomainError(d)
}

func firstDomain(h model.ProxyHost) string {
	if len(h.Domains) == 0 {
		return h.ID
	}
	return h.Domains[0]
}

// adminDomainConflict returns the name of a non-system host already serving domain.
func (s *Service) adminDomainConflict(ctx context.Context, domain string) (string, error) {
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return "", err
	}
	for _, h := range hosts {
		if h.System {
			continue
		}
		for _, d := range h.Domains {
			if strings.EqualFold(d, domain) {
				return firstDomain(h), nil
			}
		}
	}
	return "", nil
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

func (s *Service) generalBeforeSave(r *http.Request, prev, next any) error {
	p, ok1 := prev.(*model.GeneralSettings)
	n, ok2 := next.(*model.GeneralSettings)
	if !ok1 || !ok2 {
		return nil
	}
	ctx := r.Context()
	n.SetupDone = p.SetupDone // only the setup wizard finishes setup
	n.PublicIP = p.PublicIP   // detected, read-only
	if ip := s.cachedPublicIP(); ip != "" {
		n.PublicIP = ip
	}
	n.InstanceName = strings.TrimSpace(n.InstanceName)
	n.AdminDomain = model.HostNormalizeDomain(n.AdminDomain)
	n.Timezone = strings.TrimSpace(n.Timezone)
	n.LANCIDR = strings.TrimSpace(n.LANCIDR)
	n.Defaults.AccessListID = strings.TrimSpace(n.Defaults.AccessListID)
	n.Defaults.CertificateID = strings.TrimSpace(n.Defaults.CertificateID)
	n.ProxyEngine = strings.ToLower(strings.TrimSpace(n.ProxyEngine))
	if n.ProxyEngine == "" {
		n.ProxyEngine = "nginx"
	}

	e := model.Errs{}
	switch {
	case n.InstanceName == "":
		e.Add("instanceName", "Instance name is required")
	case len([]rune(n.InstanceName)) > 64:
		e.Add("instanceName", "At most 64 characters")
	case strings.IndexFunc(n.InstanceName, unicode.IsControl) >= 0:
		e.Add("instanceName", "Control characters aren't allowed")
	}
	if n.AdminDomain != "" {
		if msg := adminDomainError(n.AdminDomain); msg != "" {
			e.Add("adminDomain", "%s", msg)
		} else if name, err := s.adminDomainConflict(ctx, n.AdminDomain); err != nil {
			return err
		} else if name != "" {
			e.Add("adminDomain", "Already used by proxy host %s", name)
		}
	}
	if n.Timezone == "" {
		e.Add("timezone", "Pick a timezone")
	} else if _, err := time.LoadLocation(n.Timezone); err != nil || n.Timezone == "Local" {
		e.Add("timezone", "Unknown timezone — use an IANA name like Europe/Berlin")
	}
	for field, port := range map[string]int{"httpPort": n.HTTPPort, "httpsPort": n.HTTPSPort, "adminPort": n.AdminPort} {
		if !validPort(port) {
			e.Add(field, "Port must be between 1 and 65535")
		}
	}
	if n.HTTPPort == n.HTTPSPort {
		e.Add("httpsPort", "HTTP and HTTPS need different ports")
	}
	if n.AdminPort == n.HTTPPort || n.AdminPort == n.HTTPSPort {
		e.Add("adminPort", "The admin UI port can't be one of the proxy ports")
	} else if n.AdminPort != p.AdminPort && validPort(n.AdminPort) && s.app.AdminListener != nil {
		if err := s.app.AdminListener.CheckPort(n.AdminPort); err != nil {
			e.Add("adminPort", "%s", err.Error())
		}
	}
	if n.LANCIDR != "" {
		if pfx, err := netip.ParsePrefix(n.LANCIDR); err != nil {
			e.Add("lanCidr", "Enter a network like 192.168.1.0/24")
		} else if pfx.Masked() != pfx {
			e.Add("lanCidr", "Host bits are set — did you mean %s?", pfx.Masked())
		}
	}
	switch n.ProxyEngine {
	case "nginx", "edge":
		// Both engines render the same stored configuration, so switching in
		// either direction keeps everything; custom nginx snippets are simply
		// skipped by Relay Edge and apply again with nginx.
	default:
		e.Add("proxyEngine", "Pick nginx or Relay Edge")
	}
	if id := n.Defaults.AccessListID; id != "" {
		if _, err := s.app.Store.AccessLists().Get(ctx, id); errors.Is(err, store.ErrNotFound) {
			e.Add("defaults.accessListId", "That access list no longer exists")
		} else if err != nil {
			return err
		}
	}
	if id := n.Defaults.CertificateID; id != "" {
		if _, err := s.app.Store.Certificates().Get(ctx, id); errors.Is(err, store.ErrNotFound) {
			e.Add("defaults.certificateId", "That certificate no longer exists")
		} else if err != nil {
			return err
		}
	}
	return e.Err()
}

func (s *Service) generalAfterSave(r *http.Request, prev, next any) {
	n, ok := next.(*model.GeneralSettings)
	if !ok {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if _, err := s.syncAdminHost(r, *n, s.security(ctx)); err != nil {
		s.app.Log.Warn("auth: sync admin UI host", "err", err)
		s.app.Activity(ctx, "auth.admin_host", "warn", "Admin UI host not updated", n.AdminDomain, err.Error())
	}
	// Move the admin UI listener. The old port stays open until the new one
	// has been reached, so this can't lock anyone out.
	if p, ok := prev.(*model.GeneralSettings); ok && p.AdminPort != n.AdminPort && n.AdminPort > 0 && s.app.AdminListener != nil {
		if err := s.app.AdminListener.SwitchPort(ctx, n.AdminPort); err != nil {
			s.app.Log.Warn("auth: switch admin UI port", "port", n.AdminPort, "err", err)
			s.app.Activity(ctx, "admin.listen", "warn", "Admin UI port not changed", n.AdminDomain, err.Error())
		}
	}
}

// ---------------------------------------------------------------- security

func (s *Service) securityBeforeSave(r *http.Request, prev, next any) error {
	p, ok1 := prev.(*model.SecuritySettings)
	n, ok2 := next.(*model.SecuritySettings)
	if !ok1 || !ok2 {
		return nil
	}
	ctx := r.Context()
	n.AdminAccessListID = strings.TrimSpace(n.AdminAccessListID)
	e := model.Errs{}
	if n.SessionTTLHours < 1 || n.SessionTTLHours > 8760 {
		e.Add("sessionTtlHours", "Between 1 hour and 365 days")
	}
	if n.LoginMaxAttempts < 1 || n.LoginMaxAttempts > 100 {
		e.Add("loginMaxAttempts", "Between 1 and 100 attempts")
	}
	if n.LoginLockoutMinutes < 1 || n.LoginLockoutMinutes > 1440 {
		e.Add("loginLockoutMinutes", "Between 1 and 1440 minutes")
	}
	if id := n.AdminAccessListID; id != "" {
		al, err := s.app.Store.AccessLists().Get(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			e.Add("adminAccessListId", "That access list no longer exists")
		case err != nil:
			return err
		default:
			ip := core.ClientIP(r)
			addr, perr := netip.ParseAddr(ip)
			if perr != nil || (!addr.Unmap().IsLoopback() && !ipAllowedByRules(al.Rules, addr)) {
				e.Add("adminAccessListId", "Your address %s isn't allowed by %s — saving this would lock you out", ip, al.Name)
			}
		}
	}
	if n.Require2FAForAdmins && !p.Require2FAForAdmins {
		a := httpx.Actor(r)
		if a.Type != core.ActorUser {
			e.Add("require2faForAdmins", "Only a signed-in admin can require 2FA")
		} else if u, err := s.app.Store.GetUser(ctx, a.ID); err != nil {
			return err
		} else if pk, err := s.app.Store.CountWebAuthnCredentials(ctx, u.ID); err != nil {
			return err
		} else if !u.TOTPEnabled && pk == 0 {
			e.Add("require2faForAdmins", "Set up 2FA for your own account first (authenticator app or passkey)")
		}
	}
	return e.Err()
}

func (s *Service) securityAfterSave(r *http.Request, prev, next any) {
	s.invalidate()
	p, ok1 := prev.(*model.SecuritySettings)
	n, ok2 := next.(*model.SecuritySettings)
	if !ok1 || !ok2 || p.AdminAccessListID == n.AdminAccessListID {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	g := s.general(ctx)
	if _, err := s.syncAdminHost(r, g, *n); err != nil {
		s.app.Log.Warn("auth: sync admin UI host", "err", err)
		s.app.Activity(ctx, "auth.admin_host", "warn", "Admin UI host not updated", g.AdminDomain, err.Error())
	}
}

// ---------------------------------------------------------------- admin UI host

// syncAdminHost keeps the system proxy host for general.adminDomain →
// http://127.0.0.1:<adminPort> (websockets on, access list =
// security.adminAccessListId). It creates, updates or deletes the host and
// records pending changes; unchanged hosts are left alone. Fields users may
// tune in the hosts UI (certificate, HTTPS, HTTP/2…) are preserved.
func (s *Service) syncAdminHost(r *http.Request, g model.GeneralSettings, sec model.SecuritySettings) (*model.ProxyHost, error) {
	ctx := context.WithoutCancel(r.Context())
	repo := s.app.Store.Hosts()
	hosts, err := repo.List(ctx)
	if err != nil {
		return nil, err
	}
	var cur *model.ProxyHost
	for i := range hosts {
		if hosts[i].System {
			h := hosts[i]
			cur = &h
			break
		}
	}
	domain := model.HostNormalizeDomain(g.AdminDomain)
	if domain == "" {
		if cur == nil {
			return nil, nil
		}
		if err := repo.Delete(ctx, cur.ID); err != nil {
			return nil, err
		}
		name := firstDomain(*cur)
		s.app.Audit(ctx, core.AuditEntry{Action: "host.delete", Target: name, Detail: "Relay admin UI host · admin domain cleared", Result: "saved"})
		s.app.Changed(ctx, model.KindHost, cur.ID, name, core.ActionDeleted)
		return nil, nil
	}
	for _, h := range hosts {
		if h.System {
			continue
		}
		for _, d := range h.Domains {
			if strings.EqualFold(d, domain) {
				return nil, &model.ValidationError{Fields: map[string]string{"adminDomain": "Already used by proxy host " + firstDomain(h)}}
			}
		}
	}
	port := g.AdminPort
	if port <= 0 {
		port = 8181
	}
	next := model.ProxyHost{
		Enabled: true, HTTP2: true, HSTS: "inherit", Source: model.SourceManual,
		Locations: []model.Location{}, GeoBlock: model.GeoBlock{AllowCountries: []string{}},
	}
	if cur != nil {
		next = *cur
	}
	next.Domains = []string{domain}
	next.Upstream = model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: port}
	next.Websockets = true
	next.AccessListID = sec.AdminAccessListID
	next.System = true
	if n, ok := any(&next).(interface{ Normalize() }); ok {
		n.Normalize()
	}
	if cur != nil && reflect.DeepEqual(*cur, next) {
		return cur, nil
	}
	// The hosts slice's CRUD hooks are not used here: they deliberately refuse
	// domain changes on the system host (those must come through settings).
	if v, ok := any(&next).(model.Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	detail := "Relay admin UI host · http://127.0.0.1:" + itoa(port)
	if cur == nil {
		if err := repo.Create(ctx, &next); err != nil {
			return nil, err
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "host.create", Target: domain, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindHost, next.ID, domain, core.ActionCreated)
	} else {
		if err := repo.Update(ctx, &next); err != nil {
			return nil, err
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "host.update", Target: domain, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindHost, next.ID, domain, core.ActionUpdated)
	}
	return &next, nil
}
