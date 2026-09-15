package acme

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

type handlers struct {
	app *core.App
	svc *Service
}

// Routes registers the certs slice API: certificates, DNS providers, access lists, streams.
func Routes(app *core.App, r chi.Router) {
	svc, ok := app.Certs.(*Service)
	if !ok || svc == nil {
		svc = New(app)
	}
	h := &handlers{app: app, svc: svc}
	h.registerCertHooks()
	h.registerDNSHooks()
	h.registerAccessHooks()
	h.registerStreamHooks()
	httpx.SettingsHooks[model.SettingsTLS] = &httpx.SettingsHook{
		BeforeSave: func(r *http.Request, prev, next any) error {
			if t, ok := next.(*model.TLSSettings); ok {
				t.Email = strings.TrimSpace(t.Email)
			}
			return nil
		},
	}

	r.Post("/certificates/request", h.requestCert)
	r.Post("/certificates/upload", h.uploadCert)
	r.Get("/certificates/rate-limit", h.rateLimit)
	r.Post("/certificates/{id}/renew", h.renewCert)
	r.Post("/certificates/{id}/revoke", h.revokeCert)
	r.Get("/certificates/{id}/download", h.downloadCert)
	r.Get("/certificates/{id}/usage", h.certUsage)
	r.Post("/certificates/{id}/test", h.testCert)

	r.Get("/dns-providers/types", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, model.DNSProviderTypes)
	})
	r.Post("/dns-providers/test", h.testDraftDNS)
	r.Post("/dns-providers/{id}/test", h.testDNS)

	h.accessRoutes(r)
	h.streamRoutes(r)
}

// ---------------------------------------------------------------- certificates

type certView struct {
	model.Certificate
	Renewing bool `json:"renewing,omitempty"`
}

func (h *handlers) registerCertHooks() {
	httpx.CertificateHooks.BeforeSave = func(r *http.Request, prev, next *model.Certificate) error {
		if prev == nil {
			return httpx.Errorf(http.StatusMethodNotAllowed, "method_not_allowed", "use /api/certificates/request or /upload")
		}
		merged := *prev
		if name := strings.TrimSpace(next.Name); name != "" {
			merged.Name = name
		}
		merged.AutoRenew = next.AutoRenew && model.IsACMEProvider(prev.Provider)
		*next = merged
		return nil
	}
	httpx.CertificateHooks.AfterSave = func(r *http.Request, prev, next *model.Certificate) {
		h.svc.publish(next.ID, next.Status)
	}
	httpx.CertificateHooks.BeforeDelete = func(r *http.Request, cur *model.Certificate) error {
		if h.svc.isInflight(cur.ID) {
			return httpx.Errorf(http.StatusConflict, "in_progress", "Wait for the running order to finish before deleting "+cur.Name)
		}
		u, err := h.certUsageFor(r.Context(), cur.ID)
		if err != nil {
			return err
		}
		if names := u.names(); len(names) > 0 {
			return httpx.InUse("Certificate "+cur.Name, names)
		}
		return nil
	}
	httpx.CertificateHooks.AfterDelete = func(r *http.Request, cur *model.Certificate) {
		h.svc.removeCertFiles(cur.ID)
		h.svc.publish(cur.ID, "deleted")
	}
	httpx.CertificateHooks.DecorateList = func(r *http.Request, items []model.Certificate) any {
		out := make([]certView, len(items))
		for i, c := range items {
			if c.History == nil {
				c.History = []model.CertEvent{}
			}
			out[i] = certView{Certificate: c, Renewing: h.svc.IsRenewing(c.ID)}
		}
		return out
	}
}

func (s *Service) isInflight(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[id]
}

func (h *handlers) requestCert(w http.ResponseWriter, r *http.Request) {
	var req core.CertRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cert, err := h.svc.Request(r.Context(), req)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, cert)
}

func (h *handlers) renewCert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.svc.Renew(r.Context(), id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cert, err := h.app.Store.Certificates().Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, certView{Certificate: *cert, Renewing: h.svc.IsRenewing(id)})
}

func (h *handlers) revokeCert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cert, err := h.app.Store.Certificates().Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !model.IsACMEProvider(cert.Provider) || cert.NotAfter == nil {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "not_revocable", "Only issued Let's Encrypt certificates can be revoked"))
		return
	}
	if h.svc.isInflight(id) {
		httpx.Fail(w, r, errInProgress)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := h.svc.revoke(ctx, cert); err != nil {
		msg := humanizeACMEError("", nil, err, 0)
		msg = strings.TrimPrefix(msg, ": ")
		h.app.Audit(r.Context(), core.AuditEntry{Action: "certificate.revoke", Target: cert.Name, Detail: msg, Result: "failed"})
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadGateway, "revoke_failed", "Revocation failed: "+msg))
		return
	}
	now := time.Now()
	updated, err := h.svc.updateCert(r.Context(), id, func(c *model.Certificate) {
		c.Status = model.CertStatusFailed
		c.AutoRenew = false
		c.LastError = "Revoked on " + now.Format("2006-01-02") + " — request a new certificate"
		c.History = append(c.History, model.CertEvent{At: now, Message: "Revoked at " + providerLabel(c.Provider), Result: "failed"})
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.svc.publish(id, updated.Status)
	h.app.Audit(r.Context(), core.AuditEntry{Action: "certificate.revoke", Target: cert.Name, Result: "ok"})
	h.app.Activity(r.Context(), "cert.revoked", "warn", "Certificate revoked: "+cert.Name, cert.Name, providerLabel(cert.Provider))
	httpx.WriteJSON(w, http.StatusOK, updated)
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (h *handlers) downloadCert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cert, err := h.app.Store.Certificates().Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	part := r.URL.Query().Get("part")
	if part == "" {
		part = "fullchain"
	}
	chainPath, keyPath := h.svc.env.CertPaths(id)
	var data []byte
	switch part {
	case "fullchain", "cert", "chain":
		data, err = os.ReadFile(chainPath)
		if err == nil && part != "fullchain" {
			certs, perr := parseCertificates(data)
			if perr != nil {
				httpx.Fail(w, r, perr)
				return
			}
			certs = orderChain(certs)
			if part == "cert" {
				certs = certs[:1]
			} else {
				certs = certs[1:]
			}
			data = encodeCerts(certs)
		}
	case "key":
		if !httpx.Actor(r).IsAdmin() {
			httpx.WriteError(w, http.StatusForbidden, "admin_only", "only admins can download private keys")
			return
		}
		data, err = os.ReadFile(keyPath)
		if err == nil {
			h.app.Audit(r.Context(), core.AuditEntry{Action: "certificate.download_key", Target: cert.Name, Result: "ok"})
		}
	default:
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "part must be fullchain, cert, chain or key")
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "certificate files are not available yet")
		return
	}
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	name := unsafeFileChars.ReplaceAllString(strings.ReplaceAll(cert.Name, "*", "wildcard"), "_")
	suffix := map[string]string{"fullchain": "fullchain", "cert": "cert", "chain": "chain", "key": "privkey"}[part]
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.pem"`, name, suffix))
	w.Write(data)
}

type hostRef struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
}

type certUsage struct {
	Hosts       []hostRef `json:"hosts"`
	Redirects   []hostRef `json:"redirects"`
	DefaultHost bool      `json:"defaultHost"`
	Defaults    []string  `json:"defaults"` // settings that reference the certificate
}

func (u certUsage) names() []string {
	var out []string
	for _, x := range u.Hosts {
		out = append(out, x.Domain)
	}
	for _, x := range u.Redirects {
		out = append(out, "redirect "+x.Domain)
	}
	if u.DefaultHost {
		out = append(out, "the default host")
	}
	out = append(out, u.Defaults...)
	return out
}

func firstDomain(ds []string) string {
	if len(ds) == 0 {
		return ""
	}
	return ds[0]
}

func (h *handlers) certUsageFor(ctx context.Context, id string) (certUsage, error) {
	u := certUsage{Hosts: []hostRef{}, Redirects: []hostRef{}, Defaults: []string{}}
	hosts, err := h.app.Store.Hosts().List(ctx)
	if err != nil {
		return u, err
	}
	for _, x := range hosts {
		if x.CertificateID == id {
			u.Hosts = append(u.Hosts, hostRef{ID: x.ID, Domain: firstDomain(x.Domains)})
		}
	}
	redirects, err := h.app.Store.Redirects().List(ctx)
	if err != nil {
		return u, err
	}
	for _, x := range redirects {
		if x.CertificateID == id {
			u.Redirects = append(u.Redirects, hostRef{ID: x.ID, Domain: firstDomain(x.Domains) + x.FromPath})
		}
	}
	dh, err := store.LoadSettings[model.DefaultHostSettings](ctx, h.app.Store, model.SettingsDefaultHost)
	if err != nil {
		return u, err
	}
	u.DefaultHost = dh.CertificateID == id
	if g, err := store.LoadSettings[model.GeneralSettings](ctx, h.app.Store, model.SettingsGeneral); err == nil && g.Defaults.CertificateID == id {
		u.Defaults = append(u.Defaults, "new host defaults")
	}
	if d, err := store.LoadSettings[model.DockerSettings](ctx, h.app.Store, model.SettingsDocker); err == nil && d.DefaultCertID == id {
		u.Defaults = append(u.Defaults, "Docker discovery defaults")
	}
	return u, nil
}

func (h *handlers) certUsage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.app.Store.Certificates().Get(r.Context(), id); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u, err := h.certUsageFor(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func readFormPart(r *http.Request, name string) ([]byte, error) {
	if f, _, err := r.FormFile(name); err == nil {
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, 1<<20))
	} else if !errors.Is(err, http.ErrMissingFile) && !errors.Is(err, multipart.ErrMessageTooLarge) {
		if v := r.FormValue(name); v != "" {
			return []byte(v), nil
		}
	}
	return []byte(r.FormValue(name)), nil
}

func (h *handlers) uploadCert(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "bad_request", "expected multipart form with certificate and privateKey"))
		return
	}
	certPEM, _ := readFormPart(r, "certificate")
	keyPEM, _ := readFormPart(r, "privateKey")
	fields := model.Errs{}
	if len(strings.TrimSpace(string(certPEM))) == 0 {
		fields.Add("certificate", "Paste or choose the certificate (PEM, leaf first)")
	}
	if len(strings.TrimSpace(string(keyPEM))) == 0 {
		fields.Add("privateKey", "Paste or choose the private key (PEM)")
	}
	if len(fields) > 0 {
		httpx.Fail(w, r, fields.Err())
		return
	}
	up, err := ValidateUpload(certPEM, keyPEM, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	repo := h.app.Store.Certificates()
	name := strings.TrimSpace(r.FormValue("name"))

	// replacing an existing custom certificate (manual renewal)
	if id := r.FormValue("id"); id != "" {
		prev, err := repo.Get(ctx, id)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if prev.Provider != model.CertCustom {
			httpx.Fail(w, r, httpx.Errorf(http.StatusBadRequest, "not_custom", "Only custom certificates can be replaced by an upload"))
			return
		}
		chain, key := h.svc.env.CertPaths(id)
		if err := writeCertFiles(chain, key, up.Fullchain, up.Key); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		now := time.Now()
		updated, err := h.svc.updateCert(ctx, id, func(c *model.Certificate) {
			applyInfo(c, up.Info)
			if name != "" {
				c.Name = name
			}
			c.Status = model.CertStatusValid
			c.LastError = ""
			c.History = append(c.History, model.CertEvent{At: now, Message: "Replaced by upload · valid until " + up.Info.NotAfter.Format("2006-01-02"), Result: "ok"})
		})
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		h.app.Audit(ctx, core.AuditEntry{Action: "certificate.upload", Target: updated.Name, Detail: "replaced", Result: "saved"})
		h.app.Changed(ctx, model.KindCertificate, id, updated.Name, core.ActionUpdated)
		if h.svc.certInLiveConfig(ctx, id) && h.app.Nginx != nil {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, _ = h.app.Nginx.Reload(rctx)
			cancel()
		}
		h.svc.publish(id, updated.Status)
		httpx.WriteJSON(w, http.StatusOK, updated)
		return
	}

	if name == "" {
		name = firstNonEmpty(up.Info.Leaf.Subject.CommonName, firstDomain(up.Info.Domains), "custom certificate")
	}
	now := time.Now()
	cert := &model.Certificate{
		Meta:      model.Meta{ID: store.NewID()},
		Name:      name,
		Domains:   up.Info.Domains,
		Provider:  model.CertCustom,
		AutoRenew: false,
		Status:    model.CertStatusValid,
		History:   []model.CertEvent{{At: now, Message: "Uploaded · issued by " + firstNonEmpty(up.Info.Issuer, "unknown issuer"), Result: "ok"}},
	}
	applyInfo(cert, up.Info)
	if err := cert.Validate(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chain, key := h.svc.env.CertPaths(cert.ID)
	if err := writeCertFiles(chain, key, up.Fullchain, up.Key); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := repo.Create(ctx, cert); err != nil {
		h.svc.removeCertFiles(cert.ID)
		httpx.Fail(w, r, err)
		return
	}
	h.app.Audit(ctx, core.AuditEntry{Action: "certificate.upload", Target: cert.Name, Detail: strings.Join(cert.Domains, ", "), Result: "saved"})
	h.app.Changed(ctx, model.KindCertificate, cert.ID, cert.Name, core.ActionCreated)
	h.app.Activity(ctx, "cert.uploaded", "info", "Custom certificate uploaded: "+cert.Name, cert.Name, "Custom · "+up.Info.Issuer)
	h.svc.publish(cert.ID, cert.Status)
	httpx.WriteJSON(w, http.StatusCreated, cert)
}

func (h *handlers) rateLimit(w http.ResponseWriter, r *http.Request) {
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "domain is required")
		return
	}
	recs := h.svc.loadIssuances(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"limit":            50,
		"used":             countIssuances(recs, domain, time.Now()),
		"registeredDomain": registeredDomain(domain),
		"windowDays":       7,
	})
}

type certTestResult struct {
	OK                bool       `json:"ok"`
	Matches           bool       `json:"matches"`
	Address           string     `json:"address"`
	SNI               string     `json:"sni"`
	ServedFingerprint string     `json:"servedFingerprint,omitempty"`
	ServedSubject     string     `json:"servedSubject,omitempty"`
	ServedIssuer      string     `json:"servedIssuer,omitempty"`
	ServedNotAfter    *time.Time `json:"servedNotAfter,omitempty"`
	HostUsesCert      bool       `json:"hostUsesCert"`
	Error             string     `json:"error,omitempty"`
}

func (h *handlers) testCert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		HostID string `json:"hostId"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cert, err := h.app.Store.Certificates().Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	host, err := h.app.Store.Hosts().Get(r.Context(), body.HostID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	g, err := store.LoadSettings[model.GeneralSettings](r.Context(), h.app.Store, model.SettingsGeneral)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	sni := ""
	for _, d := range host.Domains {
		if !model.IsWildcardDomain(d) {
			sni = d
			break
		}
	}
	if sni == "" && len(host.Domains) > 0 {
		sni = "relay-test." + strings.TrimPrefix(host.Domains[0], "*.")
	}
	res := certTestResult{Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(g.HTTPSPort)), SNI: sni, HostUsesCert: host.CertificateID == id}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", res.Address, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // we only read the served certificate
	if err != nil {
		res.Error = "TLS handshake with " + res.Address + " failed: " + unwrapURLError(err).Error()
		httpx.WriteJSON(w, http.StatusOK, res)
		return
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		res.Error = "nginx presented no certificate"
		httpx.WriteJSON(w, http.StatusOK, res)
		return
	}
	leaf := state.PeerCertificates[0]
	res.OK = true
	res.ServedFingerprint = fingerprint(leaf.Raw)
	res.ServedSubject = certName(leaf)
	res.ServedIssuer = leaf.Issuer.CommonName
	na := leaf.NotAfter
	res.ServedNotAfter = &na
	res.Matches = cert.Fingerprint != "" && res.ServedFingerprint == cert.Fingerprint
	httpx.WriteJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------- DNS providers

func (h *handlers) registerDNSHooks() {
	httpx.DNSProviderHooks.BeforeSave = func(r *http.Request, prev, next *model.DNSProvider) error {
		list, err := h.app.Store.DNSProviders().List(r.Context())
		if err != nil {
			return err
		}
		for _, p := range list {
			if p.ID != next.ID && strings.EqualFold(p.Name, next.Name) {
				return (model.Errs{"name": "A DNS provider named " + p.Name + " already exists"}).Err()
			}
		}
		if prev != nil && prev.Type == next.Type && mapsEqual(prev.Credentials, next.Credentials) {
			next.Status, next.Zones, next.LastError, next.LastCheckedAt = prev.Status, prev.Zones, prev.LastError, prev.LastCheckedAt
		} else {
			next.Status, next.Zones, next.LastError, next.LastCheckedAt = "unknown", []string{}, "", nil
		}
		return nil
	}
	httpx.DNSProviderHooks.AfterSave = func(r *http.Request, prev, next *model.DNSProvider) {
		if next.Status != "unknown" {
			return
		}
		h.applyDNSTest(r.Context(), next)
		if err := h.app.Store.DNSProviders().Update(r.Context(), next); err != nil {
			h.app.Log.Warn("acme: store dns provider test", "err", err)
		}
	}
	httpx.DNSProviderHooks.BeforeDelete = func(r *http.Request, cur *model.DNSProvider) error {
		certs, err := h.app.Store.Certificates().List(r.Context())
		if err != nil {
			return err
		}
		var names []string
		for _, c := range certs {
			if c.DNSProviderID == cur.ID {
				names = append(names, c.Name)
			}
		}
		if len(names) > 0 {
			return httpx.InUse("DNS provider "+cur.Name, names)
		}
		return nil
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (h *handlers) applyDNSTest(ctx context.Context, p *model.DNSProvider) DNSTestResult {
	res := testDNSProvider(ctx, p)
	now := time.Now().UTC()
	p.Status = res.Status
	p.Zones = res.Zones
	p.LastError = res.Error
	p.LastCheckedAt = &now
	return res
}

func (h *handlers) testDNS(w http.ResponseWriter, r *http.Request) {
	p, err := h.app.Store.DNSProviders().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res := h.applyDNSTest(r.Context(), p)
	if err := h.app.Store.DNSProviders().Update(r.Context(), p); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.app.Audit(r.Context(), core.AuditEntry{Action: "dns_provider.test", Target: p.Name, Detail: firstNonEmpty(res.Error, strings.Join(res.Zones, ", ")), Result: map[bool]string{true: "ok", false: "failed"}[res.Status == "ok"]})
	p.Redact()
	httpx.WriteJSON(w, http.StatusOK, p)
}

// testDraftDNS verifies credentials from the add/edit form before saving.
func (h *handlers) testDraftDNS(w http.ResponseWriter, r *http.Request) {
	var p model.DNSProvider
	if err := httpx.Decode(r, &p); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var prev *model.DNSProvider
	if p.ID != "" {
		var err error
		if prev, err = h.app.Store.DNSProviders().Get(r.Context(), p.ID); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}
	if p.Name == "" {
		p.Name = "draft"
	}
	if err := p.KeepSecrets(prev); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := p.Validate(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, testDNSProvider(r.Context(), &p))
}
