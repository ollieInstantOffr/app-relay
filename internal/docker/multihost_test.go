package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"golang.org/x/crypto/ssh"

	"github.com/instantoffr/relay/internal/core"
	relayevents "github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func newTestApp(t *testing.T) (*core.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir}, st, relayevents.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return app, st
}

// sshServer starts an in-process SSH server that accepts any client key and
// rejects every channel; it returns its address and host key fingerprint.
func sshServer(t *testing.T) (string, string) {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					ch.Reject(ssh.Prohibited, "no channels in tests")
				}
			}()
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func TestSSHHostKeyTOFU(t *testing.T) {
	addr, fp := sshServer(t)
	_, clientKey := testSSHKey(t)
	ctx := t.Context()
	ep := model.DockerEndpoint{ID: "n1", Name: "nas", Type: model.DockerSSH, URL: "ssh://docker@" + addr, SSHKey: clientKey, Enabled: true}

	// First use: any key is accepted and its fingerprint reported.
	d, err := dialSSH(ctx, ep)
	if err != nil {
		t.Fatalf("first use: %v", err)
	}
	if d.fingerprint != fp {
		t.Errorf("fingerprint = %s, want %s", d.fingerprint, fp)
	}
	d.Close()

	// Trusted key matches.
	ep.SSHKnownHost = fp
	d, err = dialSSH(ctx, ep)
	if err != nil {
		t.Fatalf("known key: %v", err)
	}
	d.Close()

	// A different trusted key refuses the connection before authenticating.
	ep.SSHKnownHost = "SHA256:AAAAtrustedbeforeAAAA"
	_, err = dialSSH(ctx, ep)
	var mm *HostKeyMismatchError
	if !errors.As(err, &mm) || mm.Got != fp || mm.Expected != ep.SSHKnownHost {
		t.Fatalf("mismatch = %v", err)
	}

	// TestEndpoint surfaces the presented fingerprint so the UI can offer to re-trust.
	app, st := newTestApp(t)
	s := New(app)
	res, err := s.TestEndpoint(ctx, ep)
	if err != nil || res.OK || res.SSHFingerprint != fp || !strings.Contains(res.Error, "host key changed") {
		t.Fatalf("test endpoint mismatch: %v %+v", err, res)
	}

	// trustHostKey persists the first key only.
	set := store.DefaultDocker()
	ep.SSHKnownHost = ""
	set.Enabled, set.Endpoints = true, []model.DockerEndpoint{ep}
	st.PutSettings(ctx, model.SettingsDocker, set)
	c := newConn(s, ep)
	keyBefore := c.getKey()
	s.trustHostKey(ctx, c, fp)
	s.trustHostKey(ctx, c, "SHA256:other")
	got, _ := store.LoadSettings[model.DockerSettings](ctx, st, model.SettingsDocker)
	if got.Endpoints[0].SSHKnownHost != fp || c.getKey() == keyBefore {
		t.Fatalf("trusted = %q", got.Endpoints[0].SSHKnownHost)
	}
}

func TestMigrateLegacyEndpoint(t *testing.T) {
	app, st := newTestApp(t)
	ctx := t.Context()
	legacy := store.DefaultDocker()
	legacy.Enabled, legacy.Endpoint, legacy.AutoCreate, legacy.AutoRemove = true, "tcp://10.0.0.5:2375", true, true
	legacy.Endpoints = nil
	st.PutSettings(ctx, model.SettingsDocker, legacy)
	New(app).migrate(ctx)
	got, _ := store.LoadSettings[model.DockerSettings](ctx, st, model.SettingsDocker)
	if len(got.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v", got.Endpoints)
	}
	ep := got.Endpoints[0]
	if ep.ID != LocalEndpointID || ep.Type != model.DockerTCP || ep.URL != "tcp://10.0.0.5:2375" || !ep.Enabled || !ep.AutoCreate || !ep.AutoRemove || got.Endpoint != "tcp://10.0.0.5:2375" {
		t.Fatalf("migrated = %+v", got)
	}
	if effectiveUpstream(ep) != "10.0.0.5" {
		t.Errorf("upstream of migrated tcp endpoint = %q", effectiveUpstream(ep))
	}
}

func TestSettingsHookRedactsAndKeepsSecrets(t *testing.T) {
	app, _ := newTestApp(t)
	New(app).RegisterSettingsHook()
	h := httpx.SettingsHooks[model.SettingsDocker]
	_, sshKey := testSSHKey(t)
	stored := store.DefaultDocker()
	stored.Endpoints = []model.DockerEndpoint{
		{ID: "t1", Name: "pve", Type: model.DockerTLS, URL: "tcp://10.0.0.7:2376", TLSCA: "ca", TLSCert: "cert", TLSKey: "PRIVATE", Enabled: true},
		{ID: "s1", Name: "nas", Type: model.DockerSSH, URL: "ssh://docker@10.0.0.5", SSHKey: sshKey, SSHKnownHost: "SHA256:x", Enabled: true},
	}
	shown := h.Decorate(nil, &stored).(model.DockerSettings)
	if shown.Endpoints[0].TLSKey != "" || !shown.Endpoints[0].TLSKeySet || shown.Endpoints[0].TLSCert != "cert" ||
		shown.Endpoints[1].SSHKey != "" || !shown.Endpoints[1].SSHKeySet || shown.Endpoints[1].SSHKnownHost != "SHA256:x" {
		t.Fatalf("decorated = %+v", shown.Endpoints)
	}
	if stored.Endpoints[0].TLSKey != "PRIVATE" || stored.Endpoints[1].SSHKey == "" {
		t.Fatal("decorate mutated the stored settings")
	}
	// Saving the redacted payload keeps the SSH key (the fake TLS PEMs fail validation,
	// so only check the SSH endpoint here).
	next := shown
	next.Endpoints = shown.Endpoints[1:]
	if err := h.BeforeSave(nil, &stored, &next); err != nil {
		t.Fatal(err)
	}
	if next.Endpoints[0].SSHKey != sshKey || next.Endpoints[0].SSHKeySet {
		t.Fatalf("kept = %+v", next.Endpoints[0])
	}
}

func TestResolveUpstreamStoppedRemote(t *testing.T) {
	cases := []struct {
		name   string
		in     upstreamInput
		host   string
		port   int
		reason string
	}{
		{"stopped host network", upstreamInput{EndpointName: "nas", UpstreamAddress: "10.0.0.5", HostNetwork: true, Image: "jellyfin/jellyfin"},
			"10.0.0.5", 8096, "stopped · starts on 10.0.0.5:8096"},
		{"stopped unpublished", upstreamInput{EndpointName: "nas", UpstreamAddress: "10.0.0.5", Image: "traefik/whoami", Ports: []core.ContainerPort{cport(80, 0)}},
			"", 80, "stopped · no published port on nas — publish a port to proxy it"},
		{"loopback proxy counts as local", upstreamInput{Local: true, Running: true, HostNetwork: true, Image: "jellyfin/jellyfin"}, "127.0.0.1", 8096, ""},
	}
	for _, c := range cases {
		r := resolveUpstream(c.in)
		if r.Host != c.host || r.Port != c.port || r.Reason != c.reason {
			t.Errorf("%s: got %+v", c.name, r)
		}
	}
}

func TestLabelSyncPerEndpoint(t *testing.T) {
	app, st := newTestApp(t)
	ctx := context.Background()
	ep := model.DockerEndpoint{ID: "nas1", Name: "nas", Type: model.DockerTCP, URL: "tcp://10.0.0.5:2375", Enabled: true, AutoCreate: false}
	set := store.DefaultDocker()
	set.Enabled, set.Endpoints = true, []model.DockerEndpoint{ep}
	st.PutSettings(ctx, model.SettingsDocker, set)

	app1 := container.Summary{
		ID: "a1", Names: []string{"/app"}, Image: "traefik/whoami", State: "running", Labels: map[string]string{"relay.host": "app.nas.lan"},
		Ports:           []container.Port{{PrivatePort: 80, PublicPort: 28088, IP: "0.0.0.0", Type: "tcp"}},
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"bridge": {IPAddress: "172.17.0.2"}}},
	}
	off := stopped("o1", "offline", "traefik/whoami")
	off.Labels = map[string]string{"relay.host": "offline.nas.lan"}
	fake := &fakeAPI{containers: []container.Summary{app1, off}, inspect: map[string]container.InspectResponse{
		"o1": inspectWith("80/tcp", nil),
	}}
	s := New(app)
	c := newConn(s, ep)
	c.d = &dialed{api: fake}
	c.status.Connected = true
	s.conns[ep.ID] = c

	resync := func() {
		c.mu.Lock()
		c.cachedAt = time.Time{}
		c.mu.Unlock()
		s.resyncEndpoint(ctx, c, true)
	}
	resync()
	if hosts, _ := st.Hosts().List(ctx); len(hosts) != 0 {
		t.Fatalf("auto-create off still created %+v", hosts)
	}

	ep.AutoCreate = true
	set.Endpoints = []model.DockerEndpoint{ep}
	st.PutSettings(ctx, model.SettingsDocker, set)
	c.updateMeta(ep)
	resync()
	hosts, _ := st.Hosts().List(ctx)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if h := hosts[0]; h.Domains[0] != "app.nas.lan" || h.SourceRef != "nas/app" || h.Upstream.Host != "10.0.0.5" || h.Upstream.Port != 28088 {
		t.Fatalf("host = %+v", h)
	}
	list, err := s.Containers(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("containers = %v %+v", err, list)
	}
	for _, x := range list {
		if x.Name == "offline" && (x.State != "exited" || x.UpstreamHost != "" || !strings.HasPrefix(x.Reason, "stopped")) {
			t.Errorf("stopped container = %+v", x)
		}
		if x.Name == "app" && (x.HostID != hosts[0].ID || x.EndpointName != "nas") {
			t.Errorf("app container = %+v", x)
		}
	}
}
