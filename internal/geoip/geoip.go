// Package geoip keeps the IP → country database used for geo-blocking.
//
// Relay downloads the free DB-IP "IP to Country Lite" database (CC BY 4.0,
// no account needed) once a host uses geo-blocking and refreshes it monthly.
// A MaxMind GeoLite2-Country.mmdb placed in /data/geoip is used instead when
// present. Relay Edge reads the database directly; for nginx (whose official
// image has no GeoIP2 module) Relay writes a geo include with the networks of
// the allowed countries.
package geoip

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

const (
	dbipFile     = "dbip-country-lite.mmdb"
	userFile     = "GeoLite2-Country.mmdb"
	nginxDir     = "nginx"
	refreshAfter = 30 * 24 * time.Hour
	retryAfter   = 10 * time.Minute
	checkEvery   = time.Hour

	// DefaultURL is the DB-IP download; %s is the month (YYYY-MM).
	DefaultURL = "https://download.db-ip.com/free/dbip-country-lite-%s.mmdb.gz"

	SourceDBIP    = "dbip"
	SourceMaxMind = "maxmind"
)

var errNotPublished = errors.New("not published yet")

type Service struct {
	app *core.App
	dir string

	// URL is the download URL template (%s = YYYY-MM); tests point it elsewhere.
	URL    string
	Client *http.Client
	Now    func() time.Time

	mu sync.Mutex // downloads and country files

	stateMu     sync.Mutex
	downloading bool
	lastErr     string
	lastAttempt time.Time
}

func New(app *core.App) *Service {
	return &Service{
		app:    app,
		dir:    filepath.Join(app.Config.DataDir, "geoip"),
		URL:    DefaultURL,
		Client: &http.Client{Timeout: 5 * time.Minute},
		Now:    time.Now,
	}
}

func (s *Service) Start(ctx context.Context) error {
	go s.loop(ctx)
	return nil
}

func (s *Service) loop(ctx context.Context) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.needed(ctx) {
			if err := s.refresh(ctx, false); err != nil {
				s.app.Log.Warn("geoip database update", "err", err)
			}
		}
		t.Reset(checkEvery)
	}
}

func (s *Service) needed(ctx context.Context) bool {
	snap, err := s.app.Store.Snapshot(ctx)
	return err == nil && len(model.GeoCountries(snap.Hosts)) > 0
}

func (s *Service) dbipPath() string { return filepath.Join(s.dir, dbipFile) }
func (s *Service) userPath() string { return filepath.Join(s.dir, userFile) }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// Database returns the country database path ("" when there is none yet).
// /data is mounted at the same path in the engine containers.
func (s *Service) Database() string {
	if fileExists(s.userPath()) {
		return s.userPath()
	}
	if fileExists(s.dbipPath()) {
		return s.dbipPath()
	}
	return ""
}

// Ensure downloads the database when there is none (at most every few minutes after a failure).
func (s *Service) Ensure(ctx context.Context) error {
	if s.Database() != "" {
		return nil
	}
	return s.refresh(ctx, false)
}

// Update downloads the latest DB-IP database now.
func (s *Service) Update(ctx context.Context) error { return s.refresh(ctx, true) }

func (s *Service) refresh(ctx context.Context, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if !force {
		if fileExists(s.userPath()) {
			return nil // the user manages their own MaxMind file
		}
		if st, err := os.Stat(s.dbipPath()); err == nil && st.Size() > 0 && now.Sub(st.ModTime()) < refreshAfter {
			return nil
		}
		s.stateMu.Lock()
		recent, last := s.lastErr != "" && now.Sub(s.lastAttempt) < retryAfter, s.lastErr
		s.stateMu.Unlock()
		if recent {
			return errors.New(last)
		}
	}
	s.stateMu.Lock()
	s.downloading, s.lastAttempt = true, now
	hadErr := s.lastErr != ""
	s.stateMu.Unlock()

	err := s.download(ctx)

	s.stateMu.Lock()
	s.downloading = false
	s.lastErr = ""
	if err != nil {
		s.lastErr = err.Error()
	}
	s.stateMu.Unlock()
	if err != nil {
		if !hadErr {
			s.app.Activity(ctx, "geoip.failed", "warn", "Country database download failed", "", err.Error()+" · geo-blocking is skipped until it works")
		}
		return err
	}
	s.app.Log.Info("geoip database updated", "path", s.dbipPath())
	if n := s.regenerate(); n > 0 {
		s.reloadNginx(ctx)
	}
	return nil
}

func (s *Service) download(ctx context.Context) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	now := s.Now().UTC()
	months := []string{
		now.Format("2006-01"),
		time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01"),
	}
	var err error
	for _, m := range months {
		if err = s.fetch(ctx, fmt.Sprintf(s.URL, m)); !errors.Is(err, errNotPublished) {
			return err
		}
	}
	return fmt.Errorf("download country database: %w", err)
}

func (s *Service) fetch(ctx context.Context, rawURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Relay/"+s.app.Config.Version)
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("download country database: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotPublished
	}
	if resp.StatusCode != http.StatusOK {
		host := rawURL
		if u, err := url.Parse(rawURL); err == nil {
			host = u.Host
		}
		return fmt.Errorf("download country database: %s answered %s", host, resp.Status)
	}
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("download country database: not a gzip file: %w", err)
	}
	defer gz.Close()
	tmp, err := os.CreateTemp(s.dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, io.LimitReader(gz, 1<<30))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download country database: %w", err)
	}
	if err := verify(tmp.Name()); err != nil {
		return fmt.Errorf("downloaded country database is invalid: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.dbipPath())
}

func verify(path string) error {
	r, err := maxminddb.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	if !strings.Contains(strings.ToLower(r.Metadata.DatabaseType), "country") {
		return fmt.Errorf("not a country database (%s)", r.Metadata.DatabaseType)
	}
	return nil
}

// ---------------------------------------------------------------- nginx geo include

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// Code is the country, falling back to where the network is registered.
func (r countryRecord) Code() string {
	if r.Country.ISOCode != "" {
		return strings.ToUpper(r.Country.ISOCode)
	}
	return strings.ToUpper(r.RegisteredCountry.ISOCode)
}

const countriesHeader = "# countries: "

// CountryFile returns an nginx geo include ("<network> <CC>;" lines) with the
// networks of the countries, written when missing or older than the database.
// The name depends only on the countries, so versions in Config history keep
// working after the database is refreshed.
func (s *Service) CountryFile(ctx context.Context, countries []string) (string, error) {
	countries = model.NormalizeCountries(countries)
	if len(countries) == 0 {
		return "", errors.New("no countries")
	}
	db := s.Database()
	if db == "" {
		return "", errors.New("no country database yet")
	}
	sum := sha256.Sum256([]byte(strings.Join(countries, ",")))
	path := filepath.Join(s.dir, nginxDir, "countries-"+hex.EncodeToString(sum[:6])+".conf")
	s.mu.Lock()
	defer s.mu.Unlock()
	return path, writeCountryFile(db, path, countries, false)
}

func writeCountryFile(db, path string, countries []string, force bool) error {
	dbSt, err := os.Stat(db)
	if err != nil {
		return err
	}
	if !force {
		if st, err := os.Stat(path); err == nil && !st.ModTime().Before(dbSt.ModTime()) {
			return nil
		}
	}
	r, err := maxminddb.Open(db)
	if err != nil {
		return err
	}
	defer r.Close()
	want := map[string]bool{}
	for _, c := range countries {
		want[c] = true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".countries-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<16)
	fmt.Fprintf(w, "# Generated by Relay for geo-blocking from %s. Do not edit.\n%s%s\n", filepath.Base(db), countriesHeader, strings.Join(countries, ","))
	for res := range r.Networks() {
		if err := res.Err(); err != nil {
			tmp.Close()
			return fmt.Errorf("read country database: %w", err)
		}
		var rec countryRecord
		if err := res.Decode(&rec); err != nil {
			tmp.Close()
			return fmt.Errorf("read country database: %w", err)
		}
		code := rec.Code()
		if !want[code] {
			continue
		}
		p := res.Prefix()
		if overlapsLocal(p) {
			continue // local addresses are always allowed (listed in the geo block)
		}
		fmt.Fprintf(w, "%s %s;\n", p, code)
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func overlapsLocal(p netip.Prefix) bool {
	for _, l := range model.LocalNetworks {
		if p.Overlaps(l) {
			return true
		}
	}
	return false
}

// regenerate rewrites every existing country file from the current database
// and returns how many were written.
func (s *Service) regenerate() int {
	db := s.Database()
	if db == "" {
		return 0
	}
	files, _ := filepath.Glob(filepath.Join(s.dir, nginxDir, "countries-*.conf"))
	n := 0
	for _, f := range files {
		countries := readCountries(f)
		if len(countries) == 0 {
			continue
		}
		if err := writeCountryFile(db, f, countries, true); err != nil {
			s.app.Log.Warn("geoip country file", "file", f, "err", err)
			continue
		}
		n++
	}
	return n
}

func readCountries(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; i < 5 && sc.Scan(); i++ {
		if rest, ok := strings.CutPrefix(sc.Text(), countriesHeader); ok {
			return model.NormalizeCountries(strings.Split(rest, ","))
		}
	}
	return nil
}

// reloadNginx makes a running nginx read the regenerated country files.
// Relay Edge notices the new database by itself.
func (s *Service) reloadNginx(ctx context.Context) {
	if s.app.ProxyEngine(ctx) != agent.EngineNginx {
		return
	}
	e, ok := s.app.Engine.(interface {
		EngineAction(ctx context.Context, engine, action string) (*agent.ActionResponse, error)
	})
	if !ok {
		return
	}
	if resp, err := e.EngineAction(ctx, agent.EngineNginx, "reload"); err != nil {
		s.app.Log.Warn("reload nginx after country database update", "err", err)
	} else if resp != nil && !resp.OK {
		s.app.Log.Warn("reload nginx after country database update", "output", resp.Output)
	}
}

// ---------------------------------------------------------------- status

type Status struct {
	Source        string     `json:"source"` // dbip | maxmind | "" (none yet)
	Path          string     `json:"path,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
	BuiltAt       *time.Time `json:"builtAt,omitempty"`
	Hosts         int        `json:"hosts"`     // hosts with geo-blocking on
	Countries     []string   `json:"countries"` // allowed by any of them
	Downloading   bool       `json:"downloading"`
	LastError     string     `json:"lastError,omitempty"`
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
}

func (s *Service) Status(ctx context.Context) Status {
	st := Status{Countries: []string{}}
	if snap, err := s.app.Store.Snapshot(ctx); err == nil {
		if c := model.GeoCountries(snap.Hosts); c != nil {
			st.Countries = c
		}
		for _, h := range snap.Hosts {
			if h.GeoBlock.Enabled && len(model.NormalizeCountries(h.GeoBlock.AllowCountries)) > 0 {
				st.Hosts++
			}
		}
	}
	if db := s.Database(); db != "" {
		st.Path, st.Source = db, SourceDBIP
		if db == s.userPath() {
			st.Source = SourceMaxMind
		}
		if fi, err := os.Stat(db); err == nil {
			t := fi.ModTime().UTC()
			st.UpdatedAt = &t
		}
		if r, err := maxminddb.Open(db); err == nil {
			if r.Metadata.BuildEpoch > 0 {
				t := time.Unix(int64(r.Metadata.BuildEpoch), 0).UTC()
				st.BuiltAt = &t
			}
			r.Close()
		}
	}
	s.stateMu.Lock()
	st.Downloading, st.LastError = s.downloading, s.lastErr
	if !s.lastAttempt.IsZero() {
		t := s.lastAttempt.UTC()
		st.LastAttemptAt = &t
	}
	s.stateMu.Unlock()
	return st
}
