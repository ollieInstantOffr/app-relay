package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// The "GET Bucket (List Objects)" example from the AWS Signature V4 docs.
func TestSigV4KnownVector(t *testing.T) {
	c := &s3Client{
		scheme: "https", host: "s3.amazonaws.com", region: "us-east-1", bucket: "examplebucket",
		accessKey: "AKIDEXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
	q := canonicalQuery(url.Values{"max-keys": {"2"}, "prefix": {"J"}})
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?"+q, nil)
	c.sign(req, "examplebucket.s3.amazonaws.com", "/", q, emptySHA256)
	auth := req.Header.Get("Authorization")
	if !strings.HasSuffix(auth, "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7") {
		t.Fatalf("authorization = %s", auth)
	}
	if !strings.Contains(auth, "Credential=AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("authorization = %s", auth)
	}
}

func TestNormalizeAndValidateS3(t *testing.T) {
	c := model.NormalizeBackupS3(model.BackupS3Settings{Endpoint: " minio.lan:9000/ ", Bucket: "relay", Prefix: "/backups/relay", AccessKeyID: "a", SecretAccessKey: "b"})
	if c.Endpoint != "https://minio.lan:9000" || c.Prefix != "backups/relay/" || c.Region != "us-east-1" {
		t.Fatalf("normalized = %+v", c)
	}
	if errs := model.ValidateBackupS3(c); len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	bad := model.NormalizeBackupS3(model.BackupS3Settings{Endpoint: "https://x.example.com/bucket", Bucket: "No_Caps"})
	errs := model.ValidateBackupS3(bad)
	for _, f := range []string{"s3.endpoint", "s3.bucket", "s3.accessKeyId", "s3.secretAccessKey"} {
		if errs[f] == "" {
			t.Errorf("expected error for %s, got %v", f, errs)
		}
	}
}

// fakeS3 is a path-style S3 server for one bucket.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	fail    int // respond with this status to everything
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKTEST/") || r.Header.Get("x-amz-date") == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+f.bucket)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchBucket</Code></Error>`)
		return
	}
	key := strings.TrimPrefix(rest, "/")
	switch {
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if r.Header.Get("x-amz-content-sha256") != hex.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<Error><Code>XAmzContentSHA256Mismatch</Code></Error>`)
			return
		}
		f.objects[key] = body
	case r.Method == http.MethodGet && key == "":
		prefix := r.URL.Query().Get("prefix")
		keys := []string{}
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
		for _, k := range keys {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-09-15T03:00:00.000Z</LastModified></Contents>`, k, len(f.objects[k]))
		}
		b.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
		io.WriteString(w, b.String())
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeS3) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

func TestBackupCopiesToS3(t *testing.T) {
	s, app := newTestService(t)
	ctx := context.Background()
	fake := &fakeS3{bucket: "relay-backups", objects: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	host := &model.ProxyHost{Domains: []string{"grafana.home.lan"}, Enabled: true, Upstream: model.Upstream{Scheme: "http", Host: "10.0.0.2", Port: 3000}, Source: model.SourceManual}
	if err := app.Store.Hosts().Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultBackup()
	set.Passphrase = "correct horse battery"
	set.S3 = model.NormalizeBackupS3(model.BackupS3Settings{
		Enabled: true, Endpoint: srv.URL, Bucket: "relay-backups", Prefix: "relay",
		AccessKeyID: "AKTEST", SecretAccessKey: "test-secret", PathStyle: true,
	})
	if err := app.Store.PutSettings(ctx, model.SettingsBackup, set); err != nil {
		t.Fatal(err)
	}

	// A new backup is written locally and copied to the bucket.
	row, err := s.Create(ctx, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if row.RemoteStatus != RemoteUploaded || row.RemoteKey != "relay/"+row.File {
		t.Fatalf("remote = %q %q %q", row.RemoteStatus, row.RemoteKey, row.RemoteError)
	}
	local, _ := os.ReadFile(filepath.Join(s.Dir(), row.File))
	if remote, ok := fake.get(row.RemoteKey); !ok || !bytes.Equal(remote, local) {
		t.Fatalf("bucket copy differs from local file (found %v)", ok)
	}
	if st := s.Status(ctx).S3; !st.Configured || st.LastUploadAt == nil || st.LastError != "" {
		t.Fatalf("s3 status = %+v", st)
	}

	// Browse bucket lists archives only and links them to snapshots.
	fake.objects["relay/notes.txt"] = []byte("x")
	fake.objects["relay/nested/relay-old.relay.age"] = []byte("x")
	objs, err := s.RemoteList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].BackupID != row.ID || objs[0].Name != row.File {
		t.Fatalf("remote list = %+v", objs)
	}

	// Without the local file, restore and download fall back to the S3 copy.
	if err := os.Remove(filepath.Join(s.Dir(), row.File)); err != nil {
		t.Fatal(err)
	}
	rc, _, _, err := s.Open(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if _, err := s.RestoreByID(ctx, row.ID, ""); err != nil {
		t.Fatalf("restore from S3 copy: %v", err)
	}
	if _, err := s.RestoreRemote(ctx, row.RemoteKey, "correct horse battery"); err != nil {
		t.Fatalf("restore from bucket: %v", err)
	}
	for _, key := range []string{"", "relay/notes.txt", "relay/../x.relay.age", "other/" + row.File} {
		if _, err := s.RestoreRemote(ctx, key, "correct horse battery"); err == nil {
			t.Errorf("RestoreRemote(%q) should fail", key)
		}
	}

	// A failed upload keeps the local backup and can be retried.
	fake.fail = http.StatusForbidden
	failed, err := s.Create(ctx, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "ok" || failed.RemoteStatus != RemoteFailed || !strings.Contains(failed.RemoteError, "AccessDenied") {
		t.Fatalf("failed upload row = %+v", failed)
	}
	if st := s.Status(ctx).S3; st.LastError == "" {
		t.Fatal("status should report the failed copy")
	}
	fake.fail = 0
	retried, err := s.Upload(ctx, failed.ID)
	if err != nil || retried.RemoteStatus != RemoteUploaded {
		t.Fatalf("retry = %+v, %v", retried, err)
	}

	// Deleting a snapshot removes the bucket copy too.
	if err := s.Delete(ctx, retried.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.get(retried.RemoteKey); ok {
		t.Fatal("bucket copy should be deleted")
	}

	// Connection test: the saved secret is used when none is sent.
	test := set.S3
	test.SecretAccessKey = ""
	res, err := s.TestS3(ctx, test)
	if err != nil || !res.OK || !res.CanList {
		t.Fatalf("test = %+v, %v", res, err)
	}
	if _, ok := fake.get("relay/" + probeName); ok {
		t.Fatal("test file should be removed")
	}
	test.Bucket = "missing-bucket"
	if res, err := s.TestS3(ctx, test); err != nil || res.OK || !strings.Contains(res.Message, "bucket") {
		t.Fatalf("test missing bucket = %+v, %v", res, err)
	}
}

func TestBackupSettingsHookKeepsS3Secret(t *testing.T) {
	RegisterSettingsHook()
	hook := httpx.SettingsHooks[model.SettingsBackup]
	r := httptest.NewRequest(http.MethodPut, "/api/settings/backup", nil)
	prev := store.DefaultBackup()
	prev.S3 = model.BackupS3Settings{Enabled: true, Bucket: "relay", AccessKeyID: "a", SecretAccessKey: "keep-me"}

	next := prev
	next.S3.SecretAccessKey = ""
	next.S3.Prefix = "relay"
	if err := hook.BeforeSave(r, &prev, &next); err != nil {
		t.Fatal(err)
	}
	if next.S3.SecretAccessKey != "keep-me" || !next.S3.SecretSet || next.S3.Prefix != "relay/" {
		t.Fatalf("next = %+v", next.S3)
	}
	shown := hook.Decorate(r, &next).(model.BackupSettings)
	if shown.S3.SecretAccessKey != "" || !shown.S3.SecretSet {
		t.Fatalf("decorated = %+v", shown.S3)
	}

	bad := prev
	bad.S3.Bucket = "Bad Bucket"
	if err := hook.BeforeSave(r, &prev, &bad); err == nil {
		t.Fatal("invalid bucket should be rejected")
	}
	off := prev
	off.S3 = model.BackupS3Settings{}
	if err := hook.BeforeSave(r, &prev, &off); err != nil {
		t.Fatalf("disabled S3 needs no fields: %v", err)
	}
}
