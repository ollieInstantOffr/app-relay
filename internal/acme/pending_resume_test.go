package acme

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// TestRequestResumesStuckPending: a pending ACME certificate nobody is issuing
// (e.g. imported from Nginx Proxy Manager) starts when it is requested again;
// while it is being issued, a second request is refused.
func TestRequestResumesStuckPending(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir}, st, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(app)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.baseCtx = ctx
	// Hold every issuance slot so launched orders wait instead of contacting an ACME server.
	for i := 0; i < cap(s.sem); i++ {
		s.sem <- struct{}{}
	}

	stuck := &model.Certificate{Name: "app.example.com", Domains: []string{"app.example.com"}, Provider: model.CertLetsEncrypt,
		Challenge: model.ChallengeHTTP01, AutoRenew: true, Status: model.CertStatusPending, History: []model.CertEvent{}}
	if err := st.Certificates().Create(ctx, stuck); err != nil {
		t.Fatal(err)
	}
	req := core.CertRequest{Domains: []string{"app.example.com"}, Challenge: model.ChallengeHTTP01, AutoRenew: true}
	actx := core.WithActor(ctx, core.Actor{Type: core.ActorUser, Name: "admin", Role: core.RoleAdmin})

	got, err := s.Request(actx, req)
	if err != nil {
		t.Fatalf("request for stuck pending cert: %v", err)
	}
	if got.ID != stuck.ID || !s.isInflight(stuck.ID) {
		t.Fatalf("got %s (want %s), inflight=%v", got.ID, stuck.ID, s.isInflight(stuck.ID))
	}
	if list, _ := st.Certificates().List(ctx); len(list) != 1 {
		t.Fatalf("a duplicate certificate was created: %d", len(list))
	}

	_, err = s.Request(actx, req)
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("second request while issuing = %v, want 409", err)
	}

	// The scheduler's sweep skips certificates already being issued.
	s.resumePending(ctx)
	if !s.isInflight(stuck.ID) {
		t.Fatal("sweep dropped the in-flight order")
	}
}
