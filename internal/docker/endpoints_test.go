package docker

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

func testSSHKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "relay")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return sp, string(pem.EncodeToMemory(block))
}

func TestNormalizeSettings(t *testing.T) {
	s := model.DockerSettings{Enabled: true, Endpoint: "unix:///var/run/docker.sock", AutoCreate: true}
	if !NormalizeSettings(&s) {
		t.Fatal("legacy settings not migrated")
	}
	ep := s.Endpoints[0]
	if len(s.Endpoints) != 1 || ep.ID != LocalEndpointID || ep.Name != "local" || ep.Type != model.DockerSocket || !ep.Enabled || !ep.AutoCreate || ep.AutoRemove {
		t.Fatalf("migrated = %+v", s.Endpoints)
	}
	if NormalizeSettings(&s) {
		t.Fatal("normalize is not idempotent")
	}
	if !isLocal(ep) || effectiveUpstream(ep) != "" {
		t.Error("socket endpoint should be local")
	}

	tcp := model.DockerSettings{Endpoint: "tcp://10.0.0.5:2375"}
	NormalizeSettings(&tcp)
	if tcp.Endpoints[0].Type != model.DockerTCP || effectiveUpstream(tcp.Endpoints[0]) != "10.0.0.5" || isLocal(tcp.Endpoints[0]) {
		t.Errorf("legacy tcp = %+v", tcp.Endpoints[0])
	}
	proxy := model.DockerEndpoint{Type: model.DockerTCP, URL: "tcp://127.0.0.1:2375"}
	if !isLocal(proxy) || effectiveUpstream(proxy) != "" {
		t.Error("loopback socket proxy should be local")
	}
	proxy.UpstreamAddress = "192.168.1.10"
	if isLocal(proxy) || effectiveUpstream(proxy) != "192.168.1.10" {
		t.Error("explicit upstream address should win")
	}

	none := model.DockerSettings{}
	if NormalizeSettings(&none) || none.Endpoints == nil || len(none.Endpoints) != 0 {
		t.Errorf("empty settings = %+v", none)
	}

	dup := model.DockerSettings{Endpoints: []model.DockerEndpoint{{ID: "a", URL: "ssh://u@nas.lan"}, {ID: "a", URL: "tcp://10.0.0.9:2375"}}}
	NormalizeSettings(&dup)
	if dup.Endpoints[1].ID == "a" || dup.Endpoints[0].Type != model.DockerSSH || dup.Endpoints[1].Type != model.DockerTCP ||
		dup.Endpoints[0].Name != "nas.lan" || dup.Endpoints[1].Name != "10.0.0.9" {
		t.Errorf("normalized = %+v", dup.Endpoints)
	}
}

func cport(private, public int) core.ContainerPort {
	return core.ContainerPort{Private: private, Public: public, Proto: "tcp"}
}

func TestResolveUpstream(t *testing.T) {
	remote := func(in upstreamInput) upstreamInput {
		in.EndpointName, in.UpstreamAddress = "nas", "10.0.0.5"
		return in
	}
	cases := []struct {
		name   string
		in     upstreamInput
		host   string
		port   int
		http   bool
		reason string
	}{
		{"local running ip", upstreamInput{Local: true, Running: true, IP: "172.18.0.9", Image: "grafana/grafana", Ports: []core.ContainerPort{cport(3000, 3001)},
			Bindings: []binding{{3000, 3001, "0.0.0.0"}}}, "172.18.0.9", 3000, true, ""},
		{"local stopped published", upstreamInput{Local: true, Image: "nginx", Ports: []core.ContainerPort{cport(80, 8080)}, Bindings: []binding{{80, 8080, ""}}},
			"127.0.0.1", 8080, true, "stopped · starts on 127.0.0.1:8080"},
		{"local stopped none", upstreamInput{Local: true, Image: "nginx", Ports: []core.ContainerPort{cport(80, 0)}}, "", 80, true, reasonLinkOnStart},
		{"local stopped redis", upstreamInput{Local: true, Image: "redis", Ports: []core.ContainerPort{cport(6379, 0)}}, "", 6379, false, "stopped · not HTTP (6379)"},
		{"local host network", upstreamInput{Local: true, Running: true, HostNetwork: true, Image: "jellyfin/jellyfin"}, "127.0.0.1", 8096, true, ""},
		{"remote published", remote(upstreamInput{Running: true, IP: "172.20.0.5", Image: "grafana/grafana", Ports: []core.ContainerPort{cport(3000, 3001)},
			Bindings: []binding{{3000, 3001, "::"}, {3000, 3001, "0.0.0.0"}}}), "10.0.0.5", 3001, true, ""},
		{"remote stopped published", remote(upstreamInput{Image: "grafana/grafana", Ports: []core.ContainerPort{cport(3000, 3001)}, Bindings: []binding{{3000, 3001, ""}}}),
			"10.0.0.5", 3001, true, "stopped · starts on 10.0.0.5:3001"},
		{"remote host network", remote(upstreamInput{Running: true, HostNetwork: true, Image: "jellyfin/jellyfin"}), "10.0.0.5", 8096, true, ""},
		{"remote not published", remote(upstreamInput{Running: true, IP: "172.20.0.6", Image: "traefik/whoami", Ports: []core.ContainerPort{cport(80, 0)}}),
			"", 80, true, "no published port on nas — publish a port to proxy it"},
		{"remote loopback bind", remote(upstreamInput{Running: true, Image: "nginx", Ports: []core.ContainerPort{cport(80, 8080)}, Bindings: []binding{{80, 8080, "127.0.0.1"}}}),
			"", 80, true, "published on 127.0.0.1 only on nas"},
		{"remote specific bind", remote(upstreamInput{Running: true, Image: "nginx", Ports: []core.ContainerPort{cport(80, 8080)}, Bindings: []binding{{80, 8080, "192.168.1.20"}}}),
			"192.168.1.20", 8080, true, ""},
		{"remote other published port", remote(upstreamInput{Running: true, Image: "acme/app", Ports: []core.ContainerPort{cport(80, 0), cport(8081, 18081)},
			Bindings: []binding{{8081, 18081, "0.0.0.0"}}}), "10.0.0.5", 18081, true, ""},
		{"remote label port unpublished", remote(upstreamInput{Running: true, Image: "acme/app", Labels: map[string]string{"relay.port": "3000"},
			Ports: []core.ContainerPort{cport(3000, 0), cport(9000, 9000)}, Bindings: []binding{{9000, 9000, ""}}}),
			"", 3000, true, "no published port on nas — publish a port to proxy it"},
		{"remote redis published", remote(upstreamInput{Running: true, Image: "redis", Ports: []core.ContainerPort{cport(6379, 6379)}, Bindings: []binding{{6379, 6379, ""}}}),
			"10.0.0.5", 6379, false, "not HTTP (6379)"},
		{"remote without address", upstreamInput{EndpointName: "nas", Running: true, Image: "nginx", Ports: []core.ContainerPort{cport(80, 8080)}, Bindings: []binding{{80, 8080, ""}}},
			"", 80, true, "set an address for upstreams on nas"},
	}
	for _, c := range cases {
		r := resolveUpstream(c.in)
		if r.Host != c.host || r.Port != c.port || r.HTTP != c.http || r.Reason != c.reason {
			t.Errorf("%s: got %+v", c.name, r)
		}
	}
	if s := containerScheme(core.Container{UpstreamHost: "10.0.0.5", IP: "172.20.0.2", SuggestedPort: 8443, Ports: []core.ContainerPort{cport(443, 8443)}}); s != "https" {
		t.Errorf("scheme of published 443 = %s", s)
	}
}

func TestEndpointSecretsAndValidation(t *testing.T) {
	_, keyPEM := testSSHKey(t)
	prev := &model.DockerSettings{Endpoints: []model.DockerEndpoint{
		{ID: "n1", Name: "nas", Type: model.DockerSSH, URL: "ssh://docker@10.0.0.5", SSHKey: keyPEM, SSHKnownHost: "SHA256:abc", Enabled: true},
	}}
	shown := *prev
	RedactSettings(&shown)
	if shown.Endpoints[0].SSHKey != "" || !shown.Endpoints[0].SSHKeySet || prev.Endpoints[0].SSHKey == "" {
		t.Fatalf("redact: %+v / original %+v", shown.Endpoints[0], prev.Endpoints[0].SSHKey != "")
	}

	next := &model.DockerSettings{DomainPattern: "{name}.lan", Endpoints: []model.DockerEndpoint{shown.Endpoints[0],
		{Name: "PVE", Type: model.DockerTCP, URL: "tcp://10.0.0.7:2375", Enabled: true}}}
	if err := prepareSettings(prev, next); err != nil {
		t.Fatal(err)
	}
	if next.Endpoints[0].SSHKey != keyPEM || next.Endpoints[0].SSHKeySet || next.Endpoints[1].ID == "" || next.Endpoints[1].Name != "pve" {
		t.Fatalf("prepared = %+v", next.Endpoints)
	}
	if next.Endpoint != "ssh://docker@10.0.0.5" {
		t.Errorf("legacy endpoint = %q", next.Endpoint)
	}

	// Switching the type drops the SSH key and trusted fingerprint.
	sw := &model.DockerSettings{DomainPattern: "{name}.lan", Endpoints: []model.DockerEndpoint{{ID: "n1", Name: "nas", Type: model.DockerTCP, URL: "tcp://10.0.0.5:2375"}}}
	if err := prepareSettings(prev, sw); err != nil || sw.Endpoints[0].SSHKey != "" || sw.Endpoints[0].SSHKnownHost != "" {
		t.Fatalf("type switch: %v %+v", err, sw.Endpoints[0])
	}

	bad := &model.DockerSettings{DomainPattern: "static", Endpoints: []model.DockerEndpoint{
		{Name: "nas", Type: model.DockerSSH, URL: "ssh://10.0.0.5"},
		{Name: "nas", Type: model.DockerTLS, URL: "tcp://10.0.0.6:2376", TLSCert: "nope"},
		{Name: "x", Type: model.DockerTCP, URL: "tcp://10.0.0.8:2375", UpstreamAddress: "http://10.0.0.8"},
	}}
	err := prepareSettings(&model.DockerSettings{}, bad)
	var ve *model.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected validation error, got %v", err)
	}
	for _, f := range []string{"domainPattern", "endpoints.0.url", "endpoints.0.sshKey", "endpoints.1.name", "endpoints.1.tlsCert", "endpoints.2.upstreamAddress"} {
		if ve.Fields[f] == "" {
			t.Errorf("missing error for %s: %v", f, ve.Fields)
		}
	}
	if e := ValidateEndpoint(model.DockerEndpoint{Name: "local", Type: model.DockerSocket, URL: "unix:///var/run/docker.sock"}); len(e) != 0 {
		t.Errorf("valid socket endpoint: %v", e)
	}
}

func TestHostKeyDecision(t *testing.T) {
	if trust, err := hostKeyDecision("", "SHA256:new"); !trust || err != nil {
		t.Error("first use should trust")
	}
	if trust, err := hostKeyDecision("SHA256:same", "SHA256:same"); trust || err != nil {
		t.Error("known key should pass without re-trusting")
	}
	_, err := hostKeyDecision("SHA256:old", "SHA256:new")
	var mm *HostKeyMismatchError
	if !errors.As(err, &mm) || mm.Got != "SHA256:new" || !strings.Contains(err.Error(), "changed") {
		t.Errorf("mismatch = %v", err)
	}
}
