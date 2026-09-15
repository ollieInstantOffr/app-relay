package backup

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Off-site copies: every backup is written locally first, then copied to the
// S3 destination. Retention and deletes apply to both copies.

const (
	RemoteUploading = "uploading"
	RemoteUploaded  = "uploaded"
	RemoteFailed    = "failed"

	uploadTimeout = 15 * time.Minute
	maxRemoteSize = 4 << 30
	probeName     = ".relay-connection-test"
)

// S3Status summarises the off-site destination for the settings page.
type S3Status struct {
	Enabled      bool       `json:"enabled"`
	Configured   bool       `json:"configured"`
	Bucket       string     `json:"bucket,omitempty"`
	Prefix       string     `json:"prefix,omitempty"`
	Endpoint     string     `json:"endpoint,omitempty"`
	LastUploadAt *time.Time `json:"lastUploadAt,omitempty"`
	LastError    string     `json:"lastError,omitempty"` // set when the newest upload attempt failed
}

// S3TestResult is the outcome of a connection test.
type S3TestResult struct {
	OK      bool   `json:"ok"`
	CanList bool   `json:"canList"`
	Message string `json:"message"`
}

func s3Configured(c model.BackupS3Settings) bool {
	return c.Bucket != "" && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

func s3Status(set model.BackupS3Settings, rows []store.BackupRow) S3Status {
	st := S3Status{Enabled: set.Enabled, Configured: s3Configured(set), Bucket: set.Bucket, Prefix: set.Prefix, Endpoint: set.Endpoint}
	attempted := false
	for _, r := range rows {
		if r.RemoteStatus != RemoteUploaded && r.RemoteStatus != RemoteFailed {
			continue
		}
		if !attempted {
			attempted = true
			if r.RemoteStatus == RemoteFailed {
				st.LastError = r.RemoteError
			}
		}
		if r.RemoteStatus == RemoteUploaded {
			t := r.CreatedAt
			st.LastUploadAt = &t
			break
		}
	}
	return st
}

func s3Err(err error) error {
	if _, ok := err.(*httpx.HTTPError); ok {
		return err
	}
	// Not 502: an error page on the proxy in front of Relay would replace the message.
	return httpx.Errorf(http.StatusBadRequest, "s3_error", err.Error())
}

// s3 returns a client for the saved destination, whether copies are on or not.
func (s *Service) s3(ctx context.Context) (*s3Client, error) {
	set := s.settings(ctx)
	if !s3Configured(set.S3) {
		return nil, httpx.Errorf(http.StatusBadRequest, "s3_not_configured", "Set up an S3 destination first")
	}
	return s.newClient(set.S3)
}

func (s *Service) newClient(c model.BackupS3Settings) (*s3Client, error) {
	cl, err := newS3Client(c)
	if err != nil {
		return nil, httpx.Errorf(http.StatusBadRequest, "s3_not_configured", err.Error())
	}
	return cl, nil
}

// upload copies a finished local backup to S3 and records the result on row.
// It runs to completion even if the request that started it goes away.
func (s *Service) upload(ctx context.Context, dest model.BackupS3Settings, row *store.BackupRow, file string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadTimeout)
	defer cancel()
	err := func() error {
		c, err := s.newClient(dest)
		if err != nil {
			return err
		}
		row.RemoteKey = c.Key(row.File)
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		return c.Put(ctx, row.RemoteKey, f)
	}()
	if err != nil {
		row.RemoteStatus, row.RemoteError = RemoteFailed, err.Error()
		s.app.Log.Warn("backup upload to S3", "file", row.File, "err", err)
		s.app.Activity(ctx, "backup.upload_failed", "error", "Backup copy to S3 failed", "", row.File+": "+err.Error())
		s.notifyFailure(ctx, "Backup couldn't be copied to S3", row.File+": "+err.Error())
		return
	}
	row.RemoteStatus, row.RemoteError = RemoteUploaded, ""
}

func (s *Service) notifyFailure(ctx context.Context, title, msg string) {
	if s.app.Notify == nil {
		return
	}
	s.app.Notify.Notify(ctx, core.Notification{Event: model.EventBackupFailed, Level: "error", Title: title, Message: msg, URL: "/settings/backup"})
}

// Upload copies an existing local backup to S3 again (after a failed upload,
// or one made before S3 was set up).
func (s *Service) Upload(ctx context.Context, id string) (*store.BackupRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.settings(ctx)
	if !set.S3.Enabled || !s3Configured(set.S3) {
		return nil, httpx.Errorf(http.StatusBadRequest, "s3_disabled", "Turn on copies to S3 first")
	}
	r, err := s.app.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status != "ok" {
		return nil, httpx.Errorf(http.StatusConflict, "not_ready", "backup is "+r.Status)
	}
	full := filepath.Join(s.Dir(), filepath.Base(r.File))
	if _, err := os.Stat(full); err != nil {
		return nil, httpx.Errorf(http.StatusNotFound, "file_missing", "The local backup file is missing, so it can't be uploaded")
	}
	r.RemoteStatus, r.RemoteError = RemoteUploading, ""
	if err := s.app.Store.UpdateBackup(ctx, *r); err != nil {
		return nil, err
	}
	s.app.Bus.Publish(events.BackupChanged, map[string]any{"id": id, "status": RemoteUploading})
	s.upload(ctx, set.S3, r, full)
	if err := s.app.Store.UpdateBackup(context.WithoutCancel(ctx), *r); err != nil {
		return nil, err
	}
	s.app.Bus.Publish(events.BackupChanged, map[string]any{"id": id, "status": r.RemoteStatus})
	if r.RemoteStatus == RemoteFailed {
		return r, httpx.Errorf(http.StatusBadRequest, "upload_failed", r.RemoteError)
	}
	return r, nil
}

// removeRemote deletes the S3 copy of a backup that is being removed. Copies
// are left alone while S3 is turned off.
func (s *Service) removeRemote(ctx context.Context, r store.BackupRow) {
	if r.RemoteKey == "" || r.RemoteStatus != RemoteUploaded {
		return
	}
	set := s.settings(ctx)
	if !set.S3.Enabled || !s3Configured(set.S3) {
		return
	}
	c, err := s.newClient(set.S3)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := c.Delete(ctx, r.RemoteKey); err != nil {
		s.app.Log.Warn("delete backup from S3", "key", r.RemoteKey, "err", err)
	}
}

// Open returns a finished backup archive: the local file, or its S3 copy when
// the local file is gone. The caller closes it.
func (s *Service) Open(ctx context.Context, id string) (io.ReadCloser, int64, *store.BackupRow, error) {
	r, err := s.app.Store.GetBackup(ctx, id)
	if err != nil {
		return nil, 0, nil, err
	}
	if r.Status != "ok" {
		return nil, 0, nil, httpx.Errorf(http.StatusConflict, "not_ready", "backup is "+r.Status)
	}
	if f, err := os.Open(filepath.Join(s.Dir(), filepath.Base(r.File))); err == nil {
		if info, err := f.Stat(); err == nil {
			return f, info.Size(), r, nil
		}
		f.Close()
	}
	if r.RemoteStatus != RemoteUploaded || r.RemoteKey == "" {
		return nil, 0, nil, httpx.Errorf(http.StatusNotFound, "file_missing", "backup file is missing from "+s.Dir())
	}
	c, err := s.s3(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	body, size, err := c.Get(ctx, r.RemoteKey)
	if err != nil {
		return nil, 0, nil, httpx.Errorf(http.StatusBadRequest, "s3_error", "The local file is missing and the S3 copy couldn't be read: "+err.Error())
	}
	return body, size, r, nil
}

// RemoteList lists the backup archives in the bucket, newest first.
func (s *Service) RemoteList(ctx context.Context) ([]RemoteObject, error) {
	c, err := s.s3(ctx)
	if err != nil {
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	objs, err := c.List(lctx, 0)
	if err != nil {
		return nil, s3Err(err)
	}
	byKey := map[string]string{}
	if rows, err := s.app.Store.ListBackups(ctx); err == nil {
		for _, r := range rows {
			if r.RemoteKey != "" && r.RemoteStatus == RemoteUploaded {
				byKey[r.RemoteKey] = r.ID
			}
		}
	}
	for i := range objs {
		objs[i].BackupID = byKey[objs[i].Key]
	}
	return objs, nil
}

// RestoreRemote restores an archive straight from the bucket, e.g. on a new
// machine that has no local backups yet.
func (s *Service) RestoreRemote(ctx context.Context, key, passphrase string) (*RestoreResult, error) {
	c, err := s.s3(ctx)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(key, c.prefix)
	if key == "" || !strings.HasPrefix(key, c.prefix) || strings.Contains(name, "/") || strings.Contains(key, "..") || !strings.HasSuffix(name, fileExt) {
		return nil, httpx.Errorf(http.StatusBadRequest, "bad_request", "Pick a "+fileExt+" backup from the bucket")
	}
	body, _, err := c.Get(ctx, key)
	if err != nil {
		return nil, s3Err(err)
	}
	defer body.Close()
	return s.Restore(ctx, io.LimitReader(body, maxRemoteSize), passphrase)
}

// TestS3 checks a destination by uploading, listing and deleting a small
// file. An empty secret uses the saved one.
func (s *Service) TestS3(ctx context.Context, dest model.BackupS3Settings) (*S3TestResult, error) {
	if strings.TrimSpace(dest.SecretAccessKey) == "" {
		dest.SecretAccessKey = s.settings(ctx).S3.SecretAccessKey
	}
	dest = model.NormalizeBackupS3(dest)
	if err := model.ValidateBackupS3(dest).Err(); err != nil {
		return nil, err
	}
	c, err := s.newClient(dest)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	probe := c.Key(probeName)
	if err := c.putBytes(ctx, probe, []byte("Relay backup connection test. Safe to delete.\n")); err != nil {
		return &S3TestResult{Message: "Upload failed: " + err.Error()}, nil
	}
	res := &S3TestResult{OK: true, CanList: true, Message: "Connected. Backups will be copied to s3://" + c.bucket + "/" + c.prefix}
	if _, err := c.List(ctx, 1); err != nil {
		res.CanList = false
		res.Message = "Uploads work, but listing the bucket was denied, so Browse bucket won't work: " + err.Error()
	}
	if err := c.Delete(ctx, probe); err != nil {
		res.Message += ". Deleting the test file failed, so Relay can't remove old backups from the bucket: " + err.Error()
	}
	return res, nil
}
