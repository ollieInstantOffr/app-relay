package geoip

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/geoip/mmdbtest"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func newTestService(t *testing.T, handler http.HandlerFunc) (*Service, *core.App) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{Version: "test", DataDir: dir, RunDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := New(app)
	s.URL = srv.URL + "/dbip-country-lite-%s.mmdb.gz"
	s.Now = func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	return s, app
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

var testDB = mmdbtest.Build(map[string]string{
	"1.0.0.0/24": "AU",
	"2.0.0.0/16": "NO",
	"5.0.0.0/8":  "SE",
	"10.0.0.0/8": "NO", // private: never listed, always allowed
})

func TestDownloadAndCountryFile(t *testing.T) {
	var requested []string
	s, app := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		if strings.Contains(r.URL.Path, "2026-09") {
			http.NotFound(w, r) // this month's file isn't out yet
			return
		}
		w.Write(gzipped(testDB))
	})
	ctx := context.Background()
	host := &model.ProxyHost{Domains: []string{"app.example.com"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.2", Port: 3000},
		GeoBlock: model.GeoBlock{Enabled: true, AllowCountries: []string{"no"}}, Source: model.SourceManual}
	if err := app.Store.Hosts().Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	if !s.needed(ctx) {
		t.Fatal("a geo-blocked host needs the database")
	}
	if err := s.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if len(requested) != 2 || !strings.Contains(requested[1], "2026-08") {
		t.Fatalf("requests = %v", requested)
	}
	if s.Database() != filepath.Join(app.Config.DataDir, "geoip", dbipFile) {
		t.Fatalf("database = %q", s.Database())
	}

	path, err := s.CountryFile(ctx, []string{"se", "NO", "xx1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, want := range []string{"# countries: NO,SE\n", "2.0.0.0/16 NO;\n", "5.0.0.0/8 SE;\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("country file lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"1.0.0.0", "10.0.0.0"} {
		if strings.Contains(got, not) {
			t.Errorf("country file must not list %s:\n%s", not, got)
		}
	}
	again, _ := s.CountryFile(ctx, []string{"NO", "SE"})
	if again != path {
		t.Fatalf("same countries, different file: %s vs %s", again, path)
	}
	if n := s.regenerate(); n != 1 {
		t.Fatalf("regenerated %d files", n)
	}

	st := s.Status(ctx)
	if st.Source != SourceDBIP || st.Hosts != 1 || len(st.Countries) != 1 || st.Countries[0] != "NO" || st.BuiltAt == nil || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestBadDownloadKeepsNothing(t *testing.T) {
	calls := 0
	s, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write(gzipped([]byte("not a database")))
	})
	ctx := context.Background()
	if err := s.Ensure(ctx); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v", err)
	}
	if s.Database() != "" {
		t.Fatal("an invalid download must not be used")
	}
	// A failure isn't retried on every render.
	if err := s.Ensure(ctx); err == nil || calls != 1 {
		t.Fatalf("retry: err=%v calls=%d", err, calls)
	}
	if st := s.Status(ctx); st.LastError == "" || st.Source != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestUserMaxMindFileWins(t *testing.T) {
	s, app := newTestService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("no download expected") })
	dir := filepath.Join(app.Config.DataDir, "geoip")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, userFile), testDB, 0o644)
	if err := s.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status(context.Background()).Source != SourceMaxMind {
		t.Fatal("the MaxMind file should be used")
	}
}
