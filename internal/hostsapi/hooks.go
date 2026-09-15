package hostsapi

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// registerHooks customises the generic CRUD for hosts and redirects and the
// default_host settings document. Hooks that another package may already have
// installed are chained (ours run first).
func registerHooks(app *core.App) {
	prevHostSave := httpx.HostHooks.BeforeSave
	httpx.HostHooks.BeforeSave = func(r *http.Request, prev, next *model.ProxyHost) error {
		if err := beforeSaveHost(r.Context(), app, prev, next); err != nil {
			return err
		}
		if prevHostSave != nil {
			return prevHostSave(r, prev, next)
		}
		return nil
	}

	prevHostDelete := httpx.HostHooks.BeforeDelete
	httpx.HostHooks.BeforeDelete = func(r *http.Request, cur *model.ProxyHost) error {
		if err := beforeDeleteHost(r.Context(), app, cur); err != nil {
			return err
		}
		if prevHostDelete != nil {
			return prevHostDelete(r, cur)
		}
		return nil
	}

	prevHostAfterDelete := httpx.HostHooks.AfterDelete
	httpx.HostHooks.AfterDelete = func(r *http.Request, cur *model.ProxyHost) {
		if q := r.URL.Query().Get("deleteCertificate"); (q == "1" || q == "true") && cur.CertificateID != "" {
			deleteOrphanCertificate(r, app, cur.CertificateID, first(cur.Domains))
		}
		if prevHostAfterDelete != nil {
			prevHostAfterDelete(r, cur)
		}
	}

	prevRedirectSave := httpx.RedirectHooks.BeforeSave
	httpx.RedirectHooks.BeforeSave = func(r *http.Request, prev, next *model.Redirect) error {
		if err := beforeSaveRedirect(r.Context(), app, next); err != nil {
			return err
		}
		if prevRedirectSave != nil {
			return prevRedirectSave(r, prev, next)
		}
		return nil
	}

	hook := httpx.SettingsHooks[model.SettingsDefaultHost]
	if hook == nil {
		hook = &httpx.SettingsHook{}
		httpx.SettingsHooks[model.SettingsDefaultHost] = hook
	}
	prevSettingsSave := hook.BeforeSave
	hook.BeforeSave = func(r *http.Request, prev, next any) error {
		if d, ok := next.(*model.DefaultHostSettings); ok {
			if err := beforeSaveDefaultHost(r.Context(), app, d); err != nil {
				return err
			}
		}
		if prevSettingsSave != nil {
			return prevSettingsSave(r, prev, next)
		}
		return nil
	}
}

// prepareHost normalises and fully validates a host (syntax, domain
// uniqueness, references) and returns all field errors at once.
func prepareHost(rf *refs, prev, next *model.ProxyHost) error {
	next.Normalize()
	e := model.Errs{}
	if prev == nil {
		next.System = false
	} else {
		next.System = prev.System
		if prev.System {
			if !slices.Equal(prev.Domains, next.Domains) {
				e.Add("domains", "This is Relay's admin UI host — change its domain in Settings → General")
			}
			if !next.Enabled {
				e.Add("enabled", "Relay's admin UI host can't be disabled")
			}
		}
	}
	if err := mergeInto(e, next.Validate()); err != nil {
		return err
	}
	checkHostRefs(e, rf, next)
	return e.Err()
}

func beforeSaveHost(ctx context.Context, app *core.App, prev, next *model.ProxyHost) error {
	rf, err := loadRefs(ctx, app.Store)
	if err != nil {
		return err
	}
	// Custom nginx snippets are kept with either engine: Relay Edge skips them
	// and they apply again after switching back to nginx.
	return prepareHost(rf, prev, next)
}

func beforeDeleteHost(ctx context.Context, app *core.App, cur *model.ProxyHost) error {
	if cur.System {
		return httpx.Errorf(http.StatusConflict, "system_host",
			"This is Relay's admin UI host and can't be deleted. Change the admin domain in Settings → General instead.")
	}
	dh, err := store.LoadSettings[model.DefaultHostSettings](ctx, app.Store, model.SettingsDefaultHost)
	if err != nil {
		return err
	}
	if dh.Action == model.DefaultHostServe && dh.HostID == cur.ID {
		return httpx.InUse(first(cur.Domains), []string{"the default host setting (Proxy hosts → Default)"})
	}
	return nil
}

func beforeSaveRedirect(ctx context.Context, app *core.App, next *model.Redirect) error {
	next.Normalize()
	e := model.Errs{}
	if err := mergeInto(e, next.Validate()); err != nil {
		return err
	}
	rf, err := loadRefs(ctx, app.Store)
	if err != nil {
		return err
	}
	checkRedirectRefs(e, rf, next)
	return e.Err()
}

func beforeSaveDefaultHost(ctx context.Context, app *core.App, d *model.DefaultHostSettings) error {
	d.Normalize()
	e := model.Errs{}
	if err := mergeInto(e, d.Validate()); err != nil {
		return err
	}
	if d.Action == model.DefaultHostServe && d.HostID != "" {
		if _, err := app.Store.Hosts().Get(ctx, d.HostID); errors.Is(err, store.ErrNotFound) {
			e.Add("hostId", "This host no longer exists")
		} else if err != nil {
			return err
		}
	}
	if d.CertificateID != "" {
		if _, err := app.Store.Certificates().Get(ctx, d.CertificateID); errors.Is(err, store.ErrNotFound) {
			e.Add("certificateId", "This certificate no longer exists")
		} else if err != nil {
			return err
		}
	}
	return e.Err()
}

// deleteOrphanCertificate removes a certificate (document + files) after its
// host was deleted, unless something else still uses it.
func deleteOrphanCertificate(r *http.Request, app *core.App, certID, hostName string) {
	ctx := r.Context()
	users, err := loadCertUsers(ctx, app.Store, certID, "")
	if err != nil {
		app.Log.Error("hosts: certificate usage", "cert", certID, "err", err)
		return
	}
	if len(users) > 0 {
		app.Log.Info("hosts: certificate kept, still in use", "cert", certID, "users", users)
		return
	}
	cert, err := app.Store.Certificates().Get(ctx, certID)
	if err != nil {
		return
	}
	if h := httpx.CertificateHooks.BeforeDelete; h != nil {
		if err := h(r, cert); err != nil {
			app.Log.Warn("hosts: certificate delete refused", "cert", certID, "err", err)
			return
		}
	}
	if err := app.Store.Certificates().Delete(ctx, certID); err != nil {
		app.Log.Error("hosts: delete certificate", "cert", certID, "err", err)
		return
	}
	if safeIDRe.MatchString(certID) && app.Config.DataDir != "" {
		if err := os.RemoveAll(filepath.Join(app.Config.DataDir, "certs", certID)); err != nil {
			app.Log.Warn("hosts: remove certificate files", "cert", certID, "err", err)
		}
	}
	app.Audit(ctx, core.AuditEntry{Action: "certificate.delete", Target: cert.Name, Detail: "deleted together with host " + hostName, Result: "saved"})
	app.Changed(ctx, model.KindCertificate, certID, cert.Name, core.ActionDeleted)
	if h := httpx.CertificateHooks.AfterDelete; h != nil {
		h(r, cert)
	}
}
