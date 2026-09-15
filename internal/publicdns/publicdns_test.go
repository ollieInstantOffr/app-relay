package publicdns

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// fakeGoDaddy is an in-memory GoDaddy domains API.
type fakeGoDaddy struct {
	mu      sync.Mutex
	records map[string][]gdRecord // zone → records
	denied  bool
	calls   []string
}

func (f *fakeGoDaddy) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.calls = append(f.calls, r.Method+" "+r.URL.Path)
			denied := f.denied
			f.mu.Unlock()
			if r.Header.Get("Authorization") != "sso-key key1:secret1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if denied {
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"code":"ACCESS_DENIED","message":"Authenticated user is not allowed access"}`)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/domains", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []map[string]string
		for z := range f.records {
			out = append(out, map[string]string{"domain": z})
		}
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("GET /v1/domains/{d}/records", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.records[r.PathValue("d")])
	}))
	mux.HandleFunc("PATCH /v1/domains/{d}/records", auth(func(w http.ResponseWriter, r *http.Request) {
		var add []gdRecord
		json.NewDecoder(r.Body).Decode(&add)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.records[r.PathValue("d")] = append(f.records[r.PathValue("d")], add...)
	}))
	replace := func(w http.ResponseWriter, r *http.Request, next []gdRecord) {
		f.mu.Lock()
		defer f.mu.Unlock()
		d, typ, name := r.PathValue("d"), r.PathValue("type"), r.PathValue("name")
		var keep []gdRecord
		for _, rec := range f.records[d] {
			if !(rec.Type == typ && rec.Name == name) {
				keep = append(keep, rec)
			}
		}
		for _, n := range next {
			n.Type, n.Name = typ, name
			keep = append(keep, n)
		}
		f.records[d] = keep
	}
	mux.HandleFunc("PUT /v1/domains/{d}/records/{type}/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		var next []gdRecord
		json.NewDecoder(r.Body).Decode(&next)
		replace(w, r, next)
	}))
	mux.HandleFunc("DELETE /v1/domains/{d}/records/{type}/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		replace(w, r, nil)
	}))
	return mux
}

func (f *fakeGoDaddy) find(zone, typ, name string) []gdRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gdRecord
	for _, r := range f.records[zone] {
		if r.Type == typ && r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

type fixture struct {
	svc      *Service
	app      *core.App
	gd       *fakeGoDaddy
	provider *model.DNSProvider
	ctx      context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gd := &fakeGoDaddy{records: map[string][]gdRecord{
		"instantoffr.com": {
			{Type: "A", Name: "@", Data: "198.51.100.1", TTL: 600},
			{Type: "NS", Name: "@", Data: "ns1.domaincontrol.com", TTL: 3600},
			{Type: "A", Name: "www", Data: "203.0.113.10", TTL: 600},
			{Type: "CNAME", Name: "blog", Data: "blog.example.net", TTL: 3600},
			{Type: "A", Name: "*.dev", Data: "203.0.113.10", TTL: 600},
		},
	}}
	srv := httptest.NewServer(gd.handler())
	t.Cleanup(srv.Close)
	prev := godaddyAPI
	godaddyAPI = srv.URL
	t.Cleanup(func() { godaddyAPI = prev })

	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{Version: "test", RunDir: t.TempDir()}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := core.WithActor(context.Background(), core.Actor{Type: core.ActorUser, Name: "admin", Role: core.RoleAdmin})

	p := &model.DNSProvider{Name: "GoDaddy", Type: "godaddy", Credentials: map[string]string{"apiKey": "key1", "apiSecret": "secret1"}}
	if err := st.DNSProviders().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	gen := store.DefaultGeneral()
	gen.PublicIP = "203.0.113.10"
	if err := st.PutSettings(ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
	set := model.DefaultPublicDNS()
	set.Enabled, set.ProviderIDs = true, []string{p.ID}
	if err := st.PutSettings(ctx, model.SettingsPublicDNS, set); err != nil {
		t.Fatal(err)
	}
	for _, domains := range [][]string{{"test.instantoffr.com"}, {"www.instantoffr.com"}, {"blog.instantoffr.com"}, {"app.dev.instantoffr.com"}, {"grafana.home.lan"}} {
		h := &model.ProxyHost{Domains: domains, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.5", Port: 3000}, HSTS: "inherit"}
		if err := st.Hosts().Create(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(app)
	app.PublicDNS = svc
	return &fixture{svc: svc, app: app, gd: gd, provider: p, ctx: ctx}
}

func statusOf(results []DomainCheck, domain string) DomainCheck {
	for _, r := range results {
		if r.Domain == domain {
			return r
		}
	}
	return DomainCheck{}
}

func TestCheckStatuses(t *testing.T) {
	f := newFixture(t)
	results, err := f.svc.Check(f.ctx, []string{"test.instantoffr.com", "www.instantoffr.com", "blog.instantoffr.com", "app.dev.instantoffr.com", "grafana.home.lan", "instantoffr.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"test.instantoffr.com":    StatusMissing,
		"www.instantoffr.com":     StatusExists,
		"blog.instantoffr.com":    StatusConflict,
		"app.dev.instantoffr.com": StatusWildcard,
		"grafana.home.lan":        StatusUnmanaged,
		"instantoffr.com":         StatusConflict,
	}
	for d, st := range want {
		if got := statusOf(results, d); got.Status != st {
			t.Errorf("%s: status %q (%s), want %q", d, got.Status, got.Message, st)
		}
	}
	missing := statusOf(results, "test.instantoffr.com")
	if missing.Planned == nil || missing.Planned.Type != "A" || missing.Planned.Name != "test" || missing.Planned.Data != "203.0.113.10" || missing.Zone != "instantoffr.com" {
		t.Fatalf("planned = %+v", missing.Planned)
	}
	if www := statusOf(results, "www.instantoffr.com"); len(www.RelayRecords) != 1 {
		t.Fatalf("www relay records = %+v", www.RelayRecords)
	}
	if blog := statusOf(results, "blog.instantoffr.com"); len(blog.RelayRecords) != 0 || !strings.Contains(blog.Message, "blog.example.net") {
		t.Fatalf("conflict = %+v", blog)
	}

	// Excluded zones are left alone.
	set := f.svc.settings(f.ctx)
	set.ExcludedZones = []string{"instantoffr.com"}
	f.app.Store.PutSettings(f.ctx, model.SettingsPublicDNS, set)
	results, _ = f.svc.Check(f.ctx, []string{"test.instantoffr.com"})
	if results[0].Status != StatusExcluded {
		t.Fatalf("excluded zone: %+v", results[0])
	}
}

func TestSyncCreatesOnlyMissingRecords(t *testing.T) {
	f := newFixture(t)
	results, err := f.svc.Sync(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(results, "test.instantoffr.com"); got.Status != StatusCreated || len(got.RelayRecords) != 1 {
		t.Fatalf("test: %+v", got)
	}
	if recs := f.gd.find("instantoffr.com", "A", "test"); len(recs) != 1 || recs[0].Data != "203.0.113.10" || recs[0].TTL != 600 {
		t.Fatalf("created records = %+v", recs)
	}
	if recs := f.gd.find("instantoffr.com", "CNAME", "blog"); len(recs) != 1 || recs[0].Data != "blog.example.net" {
		t.Fatalf("conflicting record changed: %+v", recs)
	}
	if recs := f.gd.find("instantoffr.com", "A", "app.dev"); len(recs) != 0 {
		t.Fatal("wildcard-covered names must not get a record")
	}
	// A second sync changes nothing.
	f.gd.mu.Lock()
	before := len(f.gd.calls)
	f.gd.mu.Unlock()
	results, _ = f.svc.Sync(f.ctx, nil)
	f.gd.mu.Lock()
	for _, c := range f.gd.calls[before:] {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("second sync wrote: %s", c)
		}
	}
	f.gd.mu.Unlock()
	if got := statusOf(results, "test.instantoffr.com"); got.Status != StatusExists {
		t.Fatalf("after sync: %+v", got)
	}
}

func TestSyncAfterApply(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.Start(ctx)
	time.Sleep(20 * time.Millisecond)
	f.app.Bus.Publish(events.ApplyFinished, map[string]any{"version": int64(3), "status": "live", "error": ""})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.gd.find("instantoffr.com", "A", "test")) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the record was not created after apply")
}

func TestRecordCRUD(t *testing.T) {
	f := newFixture(t)
	ten := 10
	rec, err := f.svc.Create(f.ctx, "instantoffr.com", RecordInput{Type: "txt", Name: "_verify.instantoffr.com.", Data: "token-one", TTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "_verify" || rec.TTL != 600 || rec.ID == "" || rec.FQDN != "_verify.instantoffr.com" {
		t.Fatalf("created = %+v", rec)
	}
	if _, err := f.svc.Create(f.ctx, "instantoffr.com", RecordInput{Type: "TXT", Name: "_verify", Data: "token-two"}); err != nil {
		t.Fatal(err)
	}

	// Editing one of two TXT records keeps the other.
	up, err := f.svc.Update(f.ctx, "instantoffr.com", rec.ID, RecordInput{Type: "TXT", Name: "_verify", Data: "token-one-b", TTL: 3600})
	if err != nil {
		t.Fatal(err)
	}
	txt := f.gd.find("instantoffr.com", "TXT", "_verify")
	if len(txt) != 2 || up.Data != "token-one-b" || up.TTL != 3600 {
		t.Fatalf("after update: %+v / %+v", txt, up)
	}

	// Deleting one keeps the other; deleting the last removes the set.
	if err := f.svc.Delete(f.ctx, "instantoffr.com", up.ID); err != nil {
		t.Fatal(err)
	}
	if txt := f.gd.find("instantoffr.com", "TXT", "_verify"); len(txt) != 1 || txt[0].Data != "token-two" {
		t.Fatalf("after delete: %+v", txt)
	}

	// MX with priority, renamed on update.
	mx, err := f.svc.Create(f.ctx, "instantoffr.com", RecordInput{Type: "MX", Name: "@", Data: "mail.instantoffr.com", Priority: &ten})
	if err != nil {
		t.Fatal(err)
	}
	if mx.Priority == nil || *mx.Priority != 10 {
		t.Fatalf("mx = %+v", mx)
	}

	// Validation.
	var ve *model.ValidationError
	if _, err := f.svc.Create(f.ctx, "instantoffr.com", RecordInput{Type: "A", Name: "x", Data: "not-an-ip", TTL: 300}); !errors.As(err, &ve) || ve.Fields["data"] == "" || ve.Fields["ttl"] == "" {
		t.Fatalf("validation = %v", err)
	}
	if _, err := f.svc.Create(f.ctx, "instantoffr.com", RecordInput{Type: "CNAME", Name: "@", Data: "x.example.com"}); !errors.As(err, &ve) || ve.Fields["name"] == "" {
		t.Fatalf("apex CNAME = %v", err)
	}
	// Apex NS is read-only.
	_, list, _ := f.svc.Records(f.ctx, "instantoffr.com")
	for _, r := range list {
		if r.Type == "NS" && r.Name == "@" {
			if !r.ReadOnly {
				t.Fatal("apex NS must be read-only")
			}
			if err := f.svc.Delete(f.ctx, "instantoffr.com", r.ID); err == nil {
				t.Fatal("deleting apex NS must fail")
			}
		}
		if r.Type == "A" && r.Name == "www" && !slices.Contains(r.Hosts, hostID(t, f, "www.instantoffr.com")) {
			t.Fatalf("www record should list its proxy host: %+v", r)
		}
	}
}

func hostID(t *testing.T, f *fixture, domain string) string {
	hosts, _ := f.app.Store.Hosts().List(f.ctx)
	for _, h := range hosts {
		if slices.Contains(h.Domains, domain) {
			return h.ID
		}
	}
	t.Fatalf("no host %s", domain)
	return ""
}

func TestProviderErrorsAndDisabled(t *testing.T) {
	f := newFixture(t)
	f.gd.mu.Lock()
	f.gd.denied = true
	f.gd.mu.Unlock()
	st := f.svc.Status(f.ctx)
	if len(st.Providers) != 1 || !strings.Contains(st.Providers[0].Error, "10 or more domains") {
		t.Fatalf("status = %+v", st)
	}

	set := f.svc.settings(f.ctx)
	set.Enabled = false
	f.app.Store.PutSettings(f.ctx, model.SettingsPublicDNS, set)
	if _, err := f.svc.Check(f.ctx, []string{"test.instantoffr.com"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled check = %v", err)
	}
}

func TestSettingsValidation(t *testing.T) {
	f := newFixture(t)
	route53 := &model.DNSProvider{Name: "AWS", Type: "route53", Credentials: map[string]string{"accessKeyId": "x", "secretAccessKey": "y"}}
	if err := f.app.Store.DNSProviders().Create(f.ctx, route53); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		set   model.PublicDNSSettings
		field string
	}{
		{model.PublicDNSSettings{Enabled: true, RecordType: "A", TTL: 600}, "providerIds"},
		{model.PublicDNSSettings{Enabled: true, ProviderIDs: []string{route53.ID}, RecordType: "A", TTL: 600}, "providerIds"},
		{model.PublicDNSSettings{ProviderIDs: []string{f.provider.ID}, RecordType: "CNAME", TTL: 600}, "target"},
		{model.PublicDNSSettings{ProviderIDs: []string{f.provider.ID}, RecordType: "A", Target: "example.com", TTL: 600}, "target"},
		{model.PublicDNSSettings{ProviderIDs: []string{f.provider.ID}, RecordType: "A", TTL: 30}, "ttl"},
	}
	for i, c := range cases {
		set := c.set
		var ve *model.ValidationError
		if err := f.svc.validateSettings(f.ctx, &set); !errors.As(err, &ve) || ve.Fields[c.field] == "" {
			t.Errorf("case %d: err = %v, want field %s", i, err, c.field)
		}
	}
	ok := model.PublicDNSSettings{Enabled: true, ProviderIDs: []string{f.provider.ID}, RecordType: "cname", Target: "Edge.Example.com.", TTL: 600}
	if err := f.svc.validateSettings(f.ctx, &ok); err != nil || ok.Target != "edge.example.com" || ok.RecordType != "CNAME" {
		t.Fatalf("valid settings: %v %+v", err, ok)
	}
}
