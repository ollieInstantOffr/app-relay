package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func TestGeneralSettingsProxyEngine(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	req := httptest.NewRequest("PUT", "/api/settings/general", nil)
	prev := store.DefaultGeneral()

	empty := prev
	empty.ProxyEngine = ""
	if err := s.generalBeforeSave(req, &prev, &empty); err != nil || empty.ProxyEngine != "nginx" {
		t.Fatalf("empty engine: %v (%q)", err, empty.ProxyEngine)
	}

	var ve *model.ValidationError
	bad := prev
	bad.ProxyEngine = "caddy"
	if err := s.generalBeforeSave(req, &prev, &bad); !errors.As(err, &ve) || ve.Fields["proxyEngine"] == "" {
		t.Fatalf("unknown engine: %v", err)
	}

	edge := prev
	edge.ProxyEngine = "Edge"
	if err := s.generalBeforeSave(req, &prev, &edge); err != nil || edge.ProxyEngine != "edge" {
		t.Fatalf("edge: %v (%q)", err, edge.ProxyEngine)
	}

	// Hosts with custom nginx snippets block Relay Edge (listed, at most 5).
	for _, d := range []string{"a.home.lan", "b.home.lan", "c.home.lan", "d.home.lan", "e.home.lan", "f.home.lan"} {
		if err := s.app.Store.Hosts().Create(ctx, &model.ProxyHost{Domains: []string{d}, Enabled: true, CustomNginx: "add_header X-Test 1;"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.app.Store.Hosts().Create(ctx, &model.ProxyHost{Domains: []string{"plain.home.lan"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	edge.ProxyEngine = "edge"
	err := s.generalBeforeSave(req, &prev, &edge)
	if !errors.As(err, &ve) {
		t.Fatalf("snippets: %v", err)
	}
	msg := ve.Fields["proxyEngine"]
	if !strings.HasPrefix(msg, "Relay Edge can't run custom nginx snippets: remove them from ") || !strings.HasSuffix(msg, " and 1 more") ||
		strings.Count(msg, ".home.lan") != 5 || strings.Contains(msg, "plain.home.lan") {
		t.Fatalf("message = %q", msg)
	}
	// Staying on nginx is unaffected.
	nginx := prev
	if err := s.generalBeforeSave(req, &prev, &nginx); err != nil {
		t.Fatalf("nginx with snippets: %v", err)
	}
}

func TestOwnedByProxy(t *testing.T) {
	for p, want := range map[string]bool{"nginx": true, "nginx: master process": true, "relay": true, "/opt/relay/bin/relay": true, "caddy": false, "relayd": false, "": false} {
		if got := ownedByProxy(p); got != want {
			t.Errorf("ownedByProxy(%q) = %v", p, got)
		}
	}
}
