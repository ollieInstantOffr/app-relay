// Package acme implements the certs slice: certificate issuance and renewal
// (Let's Encrypt via lego), custom uploads, DNS providers, access lists and
// streams (see http.go, access.go, streams.go).
package acme

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/store"
)

const (
	defaultCertID       = "_default"
	maxConcurrentIssues = 2
	maxHistory          = 50
	expiringWarnDays    = 14
)

type launchOpts struct {
	renewal      bool
	stagingFirst bool
}

type Service struct {
	app *core.App
	env render.Env

	mu       sync.Mutex
	baseCtx  context.Context
	inflight map[string]bool // cert id → issuing/renewing now
	renewing map[string]bool // subset of inflight: renewal of an existing cert
	sem      chan struct{}
	now      func() time.Time
}

func New(app *core.App) *Service {
	return &Service{
		app:      app,
		env:      render.DefaultEnv(app.Config.DataDir, app.Config.RunDir, app.Config.LogDir),
		baseCtx:  context.Background(),
		inflight: map[string]bool{},
		renewing: map[string]bool{},
		sem:      make(chan struct{}, maxConcurrentIssues),
		now:      time.Now,
	}
}

// Start ensures the default certificate, resumes interrupted orders and runs
// the hourly renewal scheduler.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()
	for _, d := range []string{s.env.CertDir, s.env.ACMEWebroot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			s.app.Log.Warn("acme: create directory", "dir", d, "err", err)
		}
	}
	if err := s.ensureDefaultCert(); err != nil {
		s.app.Log.Error("acme: default certificate", "err", err)
	}
	go s.resumePending(ctx)
	go s.scheduler(ctx)
	return nil
}

// IsRenewing reports whether an existing certificate is being renewed now.
func (s *Service) IsRenewing(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewing[id]
}

// ---------------------------------------------------------------- default certificate

// ensureDefaultCert creates the self-signed placeholder served for unknown
// SNI, regenerating it when fewer than 30 days remain.
func (s *Service) ensureDefaultCert() error {
	chainPath, keyPath := s.env.DefaultCertPaths()
	now := s.now()
	if data, err := os.ReadFile(chainPath); err == nil {
		if _, kerr := os.Stat(keyPath); kerr == nil {
			if info, err := InspectPEM(data); err == nil && info.NotAfter.Sub(now) > 30*24*time.Hour {
				return nil
			}
		}
	}
	chain, key, err := selfSigned("localhost", 365*24*time.Hour, now)
	if err != nil {
		return err
	}
	if err := writeCertFiles(chainPath, keyPath, chain, key); err != nil {
		return err
	}
	s.app.Log.Info("acme: generated self-signed default certificate", "path", chainPath)
	return nil
}

// ---------------------------------------------------------------- request & renew

var (
	errInProgress = httpx.Errorf(http.StatusConflict, "in_progress", "An order for this certificate is already in progress")
)

func normalizeDomains(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range in {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Request creates a pending certificate and issues it in the background.
func (s *Service) Request(ctx context.Context, req core.CertRequest) (*model.Certificate, error) {
	domains := normalizeDomains(req.Domains)
	if req.Challenge != model.ChallengeDNS01 {
		req.DNSProviderID = ""
	}
	if errs := model.ValidateCertRequestFields(domains, req.Challenge, req.DNSProviderID); len(errs) > 0 {
		return nil, errs.Err()
	}
	if req.Challenge == model.ChallengeDNS01 {
		if _, err := s.app.Store.DNSProviders().Get(ctx, req.DNSProviderID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, (model.Errs{"dnsProviderId": "This DNS provider no longer exists"}).Err()
			}
			return nil, err
		}
	}
	tls, err := store.LoadSettings[model.TLSSettings](ctx, s.app.Store, model.SettingsTLS)
	if err != nil {
		return nil, err
	}
	provider := certProviderFor(tls)
	if _, err := serverFor(provider, tls); err != nil {
		return nil, httpx.Errorf(http.StatusBadRequest, "acme_not_configured", err.Error())
	}
	existing, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range existing {
		if c.Status == model.CertStatusPending && sameDomains(c.Domains, domains) {
			if s.isInflight(c.ID) || !model.IsACMEProvider(c.Provider) {
				return nil, httpx.Errorf(http.StatusConflict, "in_progress", "A certificate for "+domains[0]+" is already being requested")
			}
			// Pending but nobody is issuing it (e.g. imported from Nginx Proxy
			// Manager): start that certificate instead of refusing.
			if err := s.launch(ctx, c.ID, launchOpts{renewal: c.NotAfter != nil}); err != nil && !errors.Is(err, errInProgress) {
				return nil, err
			}
			return &c, nil
		}
	}
	cert := &model.Certificate{
		Name:          domains[0],
		Domains:       domains,
		Provider:      provider,
		Challenge:     req.Challenge,
		DNSProviderID: req.DNSProviderID,
		AutoRenew:     req.AutoRenew,
		Status:        model.CertStatusPending,
		History:       []model.CertEvent{},
	}
	if err := cert.Validate(); err != nil {
		return nil, err
	}
	if err := s.app.Store.Certificates().Create(ctx, cert); err != nil {
		return nil, err
	}
	detail := fmt.Sprintf("%s · %s", strings.Join(domains, ", "), challengeLabel(cert.Challenge))
	if req.Staging && provider == model.CertLetsEncrypt {
		detail += " · staging dry run first"
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "certificate.request", Target: cert.Name, Detail: detail, Result: "pending"})
	s.publish(cert.ID, cert.Status)
	if err := s.launch(ctx, cert.ID, launchOpts{stagingFirst: req.Staging && provider == model.CertLetsEncrypt}); err != nil {
		return nil, err
	}
	return cert, nil
}

func sameDomains(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, d := range a {
		set[d] = true
	}
	for _, d := range b {
		if !set[d] {
			return false
		}
	}
	return true
}

// Renew renews an ACME certificate in the background.
func (s *Service) Renew(ctx context.Context, id string) error {
	cert, err := s.app.Store.Certificates().Get(ctx, id)
	if err != nil {
		return err
	}
	if !model.IsACMEProvider(cert.Provider) {
		return httpx.Errorf(http.StatusBadRequest, "not_renewable", "Custom certificates can't be renewed by Relay — upload a new certificate instead")
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "certificate.renew", Target: cert.Name, Detail: "manual", Result: "pending"})
	return s.launch(ctx, id, launchOpts{renewal: cert.NotAfter != nil})
}

// launch starts issuance for id in a goroutine (bounded concurrency).
func (s *Service) launch(ctx context.Context, id string, opts launchOpts) error {
	s.mu.Lock()
	if s.inflight[id] {
		s.mu.Unlock()
		return errInProgress
	}
	s.inflight[id] = true
	if opts.renewal {
		s.renewing[id] = true
	}
	base := s.baseCtx
	s.mu.Unlock()

	actor := core.ActorFrom(ctx)
	if actor.IsZero() {
		actor = core.SystemActor
	}
	bg := core.WithActor(base, actor)
	if opts.renewal {
		// failed/expired certs show a spinner while retrying
		if c, err := s.updateCert(bg, id, func(c *model.Certificate) {
			if c.Status != model.CertStatusValid {
				c.Status = model.CertStatusPending
			}
		}); err == nil {
			s.publish(id, c.Status)
		}
	}
	go s.run(bg, id, opts)
	return nil
}

func (s *Service) finish(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	delete(s.renewing, id)
	s.mu.Unlock()
}

func (s *Service) publish(id, status string) {
	s.app.Bus.Publish(events.CertChanged, map[string]string{"id": id, "status": status})
}

// updateCert re-reads a certificate, applies fn and stores it.
func (s *Service) updateCert(ctx context.Context, id string, fn func(c *model.Certificate)) (*model.Certificate, error) {
	repo := s.app.Store.Certificates()
	c, err := repo.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		return nil, err
	}
	fn(c)
	if c.History == nil {
		c.History = []model.CertEvent{}
	}
	if len(c.History) > maxHistory {
		c.History = c.History[len(c.History)-maxHistory:]
	}
	if err := repo.Update(context.WithoutCancel(ctx), c); err != nil {
		return nil, err
	}
	return c, nil
}

// run performs one issuance or renewal.
func (s *Service) run(ctx context.Context, id string, opts launchOpts) {
	defer s.finish(id)
	defer func() {
		if r := recover(); r != nil {
			s.app.Log.Error("acme: issuance panic", "cert", id, "panic", r)
		}
	}()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.sem }()

	cert, err := s.app.Store.Certificates().Get(ctx, id)
	if err != nil {
		return
	}
	if !model.IsACMEProvider(cert.Provider) {
		return
	}
	tls, err := store.LoadSettings[model.TLSSettings](ctx, s.app.Store, model.SettingsTLS)
	if err != nil {
		s.app.Log.Error("acme: load tls settings", "err", err)
		return
	}
	renewal := cert.NotAfter != nil
	started := s.now()
	var events []model.CertEvent
	srv, err := serverFor(cert.Provider, tls)
	if err != nil {
		s.fail(ctx, cert, renewal, err.Error(), started, nil)
		return
	}

	if opts.stagingFirst {
		dry := *cert
		staging, _ := serverFor(model.CertLetsEncryptStaging, tls)
		out, err := s.obtain(ctx, &dry, staging)
		if err != nil {
			s.fail(ctx, cert, renewal, humanizeACMEError(cert.Challenge, cert.Domains, err, out.Propagation), started, events)
			return
		}
		events = append(events, model.CertEvent{At: s.now(), Message: "Staging dry run passed · " + out.Via, Result: "ok", DurationMs: s.now().Sub(started).Milliseconds()})
	}

	attemptStart := s.now()
	out, err := s.obtain(ctx, cert, srv)
	if err != nil {
		s.fail(ctx, cert, renewal, humanizeACMEError(cert.Challenge, cert.Domains, err, out.Propagation), started, events)
		return
	}
	fullchain, key := s.env.CertPaths(id)
	if err := writeCertFiles(fullchain, key, out.Resource.Certificate, out.Resource.PrivateKey); err != nil {
		s.fail(ctx, cert, renewal, "Could not write certificate files: "+err.Error(), started, events)
		return
	}
	now := s.now()
	if cert.Provider == model.CertLetsEncrypt {
		s.recordIssuance(ctx, cert.Domains, now)
	}
	_ = s.app.Store.PutKV(context.WithoutCancel(ctx), "acme:certacct:"+id, []byte(out.AccountKey))

	issuer := providerLabel(cert.Provider)
	if cert.Provider == model.CertACME {
		issuer = directoryHost(srv.Directory)
	}
	if out.Info.Issuer != "" {
		issuer += " " + out.Info.Issuer
	}
	msg := "Issued by " + issuer + " · " + out.Via
	if renewal {
		msg = "Renewed · " + out.Via + " · valid until " + out.Info.NotAfter.Format("2006-01-02")
	}
	events = append(events, model.CertEvent{At: now, Message: msg, Result: "ok", DurationMs: now.Sub(attemptStart).Milliseconds()})
	updated, err := s.updateCert(ctx, id, func(c *model.Certificate) {
		applyInfo(c, out.Info)
		c.Domains = cert.Domains // keep the requested order
		c.Status = model.CertStatusValid
		c.LastError = ""
		c.History = append(c.History, events...)
	})
	if err != nil {
		s.app.Log.Error("acme: store issued certificate", "cert", id, "err", err)
		return
	}
	s.publish(id, updated.Status)
	detail := providerLabel(cert.Provider) + " · " + challengeLabel(cert.Challenge)
	if renewal {
		if pc, engine := s.app.Proxy(ctx); s.certInLiveConfig(ctx, id) && pc != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			resp, rerr := pc.Reload(rctx)
			cancel()
			if rerr != nil || (resp != nil && !resp.OK) {
				out := ""
				if resp != nil {
					out = resp.Output
				}
				s.app.Log.Warn("acme: reload after renewal", "cert", id, "err", rerr, "output", out)
				s.app.Activity(ctx, "cert.reload_failed", "warn", core.ProxyEngineLabel(engine)+" reload after renewing "+updated.Name+" failed", updated.Name, firstNonEmpty(out, errString(rerr)))
			}
		}
		s.app.Activity(ctx, "cert.renewed", "ok", "Certificate renewed for "+updated.Name, updated.Name, detail)
		s.app.Audit(ctx, core.AuditEntry{Action: "certificate.renewed", Target: updated.Name, Detail: msg, Result: "ok"})
	} else {
		s.app.Changed(ctx, model.KindCertificate, id, updated.Name, core.ActionCreated)
		s.app.Activity(ctx, "cert.issued", "ok", "Certificate issued for "+updated.Name, updated.Name, detail)
		s.app.Audit(ctx, core.AuditEntry{Action: "certificate.issued", Target: updated.Name, Detail: msg, Result: "ok"})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) fail(ctx context.Context, cert *model.Certificate, renewal bool, msg string, started time.Time, prior []model.CertEvent) {
	now := s.now()
	result := "failed"
	if renewal && cert.AutoRenew {
		result = "retried" // the scheduler retries with backoff
	}
	ev := model.CertEvent{At: now, Message: msg, Result: result, DurationMs: now.Sub(started).Milliseconds()}
	updated, err := s.updateCert(ctx, cert.ID, func(c *model.Certificate) {
		c.Status = model.CertStatusFailed
		c.LastError = msg
		c.History = append(c.History, prior...)
		c.History = append(c.History, ev)
	})
	if err != nil {
		s.app.Log.Error("acme: store failed certificate", "cert", cert.ID, "err", err)
		return
	}
	s.app.Log.Warn("acme: certificate order failed", "cert", cert.Name, "err", msg)
	s.publish(cert.ID, updated.Status)
	title := "Certificate request failed"
	if renewal {
		title = "Certificate renewal failed"
	}
	if s.app.Notify != nil {
		s.app.Notify.Notify(ctx, core.Notification{Event: model.EventCertRenewFailed, Level: "error", Title: title + " · " + cert.Name, Message: msg, URL: "/certificates?cert=" + cert.ID})
	}
	s.app.Activity(ctx, "cert.failed", "warn", title+" for "+cert.Name, cert.Name, msg)
	s.app.Audit(ctx, core.AuditEntry{Action: "certificate.failed", Target: cert.Name, Detail: msg, Result: "failed"})
}

// certInLiveConfig reports whether the live config version references id.
func (s *Service) certInLiveConfig(ctx context.Context, id string) bool {
	snap, err := s.app.Store.CertsLiveSnapshot(ctx)
	if err != nil {
		return false
	}
	if snap.DefaultHost.CertificateID == id {
		return true
	}
	for _, h := range snap.Hosts {
		if h.CertificateID == id {
			return true
		}
	}
	for _, r := range snap.Redirects {
		if r.CertificateID == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- scheduler

func (s *Service) resumePending(ctx context.Context) {
	certs, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return
	}
	for _, c := range certs {
		if c.Status == model.CertStatusPending && model.IsACMEProvider(c.Provider) {
			_ = s.launch(core.WithActor(ctx, core.SystemActor), c.ID, launchOpts{renewal: c.NotAfter != nil})
		}
	}
}

func (s *Service) scheduler(ctx context.Context) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	// Pending certificates created outside Request (imports, restores) start
	// within a minute instead of waiting for the hourly check.
	pending := time.NewTicker(pendingSweepEvery)
	defer pending.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.checkAll(core.WithActor(ctx, core.SystemActor))
			timer.Reset(time.Hour)
		case <-pending.C:
			s.resumePending(ctx)
		}
	}
}

const pendingSweepEvery = time.Minute

// retryDelay is the backoff after n consecutive failed renewals.
func retryDelay(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := time.Hour << (n - 1)
	if n > 6 || d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

// consecutiveFailures counts trailing failed attempts and returns the time of
// the last attempt.
func consecutiveFailures(h []model.CertEvent) (int, time.Time) {
	n := 0
	var last time.Time
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Result == "ok" {
			break
		}
		if last.IsZero() {
			last = h[i].At
		}
		n++
	}
	return n, last
}

func (s *Service) checkAll(ctx context.Context) {
	if err := s.ensureDefaultCert(); err != nil {
		s.app.Log.Error("acme: default certificate", "err", err)
	}
	tls, err := store.LoadSettings[model.TLSSettings](ctx, s.app.Store, model.SettingsTLS)
	if err != nil {
		return
	}
	certs, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, c := range certs {
		s.mu.Lock()
		busy := s.inflight[c.ID]
		s.mu.Unlock()
		if busy {
			continue
		}
		acme := model.IsACMEProvider(c.Provider)
		if c.Status == model.CertStatusPending && acme {
			_ = s.launch(ctx, c.ID, launchOpts{renewal: c.NotAfter != nil})
			continue
		}
		if c.NotAfter == nil {
			continue
		}
		left := c.NotAfter.Sub(now)
		if left <= 0 && c.Status != model.CertStatusExpired && c.Status != model.CertStatusPending {
			if upd, err := s.updateCert(ctx, c.ID, func(x *model.Certificate) {
				x.Status = model.CertStatusExpired
				x.History = append(x.History, model.CertEvent{At: now, Message: "Certificate expired", Result: "failed"})
			}); err == nil {
				c = *upd
				s.publish(c.ID, c.Status)
				s.app.Activity(ctx, "cert.expired", "error", "Certificate expired: "+c.Name, c.Name, providerLabel(c.Provider))
				s.notifyExpiring(ctx, &c, now, true)
			}
		}
		if acme && c.AutoRenew && left <= time.Duration(tls.RenewDaysBefore)*24*time.Hour {
			n, last := consecutiveFailures(c.History)
			backingOff := (c.Status == model.CertStatusFailed || c.Status == model.CertStatusExpired) && n > 0 && now.Sub(last) < retryDelay(n)
			if !backingOff {
				_ = s.launch(ctx, c.ID, launchOpts{renewal: true})
				continue
			}
		}
		if left > 0 && left < expiringWarnDays*24*time.Hour && (!c.AutoRenew || !acme || c.Status == model.CertStatusFailed) {
			s.notifyExpiring(ctx, &c, now, false)
		}
	}
}

// notifyExpiring sends EventCertExpiring at most once per certificate per day.
func (s *Service) notifyExpiring(ctx context.Context, c *model.Certificate, now time.Time, expired bool) {
	key := "acme:expiring-notified:" + c.ID
	day := now.UTC().Format("2006-01-02")
	if raw, err := s.app.Store.GetKV(ctx, key); err == nil && string(raw) == day {
		return
	}
	_ = s.app.Store.PutKV(ctx, key, []byte(day))
	if s.app.Notify == nil {
		return
	}
	days := int(c.NotAfter.Sub(now).Hours() / 24)
	title := fmt.Sprintf("Certificate expires in %d days · %s", days, c.Name)
	msg := "Auto-renew is off."
	switch {
	case expired:
		title = "Certificate expired · " + c.Name
		msg = "Hosts using it now show browser warnings."
	case !model.IsACMEProvider(c.Provider):
		msg = "Custom certificate — upload a renewed one."
	case c.Status == model.CertStatusFailed:
		msg = "Automatic renewal is failing: " + c.LastError
	}
	s.app.Notify.Notify(ctx, core.Notification{Event: model.EventCertExpiring, Level: "warn", Title: title, Message: msg, URL: "/certificates?cert=" + c.ID})
}

// removeCertFiles deletes a certificate's directory (never the default cert).
func (s *Service) removeCertFiles(id string) {
	if id == "" || id == defaultCertID || strings.ContainsAny(id, `/\.`) {
		return
	}
	dir := filepath.Join(s.env.CertDir, id)
	if err := os.RemoveAll(dir); err != nil {
		s.app.Log.Warn("acme: remove certificate files", "dir", dir, "err", err)
	}
}
