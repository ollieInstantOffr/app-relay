// Package backup implements encrypted local backups and restore (slice: ops).
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	TriggerScheduled     = "scheduled"
	TriggerManual        = "manual"
	TriggerBeforeRestore = "before-restore"
	TriggerBeforeUpgrade = "before-upgrade"

	// keepOther is how many manual / before-restore / before-upgrade backups
	// are kept per trigger.
	keepOther = 10
	fileExt   = ".relay.age"

	// keepOnRestore is backed up but never overwritten by a restore.
	keepOnRestore = "config_versions"
)

type Service struct {
	app *core.App

	// ScryptLogN overrides the age scrypt work factor (tests); 0 = default.
	ScryptLogN int

	mu      sync.Mutex // one backup/restore at a time
	stateMu sync.Mutex
	lastRun *store.BackupRow // last scheduled attempt
	warning string
	skipDay string
}

func New(app *core.App) *Service { return &Service{app: app} }

// Dir is the local backup destination.
func (s *Service) Dir() string { return filepath.Join(s.app.Config.DataDir, "backups") }

func (s *Service) certDir() string { return filepath.Join(s.app.Config.DataDir, "certs") }

func (s *Service) Start(ctx context.Context) error {
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		s.app.Log.Warn("backup dir", "err", err)
	}
	go s.scheduler(ctx)
	return nil
}

// ---------------------------------------------------------------- settings

func (s *Service) settings(ctx context.Context) model.BackupSettings {
	v, err := store.LoadSettings[model.BackupSettings](ctx, s.app.Store, model.SettingsBackup)
	if err != nil {
		return store.DefaultBackup()
	}
	return v
}

func location(ctx context.Context, st *store.Store) *time.Location {
	g, err := store.LoadSettings[model.GeneralSettings](ctx, st, model.SettingsGeneral)
	if err == nil && g.Timezone != "" {
		if loc, err := time.LoadLocation(g.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// parseHHMM returns hour and minute of "03:00" (defaults to 03:00).
func parseHHMM(s string) (int, int) {
	m := hhmm.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 3, 0
	}
	return int(m[1][0]-'0')*10 + int(m[1][1]-'0'), int(m[2][0]-'0')*10 + int(m[2][1]-'0')
}

// NextRun returns the next scheduled time strictly after now.
func NextRun(timeOfDay string, now time.Time) time.Time {
	h, m := parseHHMM(timeOfDay)
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// RegisterSettingsHook makes the passphrase write-only and validates the
// schedule. Called from opsapi.Routes.
func RegisterSettingsHook() {
	httpx.SettingsHooks[model.SettingsBackup] = &httpx.SettingsHook{
		Decorate: func(r *http.Request, v any) any {
			b := *(v.(*model.BackupSettings))
			b.PassphraseSet = b.Passphrase != ""
			b.Passphrase = ""
			return b
		},
		BeforeSave: func(r *http.Request, prev, next any) error {
			p := prev.(*model.BackupSettings)
			n := next.(*model.BackupSettings)
			if n.Passphrase == "" {
				n.Passphrase = p.Passphrase
			}
			errs := model.Errs{}
			if n.Passphrase != p.Passphrase && len(n.Passphrase) < 8 {
				errs.Add("passphrase", "use at least 8 characters")
			}
			if !hhmm.MatchString(strings.TrimSpace(n.Time)) {
				errs.Add("time", "use HH:MM, e.g. 03:00")
			}
			n.Time = strings.TrimSpace(n.Time)
			if n.Keep < 1 || n.Keep > 365 {
				errs.Add("keep", "keep between 1 and 365 backups")
			}
			n.PassphraseSet = n.Passphrase != ""
			return errs.Err()
		},
		AfterSave: func(r *http.Request, prev, next any) {
			n := next.(*model.BackupSettings)
			n.PassphraseSet = n.Passphrase != ""
			n.Passphrase = "" // never echo it back
		},
	}
}

// ---------------------------------------------------------------- status

type Destination struct {
	Kind      string `json:"kind"` // local
	Path      string `json:"path"`
	FreeBytes *int64 `json:"freeBytes,omitempty"`
	Writable  bool   `json:"writable"`
}

type Status struct {
	Enabled     bool             `json:"enabled"`
	Time        string           `json:"time"`
	Keep        int              `json:"keep"`
	NextRunAt   *time.Time       `json:"nextRunAt,omitempty"`
	LastRun     *store.BackupRow `json:"lastRun,omitempty"`
	Warning     string           `json:"warning,omitempty"`
	Destination Destination      `json:"destination"`
	Timezone    string           `json:"timezone"`
}

func (s *Service) Status(ctx context.Context) Status {
	set := s.settings(ctx)
	loc := location(ctx, s.app.Store)
	st := Status{Enabled: set.Enabled, Time: set.Time, Keep: set.Keep, Timezone: loc.String()}
	if set.Enabled {
		n := NextRun(set.Time, time.Now().In(loc))
		st.NextRunAt = &n
		if set.Passphrase == "" {
			st.Warning = "Scheduled backups are skipped until you set an encryption passphrase."
		}
	}
	dir := s.Dir()
	st.Destination = Destination{Kind: "local", Path: dir}
	if free, ok := freeBytes(dir); ok {
		st.Destination.FreeBytes = &free
	}
	if f, err := os.CreateTemp(dir, ".probe-*"); err == nil {
		st.Destination.Writable = true
		f.Close()
		os.Remove(f.Name())
	}
	rows, err := s.app.Store.ListBackups(ctx)
	if err == nil {
		for i := range rows {
			if rows[i].Trigger == TriggerScheduled && rows[i].Status != "running" {
				r := rows[i]
				st.LastRun = &r
				break
			}
		}
	}
	s.stateMu.Lock()
	if st.Warning == "" && s.warning != "" && set.Enabled {
		st.Warning = s.warning
	}
	s.stateMu.Unlock()
	return st
}

// ---------------------------------------------------------------- create

// Create writes a new backup. passphrase overrides the stored passphrase
// (used for before-restore backups when none is stored).
func (s *Service) Create(ctx context.Context, trigger, passphrase string) (*store.BackupRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(ctx, trigger, passphrase)
}

func (s *Service) create(ctx context.Context, trigger, passphrase string) (*store.BackupRow, error) {
	set := s.settings(ctx)
	if passphrase == "" {
		passphrase = set.Passphrase
	}
	if passphrase == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "passphrase_required", "Set a backup encryption passphrase first")
	}
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		return nil, err
	}
	now := time.Now()
	id := store.NewID()
	name := "relay-" + now.UTC().Format("20060102-150405") + fileExt
	full := filepath.Join(s.Dir(), name)
	if _, err := os.Stat(full); err == nil {
		name = "relay-" + now.UTC().Format("20060102-150405") + "-" + id[:4] + fileExt
		full = filepath.Join(s.Dir(), name)
	}
	row := store.BackupRow{ID: id, CreatedAt: now, Trigger: trigger, File: name, Status: "running", Contents: map[string]int{}}
	if err := s.app.Store.InsertBackup(ctx, row); err != nil {
		return nil, err
	}
	s.app.Bus.Publish(events.BackupChanged, map[string]any{"id": id, "status": "running"})

	fail := func(err error) (*store.BackupRow, error) {
		row.Status = "failed"
		row.Error = err.Error()
		_ = s.app.Store.UpdateBackup(context.WithoutCancel(ctx), row)
		s.app.Bus.Publish(events.BackupChanged, map[string]any{"id": id, "status": "failed"})
		return &row, err
	}

	tmp := full + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	m, err := writeArchive(ctx, f, s.app.Store, s.certDir(), passphrase, s.app.Config.Version, set.IncludePrivateKeys, s.ScryptLogN)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fail(err)
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return fail(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		return fail(err)
	}
	row.Size = info.Size()
	row.Contents = m.Counts
	row.Status = "ok"
	if err := s.app.Store.UpdateBackup(ctx, row); err != nil {
		return nil, err
	}
	s.prune(ctx)
	s.app.Bus.Publish(events.BackupChanged, map[string]any{"id": id, "status": "ok"})
	return &row, nil
}

// prune enforces retention: newest keep scheduled backups, newest keepOther
// per other trigger.
func (s *Service) prune(ctx context.Context) {
	set := s.settings(ctx)
	rows, err := s.app.Store.ListBackups(ctx)
	if err != nil {
		return
	}
	seen := map[string]int{}
	for _, r := range rows {
		if r.Status == "running" {
			continue
		}
		seen[r.Trigger]++
		limit := keepOther
		if r.Trigger == TriggerScheduled {
			limit = set.Keep
			if limit < 1 {
				limit = 1
			}
		}
		if seen[r.Trigger] > limit {
			if err := s.remove(ctx, r); err != nil {
				s.app.Log.Warn("prune backup", "id", r.ID, "err", err)
			}
		}
	}
}

func (s *Service) remove(ctx context.Context, r store.BackupRow) error {
	if r.File != "" {
		if err := os.Remove(filepath.Join(s.Dir(), filepath.Base(r.File))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.app.Store.DeleteBackup(ctx, r.ID)
}

// Delete removes a backup file and row.
func (s *Service) Delete(ctx context.Context, id string) error {
	r, err := s.app.Store.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if r.Status == "running" {
		return httpx.Errorf(http.StatusConflict, "running", "this backup is still being written")
	}
	if err := s.remove(ctx, *r); err != nil {
		return err
	}
	s.app.Bus.Publish(events.BackupChanged, map[string]any{"id": id, "status": "deleted"})
	return nil
}

// Path returns the file of a finished backup.
func (s *Service) Path(ctx context.Context, id string) (string, *store.BackupRow, error) {
	r, err := s.app.Store.GetBackup(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if r.Status != "ok" {
		return "", nil, httpx.Errorf(http.StatusConflict, "not_ready", "backup is "+r.Status)
	}
	p := filepath.Join(s.Dir(), filepath.Base(r.File))
	if _, err := os.Stat(p); err != nil {
		return "", nil, httpx.Errorf(http.StatusNotFound, "file_missing", "backup file is missing from "+s.Dir())
	}
	return p, r, nil
}

// ---------------------------------------------------------------- restore

type RestoreResult struct {
	Manifest       Manifest `json:"manifest"`
	BeforeRestore  string   `json:"beforeRestoreBackupId"`
	SessionKept    bool     `json:"sessionKept"`
	ReviewPending  bool     `json:"reviewPending"`
	RestoredTables int      `json:"restoredTables"`
}

// RestoreByID restores a stored backup (passphrase defaults to the stored one).
func (s *Service) RestoreByID(ctx context.Context, id, passphrase string) (*RestoreResult, error) {
	p, _, err := s.Path(ctx, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s.Restore(ctx, f, passphrase)
}

// Restore decrypts src, takes a before-restore backup of the current state,
// then replaces the database tables and certificate files.
func (s *Service) Restore(ctx context.Context, src io.Reader, passphrase string) (*RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.settings(ctx)
	if passphrase == "" {
		passphrase = set.Passphrase
	}
	if passphrase == "" {
		return nil, httpx.Errorf(http.StatusBadRequest, "passphrase_required", "Enter the passphrase this backup was encrypted with")
	}
	a, err := readArchive(src, passphrase)
	if errors.Is(err, ErrWrongPassphrase) {
		return nil, httpx.Errorf(http.StatusBadRequest, "wrong_passphrase", "Wrong passphrase, or the file is not a Relay backup")
	}
	if err != nil {
		return nil, httpx.Errorf(http.StatusBadRequest, "invalid_archive", err.Error())
	}

	before, err := s.create(ctx, TriggerBeforeRestore, set.Passphrase)
	if err != nil && set.Passphrase == "" {
		before, err = s.create(ctx, TriggerBeforeRestore, passphrase)
	}
	if err != nil {
		return nil, fmt.Errorf("could not back up the current state before restoring: %w", err)
	}

	// Config versions describe what THIS instance's engines actually run
	// (the live row drives pending changes and rollback). Restoring them from
	// an archive would mark an old version live while nginx/HAProxy still
	// serve the newer one, so the restored config could never be applied
	// ("nothing to apply"). Keep the local history; the restored config shows
	// up as pending changes instead.
	delete(a.Tables, keepOnRestore)
	actor := core.ActorFrom(ctx)
	kept, err := s.app.Store.RestoreTables(ctx, a.Tables, actor.SessionID)
	if err != nil {
		return nil, fmt.Errorf("restore failed, nothing was changed: %w", err)
	}
	if err := writeCerts(s.certDir(), a.Certs); err != nil {
		s.app.Log.Error("restore certificate files", "err", err)
	}

	for _, kind := range []string{model.KindHost, model.KindRedirect, model.KindStream, model.KindAccessList, model.KindCertificate, model.KindDNSProvider, model.KindBackend, model.KindFrontend} {
		s.app.Changed(ctx, kind, "", "", core.ActionUpdated)
	}
	for _, key := range []string{model.SettingsGeneral, model.SettingsTLS, model.SettingsDefaultHost, model.SettingsHAProxy, model.SettingsBlocklist} {
		s.app.Changed(ctx, "settings", key, key, core.ActionUpdated)
	}
	detail := fmt.Sprintf("archive from %s · %d hosts · %d backends · %d certs", a.Manifest.Created.In(location(ctx, s.app.Store)).Format("2006-01-02 15:04"),
		a.Manifest.Counts["hosts"], a.Manifest.Counts["backends"], a.Manifest.Counts["certificates"])
	s.app.Audit(ctx, core.AuditEntry{Action: "backup.restore", Target: "backup", Detail: detail, Result: "ok"})
	s.app.Activity(ctx, "backup.restored", "warn", "Configuration restored from backup", "", detail)
	s.app.Bus.Publish(events.BackupChanged, map[string]any{"status": "restored"})

	return &RestoreResult{
		Manifest: a.Manifest, BeforeRestore: before.ID, SessionKept: kept, ReviewPending: true, RestoredTables: len(a.Tables),
	}, nil
}

// ---------------------------------------------------------------- scheduler

func (s *Service) scheduler(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

const (
	kvLastScheduled = "ops.backup.last_scheduled"
	kvLastSkipped   = "ops.backup.last_skipped"
)

func (s *Service) tick(ctx context.Context) {
	set := s.settings(ctx)
	if !set.Enabled {
		return
	}
	loc := location(ctx, s.app.Store)
	now := time.Now().In(loc)
	h, m := parseHHMM(set.Time)
	due := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, loc)
	if now.Before(due) {
		return
	}
	day := now.Format("2006-01-02")
	if last, err := s.app.Store.GetKV(ctx, kvLastScheduled); err == nil && string(last) == day {
		return
	}
	if set.Passphrase == "" {
		s.stateMu.Lock()
		first := s.skipDay != day
		s.skipDay = day
		// Persist so restarts on the same day don't repeat the activity item.
		if first {
			if last, err := s.app.Store.GetKV(ctx, kvLastSkipped); err == nil && string(last) == day {
				first = false
			} else {
				_ = s.app.Store.PutKV(ctx, kvLastSkipped, []byte(day))
			}
		}
		s.warning = "Scheduled backup skipped on " + day + ": no encryption passphrase set."
		s.stateMu.Unlock()
		if first {
			s.app.Activity(ctx, "backup.skipped", "warn", "Scheduled backup skipped", "", "Set an encryption passphrase in Settings → Backup & restore")
			s.app.Bus.Publish(events.BackupChanged, map[string]any{"status": "skipped"})
		}
		return
	}
	_ = s.app.Store.PutKV(ctx, kvLastScheduled, []byte(day))
	row, err := s.Create(ctx, TriggerScheduled, "")
	s.stateMu.Lock()
	s.warning = ""
	s.lastRun = row
	s.stateMu.Unlock()
	if err != nil {
		s.app.Log.Error("scheduled backup", "err", err)
		s.app.Activity(ctx, "backup.failed", "error", "Scheduled backup failed", "", err.Error())
		return
	}
	s.app.Log.Info("scheduled backup written", "file", row.File, "size", row.Size)
}
