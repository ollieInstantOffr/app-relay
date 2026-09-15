package auth

import (
	"context"
	"errors"
	"net/http/httptest"
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

	// Hosts with custom nginx snippets don't block switching in either
	// direction: Relay Edge skips the snippets and nginx applies them again.
	if err := s.app.Store.Hosts().Create(ctx, &model.ProxyHost{Domains: []string{"a.home.lan"}, Enabled: true, CustomNginx: "add_header X-Test 1;"}); err != nil {
		t.Fatal(err)
	}
	edge.ProxyEngine = "edge"
	if err := s.generalBeforeSave(req, &prev, &edge); err != nil {
		t.Fatalf("edge with snippets: %v", err)
	}
	back := edge
	back.ProxyEngine = "nginx"
	if err := s.generalBeforeSave(req, &edge, &back); err != nil {
		t.Fatalf("back to nginx with snippets: %v", err)
	}
}

func TestOwnedByProxy(t *testing.T) {
	for p, want := range map[string]bool{"nginx": true, "nginx: master process": true, "relay": true, "/opt/relay/bin/relay": true, "caddy": false, "relayd": false, "": false} {
		if got := ownedByProxy(p); got != want {
			t.Errorf("ownedByProxy(%q) = %v", p, got)
		}
	}
}

func TestGeneralSettingsLBEngine(t *testing.T) {
	s, _ := newTestService(t)
	req := httptest.NewRequest("PUT", "/api/settings/general", nil)
	prev := store.DefaultGeneral()
	if prev.LBEngine != "haproxy" {
		t.Fatalf("default lb engine = %q", prev.LBEngine)
	}

	empty := prev
	empty.LBEngine = ""
	if err := s.generalBeforeSave(req, &prev, &empty); err != nil || empty.LBEngine != "haproxy" {
		t.Fatalf("empty lb engine: %v (%q)", err, empty.LBEngine)
	}

	var ve *model.ValidationError
	bad := prev
	bad.LBEngine = "envoy"
	if err := s.generalBeforeSave(req, &prev, &bad); !errors.As(err, &ve) || ve.Fields["lbEngine"] != "Pick HAProxy or Relay Balancer" {
		t.Fatalf("unknown lb engine: %v", err)
	}

	bal := prev
	bal.LBEngine = " Balancer "
	if err := s.generalBeforeSave(req, &prev, &bal); err != nil || bal.LBEngine != "balancer" {
		t.Fatalf("balancer: %v (%q)", err, bal.LBEngine)
	}
	back := bal
	back.LBEngine = "haproxy"
	if err := s.generalBeforeSave(req, &bal, &back); err != nil || back.LBEngine != "haproxy" {
		t.Fatalf("back to haproxy: %v", err)
	}
}
