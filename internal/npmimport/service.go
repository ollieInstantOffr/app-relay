// Package npmimport converts a Nginx Proxy Manager SQLite database into Relay
// configuration (slice: ops).
package npmimport

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
)

const previewTTL = 30 * time.Minute

type Service struct {
	app *core.App

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	plan    *Plan
	expires time.Time
}

func New(app *core.App) *Service { return &Service{app: app, sessions: map[string]*session{}} }

// ---------------------------------------------------------------- preview

type Count struct {
	Total     int `json:"total"`
	Conflicts int `json:"conflicts"`
	Warnings  int `json:"warnings"`
}

type PreviewItem struct {
	Kind string `json:"kind"` // hosts | redirects | streams | accessLists | certificates
	Base
}

type Preview struct {
	Token     string           `json:"token"`
	Source    string           `json:"source"`
	ExpiresAt time.Time        `json:"expiresAt"`
	Counts    map[string]Count `json:"counts"`
	Items     []PreviewItem    `json:"items"`
	Warnings  []string         `json:"warnings"`
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func openReadOnly(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	for _, q := range []string{"mode=ro", "mode=ro&immutable=1"} {
		db, err := sql.Open("sqlite", u.String()+"?"+q)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		if err := db.Ping(); err == nil {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err == nil {
				return db, nil
			}
		}
		db.Close()
	}
	return nil, errors.New("could not open the file as a SQLite database")
}

func (s *Service) preview(ctx context.Context, dbPath, dataDir, source string) (*Preview, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "invalid_database", err.Error())
	}
	defer db.Close()
	ex, err := loadExisting(ctx, s.app.Store)
	if err != nil {
		return nil, err
	}
	plan, err := BuildPlan(ctx, db, dataDir, ex)
	if err != nil {
		if strings.Contains(err.Error(), "not a Nginx Proxy Manager") {
			return nil, httpx.Errorf(http.StatusUnprocessableEntity, "invalid_database", err.Error())
		}
		return nil, err
	}
	token := newToken()
	s.mu.Lock()
	for t, sess := range s.sessions {
		if time.Now().After(sess.expires) {
			delete(s.sessions, t)
		}
	}
	expires := time.Now().Add(previewTTL)
	s.sessions[token] = &session{plan: plan, expires: expires}
	s.mu.Unlock()
	return summarize(plan, token, source, expires), nil
}

func summarize(p *Plan, token, source string, expires time.Time) *Preview {
	pv := &Preview{Token: token, Source: source, ExpiresAt: expires, Counts: map[string]Count{}, Items: []PreviewItem{}, Warnings: p.Warnings}
	if pv.Warnings == nil {
		pv.Warnings = []string{}
	}
	add := func(kind string, b Base) {
		c := pv.Counts[kind]
		c.Total++
		if b.Conflict != "" {
			c.Conflicts++
		}
		c.Warnings += len(b.Warnings)
		pv.Counts[kind] = c
		if b.Warnings == nil {
			b.Warnings = []string{}
		}
		pv.Items = append(pv.Items, PreviewItem{Kind: kind, Base: b})
	}
	for _, k := range []string{"hosts", "redirects", "streams", "accessLists", "certificates"} {
		pv.Counts[k] = Count{}
	}
	// Overwritable mirrors Commit: conflicts replace exactly one existing
	// entity, and certificates only when the NPM files are available.
	for _, it := range p.Hosts {
		it.Overwritable = it.Conflict != "" && it.ExistingID != ""
		add("hosts", it.Base)
	}
	for _, it := range p.Redirects {
		it.Overwritable = it.Conflict != "" && it.ExistingID != ""
		add("redirects", it.Base)
	}
	for _, it := range p.Streams {
		it.Overwritable = it.Conflict != "" && it.ExistingID != ""
		add("streams", it.Base)
	}
	for _, it := range p.AccessLists {
		it.Overwritable = it.Conflict != "" && it.ExistingID != ""
		add("accessLists", it.Base)
	}
	for _, it := range p.Certs {
		it.Overwritable = it.Conflict != "" && it.ExistingID != "" && it.Chain != nil
		add("certificates", it.Base)
	}
	return pv
}

// PreviewUpload converts an uploaded database.sqlite.
func (s *Service) PreviewUpload(ctx context.Context, src io.Reader, filename string) (*Preview, error) {
	tmpDir := filepath.Join(s.app.Config.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmpDir, "npm-*.sqlite")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, io.LimitReader(src, 1<<30)); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return s.preview(ctx, f.Name(), "", filename)
}

// PreviewPath converts a mounted NPM data folder (or a database file path).
func (s *Service) PreviewPath(ctx context.Context, p string) (*Preview, error) {
	p = filepath.Clean(strings.TrimSpace(p))
	if !filepath.IsAbs(p) {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "invalid", "enter an absolute path inside the Relay container, e.g. /import/npm/data")
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, httpx.Errorf(http.StatusUnprocessableEntity, "not_found", p+" does not exist inside the Relay container — mount the NPM data folder first")
	}
	dbPath, dataDir := p, filepath.Dir(p)
	if info.IsDir() {
		dataDir = p
		dbPath = filepath.Join(p, "database.sqlite")
		if _, err := os.Stat(dbPath); err != nil {
			return nil, httpx.Errorf(http.StatusUnprocessableEntity, "not_found", "no database.sqlite in "+p+" (only NPM installs using SQLite can be imported)")
		}
	}
	return s.preview(ctx, dbPath, dataDir, p)
}

// ---------------------------------------------------------------- commit

type CommitResult struct {
	Created map[string]int `json:"created"`
	Updated map[string]int `json:"updated"`
	Skipped map[string]int `json:"skipped"`
	Errors  []string       `json:"errors"`
}

func keepSecrets(next, prev any) error {
	if sk, ok := next.(model.SecretKeeper); ok {
		return sk.KeepSecrets(prev)
	}
	return nil
}

func validate(v any) error {
	if val, ok := v.(model.Validator); ok {
		return val.Validate()
	}
	return nil
}

// Commit applies a previewed import. Conflicting items are skipped unless
// overwrite is set and the conflict maps to exactly one existing entity.
func (s *Service) Commit(r *http.Request, token string, overwrite bool) (*CommitResult, error) {
	ctx := r.Context()
	s.mu.Lock()
	sess := s.sessions[token]
	if sess != nil {
		delete(s.sessions, token)
	}
	s.mu.Unlock()
	if sess == nil || time.Now().After(sess.expires) {
		return nil, httpx.Errorf(http.StatusGone, "preview_expired", "This preview expired — run the import preview again")
	}
	p := sess.plan
	res := &CommitResult{Created: map[string]int{}, Updated: map[string]int{}, Skipped: map[string]int{}, Errors: []string{}}
	st := s.app.Store
	const detail = "imported from Nginx Proxy Manager"
	fail := func(kind, name string, err error) {
		res.Skipped[kind]++
		res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", name, err))
	}

	// Access lists.
	accessIDs := map[int]string{}
	for _, it := range p.AccessLists {
		next := it.List
		var prev *model.AccessList
		if it.ExistingID != "" {
			if !overwrite {
				accessIDs[it.NPMID] = it.ExistingID
				res.Skipped["accessLists"]++
				continue
			}
			var err error
			if prev, err = st.AccessLists().Get(ctx, it.ExistingID); err != nil {
				fail("accessLists", it.Name, err)
				continue
			}
			next.ID = prev.ID
		}
		var pa any
		if prev != nil {
			pa = prev
		}
		if err := keepSecrets(&next, pa); err != nil {
			fail("accessLists", it.Name, err)
			continue
		}
		for i := range next.BasicAuth.Users {
			u := &next.BasicAuth.Users[i]
			if u.Password != "" && u.PasswordHash == "" {
				h, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
				if err != nil {
					return nil, err
				}
				u.PasswordHash = string(h)
			}
			if u.PasswordHash != "" {
				u.Password = ""
			}
		}
		if httpx.AccessListHooks.BeforeSave != nil {
			if err := httpx.AccessListHooks.BeforeSave(r, prev, &next); err != nil {
				fail("accessLists", it.Name, err)
				continue
			}
		}
		if err := validate(&next); err != nil {
			fail("accessLists", it.Name, err)
			continue
		}
		action := core.ActionCreated
		if prev == nil {
			if err := st.AccessLists().Create(ctx, &next); err != nil {
				fail("accessLists", it.Name, err)
				continue
			}
			res.Created["accessLists"]++
		} else {
			action = core.ActionUpdated
			if err := st.AccessLists().Update(ctx, &next); err != nil {
				fail("accessLists", it.Name, err)
				continue
			}
			res.Updated["accessLists"]++
		}
		accessIDs[it.NPMID] = next.ID
		s.app.Audit(ctx, core.AuditEntry{Action: "access_list." + verb(action), Target: next.Name, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindAccessList, next.ID, next.Name, action)
	}

	// Certificates.
	certIDs := map[int]string{}
	certDir := filepath.Join(s.app.Config.DataDir, "certs")
	for _, it := range p.Certs {
		next := it.Cert
		var prev *model.Certificate
		if it.ExistingID != "" {
			if !overwrite || it.Chain == nil {
				certIDs[it.NPMID] = it.ExistingID
				res.Skipped["certificates"]++
				continue
			}
			var err error
			if prev, err = st.Certificates().Get(ctx, it.ExistingID); err != nil {
				fail("certificates", it.Name, err)
				continue
			}
			next.ID = prev.ID
			next.History = append(prev.History, next.History...)
		}
		if next.Challenge == model.ChallengeDNS01 && next.DNSProviderID == "" && model.IsACMEProvider(next.Provider) {
			fail("certificates", it.Name, fmt.Errorf("uses DNS-01 via %s — add that DNS provider in Relay and import again", orDash(it.dnsType)))
			continue
		}
		if err := validate(&next); err != nil {
			fail("certificates", it.Name, err)
			continue
		}
		action := core.ActionCreated
		if prev == nil {
			if err := st.Certificates().Create(ctx, &next); err != nil {
				fail("certificates", it.Name, err)
				continue
			}
			res.Created["certificates"]++
		} else {
			action = core.ActionUpdated
			if err := st.Certificates().Update(ctx, &next); err != nil {
				fail("certificates", it.Name, err)
				continue
			}
			res.Updated["certificates"]++
		}
		if it.Chain != nil {
			if err := writeCertFiles(filepath.Join(certDir, next.ID), it.Chain, it.Key); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: writing certificate files: %v", it.Name, err))
			}
		}
		certIDs[it.NPMID] = next.ID
		s.app.Audit(ctx, core.AuditEntry{Action: "certificate." + verb(action), Target: next.Name, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindCertificate, next.ID, next.Name, action)
	}

	// Proxy hosts.
	plannedLists := map[int]bool{}
	for _, it := range p.AccessLists {
		plannedLists[it.NPMID] = true
	}
	for _, it := range p.Hosts {
		next := it.Host
		next.AccessListID = accessIDs[it.AccessNPM]
		if plannedLists[it.AccessNPM] && next.AccessListID == "" {
			// Never import a protected host without its protection.
			fail("hosts", it.Name, errors.New("its access list could not be imported, so the host was skipped instead of being left unprotected"))
			continue
		}
		next.CertificateID = certIDs[it.CertNPM]
		if next.CertificateID == "" {
			next.ForceHTTPS = false
			next.HTTP2 = false
		}
		var prev *model.ProxyHost
		if it.Conflict != "" {
			if !overwrite || it.ExistingID == "" {
				res.Skipped["hosts"]++
				continue
			}
			var err error
			if prev, err = st.Hosts().Get(ctx, it.ExistingID); err != nil {
				fail("hosts", it.Name, err)
				continue
			}
			if prev.System {
				fail("hosts", it.Name, errors.New("Relay's own admin host cannot be overwritten"))
				continue
			}
			next.ID = prev.ID
		}
		if httpx.HostHooks.BeforeSave != nil {
			if err := httpx.HostHooks.BeforeSave(r, prev, &next); err != nil {
				fail("hosts", it.Name, err)
				continue
			}
		}
		if err := validate(&next); err != nil {
			fail("hosts", it.Name, err)
			continue
		}
		action := core.ActionCreated
		if prev == nil {
			if err := st.Hosts().Create(ctx, &next); err != nil {
				fail("hosts", it.Name, err)
				continue
			}
			res.Created["hosts"]++
		} else {
			action = core.ActionUpdated
			if err := st.Hosts().Update(ctx, &next); err != nil {
				fail("hosts", it.Name, err)
				continue
			}
			res.Updated["hosts"]++
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "host." + verb(action), Target: it.Name, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindHost, next.ID, it.Name, action)
		if httpx.HostHooks.AfterSave != nil {
			httpx.HostHooks.AfterSave(r, prev, &next)
		}
	}

	// Redirects.
	for _, it := range p.Redirects {
		next := it.Redirect
		next.CertificateID = certIDs[it.CertNPM]
		if next.CertificateID == "" {
			next.ForceHTTPS = false
		}
		var prev *model.Redirect
		if it.Conflict != "" {
			if !overwrite || it.ExistingID == "" {
				res.Skipped["redirects"]++
				continue
			}
			var err error
			if prev, err = st.Redirects().Get(ctx, it.ExistingID); err != nil {
				fail("redirects", it.Name, err)
				continue
			}
			next.ID = prev.ID
		}
		if httpx.RedirectHooks.BeforeSave != nil {
			if err := httpx.RedirectHooks.BeforeSave(r, prev, &next); err != nil {
				fail("redirects", it.Name, err)
				continue
			}
		}
		if err := validate(&next); err != nil {
			fail("redirects", it.Name, err)
			continue
		}
		action := core.ActionCreated
		if prev == nil {
			if err := st.Redirects().Create(ctx, &next); err != nil {
				fail("redirects", it.Name, err)
				continue
			}
			res.Created["redirects"]++
		} else {
			action = core.ActionUpdated
			if err := st.Redirects().Update(ctx, &next); err != nil {
				fail("redirects", it.Name, err)
				continue
			}
			res.Updated["redirects"]++
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "redirect." + verb(action), Target: it.Name, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindRedirect, next.ID, it.Name, action)
	}

	// Streams.
	for _, it := range p.Streams {
		next := it.Stream
		var prev *model.Stream
		if it.Conflict != "" {
			if !overwrite || it.ExistingID == "" {
				res.Skipped["streams"]++
				continue
			}
			var err error
			if prev, err = st.Streams().Get(ctx, it.ExistingID); err != nil {
				fail("streams", it.Name, err)
				continue
			}
			next.ID = prev.ID
		}
		if httpx.StreamHooks.BeforeSave != nil {
			if err := httpx.StreamHooks.BeforeSave(r, prev, &next); err != nil {
				fail("streams", it.Name, err)
				continue
			}
		}
		if err := validate(&next); err != nil {
			fail("streams", it.Name, err)
			continue
		}
		action := core.ActionCreated
		if prev == nil {
			if err := st.Streams().Create(ctx, &next); err != nil {
				fail("streams", it.Name, err)
				continue
			}
			res.Created["streams"]++
		} else {
			action = core.ActionUpdated
			if err := st.Streams().Update(ctx, &next); err != nil {
				fail("streams", it.Name, err)
				continue
			}
			res.Updated["streams"]++
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "stream." + verb(action), Target: next.Name, Detail: detail, Result: "saved"})
		s.app.Changed(ctx, model.KindStream, next.ID, next.Name, action)
	}

	total := 0
	for _, n := range res.Created {
		total += n
	}
	for _, n := range res.Updated {
		total += n
	}
	summary := fmt.Sprintf("%d hosts · %d redirects · %d streams · %d access lists · %d certificates",
		res.Created["hosts"]+res.Updated["hosts"], res.Created["redirects"]+res.Updated["redirects"], res.Created["streams"]+res.Updated["streams"],
		res.Created["accessLists"]+res.Updated["accessLists"], res.Created["certificates"]+res.Updated["certificates"])
	s.app.Audit(ctx, core.AuditEntry{Action: "import.npm", Target: "Nginx Proxy Manager", Detail: summary, Result: "ok"})
	if total > 0 {
		s.app.Activity(ctx, "import.npm", "ok", "Imported from Nginx Proxy Manager", "", summary)
	}
	return res, nil
}

func verb(action string) string {
	if action == core.ActionUpdated {
		return "update"
	}
	return "create"
}

func writeCertFiles(dir string, chain, key []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"fullchain.pem": chain, "privkey.pem": key} {
		mode := os.FileMode(0o644)
		if name == "privkey.pem" {
			mode = 0o600
		}
		tmp := filepath.Join(dir, "."+name+".tmp")
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
