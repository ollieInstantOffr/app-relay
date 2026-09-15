package hostsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func newTestApp(t *testing.T) (*core.App, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	app := &core.App{
		Config: core.Config{DataDir: dir},
		Store:  st,
		Bus:    events.New(),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	httpx.HostHooks = httpx.Hooks[model.ProxyHost]{}
	httpx.RedirectHooks = httpx.Hooks[model.Redirect]{}
	delete(httpx.SettingsHooks, model.SettingsDefaultHost)
	r := chi.NewRouter()
	Routes(app, r)
	return app, r
}

// saveHost mirrors the generic CRUD order: BeforeSave → Validate → store.
func saveHost(app *core.App, prev, h *model.ProxyHost) error {
	req := httptest.NewRequest(http.MethodPost, "/hosts", nil)
	if err := httpx.HostHooks.BeforeSave(req, prev, h); err != nil {
		return err
	}
	if err := h.Validate(); err != nil {
		return err
	}
	if prev == nil {
		return app.Store.Hosts().Create(req.Context(), h)
	}
	return app.Store.Hosts().Update(req.Context(), h)
}

func call(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func fields(err error) map[string]string {
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		return ve.Fields
	}
	return nil
}

func TestHostsAPIIntegration(t *testing.T) {
	app, r := newTestApp(t)
	ctx := context.Background()
	st := app.Store

	cert := &model.Certificate{Name: "grafana.home.lan", Domains: []string{"grafana.home.lan"}, Provider: model.CertLetsEncrypt, Status: model.CertStatusValid, History: []model.CertEvent{}}
	if err := st.Certificates().Create(ctx, cert); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(app.Config.DataDir, "certs", cert.ID)
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(certDir, "fullchain.pem"), []byte("x"), 0o600)

	// ---- create with normalisation
	grafana := &model.ProxyHost{
		Domains: []string{" Grafana.home.lan "}, Enabled: true, CertificateID: cert.ID, ForceHTTPS: true,
		Upstream:  model.Upstream{Scheme: "http", Host: "10.0.0.21", Port: 3000},
		Locations: []model.Location{{Path: "/api/", Kind: model.LocationProxy, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.22", Port: 8080}}},
	}
	if err := saveHost(app, nil, grafana); err != nil {
		t.Fatalf("create grafana: %v", err)
	}
	if grafana.Domains[0] != "grafana.home.lan" || grafana.HSTS != "inherit" || grafana.Locations[0].ID == "" {
		t.Fatalf("not normalised: %#v", grafana)
	}

	// ---- domain uniqueness across hosts
	dup := &model.ProxyHost{Domains: []string{"grafana.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.9", Port: 80}}
	if f := fields(saveHost(app, nil, dup)); !strings.Contains(f["domains.0"], "host grafana.home.lan") {
		t.Fatalf("expected conflict on domains.0, got %v", f)
	}
	// Updating a host with its own domains is fine.
	again := *grafana
	if err := saveHost(app, grafana, &again); err != nil {
		t.Fatalf("self update: %v", err)
	}

	// ---- system host (created directly, like the auth slice does)
	sys := &model.ProxyHost{Domains: []string{"proxy.home.lan"}, Enabled: true, System: true, Upstream: model.Upstream{Scheme: "http", Host: "127.0.0.1", Port: 8181}, HSTS: "inherit", Source: model.SourceManual}
	if err := st.Hosts().Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	renamed := *sys
	renamed.Domains = []string{"admin.home.lan"}
	if f := fields(saveHost(app, sys, &renamed)); f["domains"] == "" {
		t.Fatalf("system host domain change should be refused: %v", f)
	}

	// ---- check-domain
	rec := call(t, r, http.MethodPost, "/hosts/check-domain", map[string]string{"domain": "GRAFANA.home.lan"})
	cd := decodeBody[checkDomainResp](t, rec)
	if rec.Code != 200 || !cd.Valid || cd.Conflict == nil || cd.Conflict.ID != grafana.ID {
		t.Fatalf("check-domain conflict: %d %s", rec.Code, rec.Body)
	}
	cd = decodeBody[checkDomainResp](t, call(t, r, http.MethodPost, "/hosts/check-domain", map[string]string{"domain": "grafana.home.lan", "excludeId": grafana.ID}))
	if cd.Conflict != nil {
		t.Fatalf("excluded host still conflicts: %#v", cd)
	}
	cd = decodeBody[checkDomainResp](t, call(t, r, http.MethodPost, "/hosts/check-domain", map[string]string{"domain": "bad..lan"}))
	if cd.Valid || cd.Error == "" {
		t.Fatalf("invalid domain accepted: %#v", cd)
	}

	// ---- duplicate
	rec = call(t, r, http.MethodPost, "/hosts/"+grafana.ID+"/duplicate", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
	cp := decodeBody[model.ProxyHost](t, rec)
	if cp.Enabled || cp.Domains[0] != "copy-grafana.home.lan" || cp.ID == grafana.ID || cp.Locations[0].ID == grafana.Locations[0].ID {
		t.Fatalf("duplicate result: %#v", cp)
	}

	// ---- usage: certificate shared with the copy
	usage := decodeBody[usageResp](t, call(t, r, http.MethodGet, "/hosts/"+grafana.ID+"/usage", nil))
	if usage.CertificateName != "grafana.home.lan" || usage.Locations != 1 || len(usage.CertificateSharedWith) != 1 || usage.DefaultHostAction != "close" {
		t.Fatalf("usage: %#v", usage)
	}

	// ---- toggle
	if rec = call(t, r, http.MethodPost, "/hosts/"+sys.ID+"/toggle", map[string]bool{"enabled": false}); rec.Code != http.StatusConflict {
		t.Fatalf("disabling system host: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, r, http.MethodPost, "/hosts/"+cp.ID+"/toggle", map[string]bool{"enabled": true})
	if rec.Code != 200 || !decodeBody[model.ProxyHost](t, rec).Enabled {
		t.Fatalf("enable copy: %d %s", rec.Code, rec.Body)
	}

	// ---- summary
	sum := decodeBody[summaryResp](t, call(t, r, http.MethodGet, "/hosts/summary", nil))
	if sum.All != 3 || sum.NoSSL != 1 || sum.Disabled != 0 {
		t.Fatalf("summary: %#v", sum)
	}

	// ---- bulk
	if rec = call(t, r, http.MethodPost, "/hosts/bulk", bulkReq{IDs: []string{grafana.ID}, Action: bulkAttachAccessList, Value: "missing"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bulk missing list: %d %s", rec.Code, rec.Body)
	}
	if rec = call(t, r, http.MethodPost, "/hosts/bulk", bulkReq{IDs: []string{grafana.ID}, Action: "explode"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bulk bad action: %d", rec.Code)
	}
	res := decodeBody[bulkResp](t, call(t, r, http.MethodPost, "/hosts/bulk", bulkReq{IDs: []string{cp.ID, sys.ID}, Action: bulkDisable}))
	if res.OK != 1 || res.Failed != 1 || res.Results[1].Error == "" {
		t.Fatalf("bulk disable: %#v", res)
	}
	res = decodeBody[bulkResp](t, call(t, r, http.MethodPost, "/hosts/bulk", bulkReq{IDs: []string{cp.ID, sys.ID}, Action: bulkDelete}))
	if res.OK != 1 || res.Failed != 1 {
		t.Fatalf("bulk delete: %#v", res)
	}
	if _, err := st.Hosts().Get(ctx, cp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("copy should be deleted: %v", err)
	}
	usage = decodeBody[usageResp](t, call(t, r, http.MethodGet, "/hosts/"+grafana.ID+"/usage", nil))
	if len(usage.CertificateSharedWith) != 0 {
		t.Fatalf("certificate should now be unused: %#v", usage)
	}

	// ---- default host settings hook + delete guard
	req := httptest.NewRequest(http.MethodPut, "/settings/default_host", nil)
	hook := httpx.SettingsHooks[model.SettingsDefaultHost]
	if f := fields(hook.BeforeSave(req, nil, &model.DefaultHostSettings{Action: "host", HostID: "nope"})); f["hostId"] == "" {
		t.Fatalf("missing host accepted: %v", f)
	}
	dh := &model.DefaultHostSettings{Action: "host", HostID: grafana.ID}
	if err := hook.BeforeSave(req, nil, dh); err != nil {
		t.Fatalf("default host: %v", err)
	}
	if err := st.PutSettings(ctx, model.SettingsDefaultHost, dh); err != nil {
		t.Fatal(err)
	}
	var he *httpx.HTTPError
	if err := httpx.HostHooks.BeforeDelete(req, grafana); !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("deleting the default host should be refused: %v", err)
	}
	if err := httpx.HostHooks.BeforeDelete(req, sys); !errors.As(err, &he) || he.Code != "system_host" {
		t.Fatalf("deleting the system host should be refused: %v", err)
	}
	if err := st.PutSettings(ctx, model.SettingsDefaultHost, model.DefaultHostSettings{Action: "close"}); err != nil {
		t.Fatal(err)
	}

	// ---- delete with ?deleteCertificate=1 removes the orphaned certificate and its files
	del := httptest.NewRequest(http.MethodDelete, "/hosts/"+grafana.ID+"?deleteCertificate=1", nil)
	if err := httpx.HostHooks.BeforeDelete(del, grafana); err != nil {
		t.Fatal(err)
	}
	if err := st.Hosts().Delete(ctx, grafana.ID); err != nil {
		t.Fatal(err)
	}
	httpx.HostHooks.AfterDelete(del, grafana)
	if _, err := st.Certificates().Get(ctx, cert.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("certificate should be deleted: %v", err)
	}
	if _, err := os.Stat(certDir); !os.IsNotExist(err) {
		t.Fatalf("certificate files should be removed: %v", err)
	}

	// ---- redirects: whole-domain conflicts with a host, path redirects may share the domain
	rreq := httptest.NewRequest(http.MethodPost, "/redirects", nil)
	whole := &model.Redirect{Domains: []string{"proxy.home.lan"}, To: "https://example.com"}
	if f := fields(httpx.RedirectHooks.BeforeSave(rreq, nil, whole)); !strings.Contains(f["domains.0"], "host proxy.home.lan") {
		t.Fatalf("whole-domain redirect should conflict: %v", f)
	}
	path := &model.Redirect{Domains: []string{"proxy.home.lan"}, FromPath: "/old", To: "https://example.com/new"}
	if err := httpx.RedirectHooks.BeforeSave(rreq, nil, path); err != nil {
		t.Fatalf("path redirect: %v", err)
	}
	if path.Code != 301 {
		t.Fatalf("redirect not normalised: %#v", path)
	}
}
