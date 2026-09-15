package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"github.com/instantoffr/relay/internal/core"
	relayevents "github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func ports(ps ...int) []core.ContainerPort {
	out := []core.ContainerPort{}
	for _, p := range ps {
		out = append(out, core.ContainerPort{Private: p, Proto: "tcp"})
	}
	return out
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		image  string
		ports  []core.ContainerPort
		labels map[string]string
		http   bool
		port   int
		scheme string
		reason string
	}{
		{"grafana hint", "grafana/grafana:11.2", ports(3000), nil, true, 3000, "http", ""},
		{"loki hint", "grafana/loki:3.0", ports(3100, 9095), nil, true, 3100, "http", ""},
		{"redis", "redis:7", ports(6379), nil, false, 6379, "http", "not HTTP (6379)"},
		{"postgres", "postgres:16", ports(5432), nil, false, 5432, "http", "not HTTP (5432)"},
		{"nginx prefers 80", "nginx:latest", ports(443, 80), nil, true, 80, "http", ""},
		{"https only", "someapp", ports(8443), nil, true, 8443, "https", ""},
		{"unknown port", "acme/exporter", ports(9100), nil, true, 9100, "http", ""},
		{"db + web", "acme/app", ports(5432, 8080), nil, true, 8080, "http", ""},
		{"label port", "redis:7", ports(6379), map[string]string{"relay.port": "8001"}, true, 8001, "http", ""},
		{"label scheme", "x", ports(8000), map[string]string{"relay.scheme": "https"}, true, 8000, "https", ""},
		{"no ports hint", "jellyfin/jellyfin", nil, nil, true, 8096, "http", ""},
		{"no ports", "busybox", nil, nil, false, 0, "", "no exposed ports"},
		{"udp only", "pihole-dns", []core.ContainerPort{{Private: 5353, Proto: "udp"}}, nil, false, 0, "http", "UDP only"},
	}
	for _, c := range cases {
		g := Classify(c.image, c.ports, c.labels)
		if g.HTTP != c.http || g.Port != c.port || g.Reason != c.reason || (c.scheme != "" && g.Scheme != c.scheme) {
			t.Errorf("%s: got %+v", c.name, g)
		}
	}
	if imageBase("ghcr.io/immich-app/immich-server:v1.2@sha256:abc") != "immich-server" {
		t.Error("imageBase")
	}
}

func TestPickIP(t *testing.T) {
	nets := map[string]*network.EndpointSettings{
		"bridge":      {IPAddress: "172.17.0.2"},
		"app_default": {IPAddress: "172.18.0.9"},
	}
	if ip := PickIP("bridge", nets); ip != "172.18.0.9" {
		t.Errorf("user network not preferred: %s", ip)
	}
	if ip := PickIP("default", map[string]*network.EndpointSettings{"bridge": {IPAddress: "172.17.0.2"}}); ip != "172.17.0.2" {
		t.Errorf("bridge fallback: %s", ip)
	}
	if ip := PickIP("host", nil); ip != "127.0.0.1" {
		t.Errorf("host mode: %s", ip)
	}
	both := map[string]*network.EndpointSettings{"b_net": {IPAddress: "10.1.0.2"}, "a_net": {IPAddress: "10.0.0.2"}}
	if ip := PickIP("b_net", both); ip != "10.1.0.2" {
		t.Errorf("network mode preference: %s", ip)
	}
}

func TestParseLabels(t *testing.T) {
	spec, ok, err := ParseLabels(map[string]string{
		"relay.host": "Grafana.home.lan, g.home.lan", "relay.port": "3000", "relay.tls": "letsencrypt",
		"relay.access": "lan-only", "relay.websockets": "true", "relay.scheme": "https",
	})
	if !ok || err != nil || len(spec.Domains) != 2 || spec.Domains[0] != "grafana.home.lan" || spec.Port != 3000 ||
		spec.TLS != "letsencrypt" || spec.Access != "lan-only" || spec.Websockets == nil || !*spec.Websockets || spec.Scheme != "https" {
		t.Fatalf("spec=%+v ok=%v err=%v", spec, ok, err)
	}
	if _, ok, _ := ParseLabels(map[string]string{"com.docker.compose.project": "x"}); ok {
		t.Error("no relay labels should not match")
	}
	if _, ok, _ := ParseLabels(map[string]string{"relay.host": "a.lan", "relay.enable": "false"}); ok {
		t.Error("relay.enable=false should disable")
	}
	spec, ok, err = ParseLabels(map[string]string{"relay.host": "a.lan", "relay.port": "99999", "relay.tls": "sometimes"})
	if !ok || err == nil || !strings.Contains(err.Error(), "relay.port") || !strings.Contains(err.Error(), "relay.tls") || spec.TLS != "auto" {
		t.Errorf("invalid labels: %+v %v %v", spec, ok, err)
	}
	spec, ok, _ = ParseLabels(map[string]string{"relay.backend": "web-app", "relay.port": "8080"})
	if !ok || spec.Backend != "web-app" {
		t.Errorf("backend label: %+v", spec)
	}
	if _, ok, _ := ParseLabels(map[string]string{"relay.host": "not a domain!"}); ok {
		t.Error("invalid host only should not match")
	}
	if DomainFromPattern("{name}.home.lan", "Uptime_Kuma") != "uptime-kuma.home.lan" {
		t.Error("domain pattern")
	}
	if !certCovers([]string{"*.home.lan"}, "loki.home.lan") || certCovers([]string{"*.home.lan"}, "a.b.home.lan") || certCovers([]string{"*.home.lan"}, "home.lan") {
		t.Error("wildcard coverage")
	}
}

// ---------------------------------------------------------------- reconcile

type fakeAPI struct {
	mu         sync.Mutex
	containers []container.Summary
	inspect    map[string]container.InspectResponse
}

func (f *fakeAPI) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := o.Filters.Get("name")
	out := []container.Summary{}
	for _, c := range f.containers {
		if len(names) > 0 && "^"+c.Names[0]+"$" != names[0] {
			continue
		}
		if !o.All && c.State != "running" {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeAPI) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.inspect[id]; ok {
		return r, nil
	}
	return container.InspectResponse{}, errors.New("no such container")
}

func (f *fakeAPI) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	return make(chan events.Message), make(chan error)
}
func (f *fakeAPI) ServerVersion(ctx context.Context) (types.Version, error) {
	return types.Version{Version: "27.1"}, nil
}
func (f *fakeAPI) Close() error { return nil }

func summary(id, name, ip string, port uint16, labels map[string]string) container.Summary {
	return container.Summary{
		ID: id, Names: []string{"/" + name}, Image: "grafana/grafana", State: "running", Labels: labels,
		Ports:           []container.Port{{PrivatePort: port, Type: "tcp"}},
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"app": {IPAddress: ip}}},
	}
}

func stopped(id, name, image string) container.Summary {
	sm := container.Summary{ID: id, Names: []string{"/" + name}, Image: image, State: "exited", Labels: map[string]string{}}
	sm.HostConfig.NetworkMode = "bridge"
	return sm
}

func inspectWith(exposed string, bindings nat.PortMap) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{PortBindings: bindings}},
		Config:            &container.Config{ExposedPorts: nat.PortSet{nat.Port(exposed): struct{}{}}},
	}
}

func TestReconcileMultiEndpoint(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := core.New(core.Config{DataDir: dir, RunDir: dir, LogDir: dir}, st, relayevents.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	set := store.DefaultDocker()
	set.KeepInSync = true
	set.Endpoints = []model.DockerEndpoint{
		{ID: LocalEndpointID, Name: "local", Type: model.DockerSocket, URL: "unix:///var/run/docker.sock", Enabled: true, AutoCreate: true, AutoRemove: true},
		{ID: "nas1", Name: "nas", Type: model.DockerTCP, URL: "tcp://10.0.0.5:2375", Enabled: true, AutoCreate: true, AutoRemove: false},
	}
	st.PutSettings(ctx, model.SettingsDocker, set)
	wild := &model.Certificate{Name: "*.home.lan", Domains: []string{"*.home.lan"}, Provider: model.CertLetsEncrypt, Status: model.CertStatusValid}
	st.Certificates().Create(ctx, wild)
	lan := &model.AccessList{Name: "lan-only"}
	st.AccessLists().Create(ctx, lan)
	web := &model.Backend{Name: "web-app", Mode: "http", Algorithm: "roundrobin", HealthCheck: model.HealthCheck{Type: "http", Method: "GET", Path: "/"}}
	st.Backends().Create(ctx, web)

	local := &fakeAPI{
		containers: []container.Summary{
			summary("c1", "loki", "172.18.0.9", 3100, map[string]string{"relay.host": "loki.home.lan", "relay.port": "3100", "relay.access": "lan-only"}),
			summary("c2", "api-4", "172.18.0.14", 9000, map[string]string{"relay.backend": "web-app"}),
			summary("c3", "redis", "172.18.0.3", 6379, nil),
			stopped("s1", "old", "nginx:latest"),
			stopped("s2", "noport", "traefik/whoami"),
		},
		inspect: map[string]container.InspectResponse{
			"s1": inspectWith("80/tcp", nat.PortMap{"80/tcp": []nat.PortBinding{{HostPort: "8080"}}}),
			"s2": inspectWith("80/tcp", nil),
		},
	}
	grafana := container.Summary{
		ID: "g1", Names: []string{"/grafana"}, Image: "grafana/grafana", State: "running",
		Labels:          map[string]string{"relay.host": "grafana.nas.lan"},
		Ports:           []container.Port{{PrivatePort: 3000, PublicPort: 3001, IP: "0.0.0.0", Type: "tcp"}, {PrivatePort: 3000, PublicPort: 3001, IP: "::", Type: "tcp"}},
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"stack": {IPAddress: "172.20.0.5"}}},
	}
	jelly := container.Summary{ID: "j1", Names: []string{"/jellyfin"}, Image: "jellyfin/jellyfin", State: "running", Labels: map[string]string{}}
	jelly.HostConfig.NetworkMode = "host"
	internal := summary("i1", "whoami", "172.20.0.6", 80, nil)
	internal.Image = "traefik/whoami"
	nas := &fakeAPI{containers: []container.Summary{grafana, jelly, internal}}

	s := New(app)
	s.removeDelay = 0
	fakes := map[string]*fakeAPI{LocalEndpointID: local, "nas1": nas}
	conns := map[string]*endpointConn{}
	for _, ep := range set.Endpoints {
		c := newConn(s, ep)
		c.d = &dialed{api: fakes[ep.ID]}
		c.status.Connected = true
		s.conns[ep.ID] = c
		conns[ep.ID] = c
	}
	resync := func() {
		for _, c := range conns {
			c.mu.Lock()
			c.cachedAt = time.Time{}
			c.mu.Unlock()
			s.resyncEndpoint(ctx, c, true)
		}
	}
	resync()

	hosts, _ := st.Hosts().List(ctx)
	byDomain := map[string]model.ProxyHost{}
	for _, h := range hosts {
		byDomain[h.Domains[0]] = h
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}
	if h := byDomain["loki.home.lan"]; h.SourceRef != "local/loki" || h.Upstream.Host != "172.18.0.9" || h.Upstream.Port != 3100 ||
		h.CertificateID != wild.ID || h.AccessListID != lan.ID || !h.ForceHTTPS {
		t.Fatalf("loki host = %+v", h)
	}
	if h := byDomain["grafana.nas.lan"]; h.SourceRef != "nas/grafana" || h.Upstream.Host != "10.0.0.5" || h.Upstream.Port != 3001 || h.Source != model.SourceDocker {
		t.Fatalf("grafana host = %+v", h)
	}
	b, _ := st.Backends().Get(ctx, web.ID)
	if len(b.Servers) != 1 || b.Servers[0].Address != "172.18.0.14" || b.Servers[0].Port != 9000 || !b.Servers[0].Check {
		t.Fatalf("backend servers = %+v", b.Servers)
	}
	audit, _ := st.ListAudit(ctx, store.AuditQuery{})
	found := false
	for _, a := range audit {
		if a.Action == "host.create" && a.ActorType == core.ActorDocker && a.Detail == "from labels on container grafana on nas" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows: %+v", audit)
	}

	list, err := s.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]core.Container{}
	for _, c := range list {
		byName[c.Name] = c
	}
	if len(list) != 8 {
		t.Fatalf("containers = %d", len(list))
	}
	check := func(name, host string, port int, reason string) {
		t.Helper()
		c := byName[name]
		if c.UpstreamHost != host || (host != "" && c.SuggestedPort != port) || c.Reason != reason {
			t.Errorf("%s: upstream=%s:%d reason=%q", name, c.UpstreamHost, c.SuggestedPort, c.Reason)
		}
	}
	check("loki", "172.18.0.9", 3100, "")
	check("old", "127.0.0.1", 8080, "stopped · starts on 127.0.0.1:8080")
	check("noport", "", 0, "stopped — no published port")
	check("grafana", "10.0.0.5", 3001, "")
	check("jellyfin", "10.0.0.5", 8096, "")
	check("whoami", "", 0, "no published port on nas — publish a port to proxy it")
	check("redis", "172.18.0.3", 6379, "not HTTP (6379)")
	if byName["loki"].HostID != byDomain["loki.home.lan"].ID || byName["grafana"].HostID != byDomain["grafana.nas.lan"].ID ||
		byName["api-4"].BackendID != web.ID || byName["grafana"].EndpointName != "nas" || byName["old"].State != "exited" {
		t.Errorf("decorated = %+v", list)
	}

	st2 := s.Status(ctx)
	if !st2.Connected || st2.Containers != 8 || st2.Running != 6 || len(st2.Endpoints) != 2 || st2.Endpoints[1].UpstreamAddress != "10.0.0.5" || st2.Endpoints[0].Running != 3 {
		t.Errorf("status = %+v", st2)
	}

	// Recreated containers: new IP locally, new published port on nas.
	local.mu.Lock()
	local.containers[0] = summary("c1b", "loki", "172.18.0.30", 3100, local.containers[0].Labels)
	local.containers[1] = summary("c2b", "api-4", "172.18.0.31", 9000, local.containers[1].Labels)
	local.mu.Unlock()
	nas.mu.Lock()
	nas.containers[0].Ports = []container.Port{{PrivatePort: 3000, PublicPort: 3002, IP: "0.0.0.0", Type: "tcp"}}
	nas.mu.Unlock()
	resync()
	hosts, _ = st.Hosts().List(ctx)
	for _, h := range hosts {
		byDomain[h.Domains[0]] = h
	}
	b, _ = st.Backends().Get(ctx, web.ID)
	if len(hosts) != 2 || byDomain["loki.home.lan"].Upstream.Host != "172.18.0.30" || byDomain["grafana.nas.lan"].Upstream.Port != 3002 ||
		len(b.Servers) != 1 || b.Servers[0].Address != "172.18.0.31" {
		t.Fatalf("after recreate: hosts=%+v servers=%+v", hosts, b.Servers)
	}

	// Label change updates the domain.
	local.mu.Lock()
	local.containers[0].Labels = map[string]string{"relay.host": "logs.home.lan", "relay.port": "3100"}
	local.mu.Unlock()
	resync()
	if h, _ := st.Hosts().Get(ctx, byDomain["loki.home.lan"].ID); h == nil || h.Domains[0] != "logs.home.lan" {
		t.Fatalf("after label change: %+v", h)
	}

	// Bulk create: stopped container with a published port works; unpublished remote one doesn't.
	req := httptest.NewRequest("POST", "/api/docker/hosts", nil).WithContext(core.WithActor(ctx, core.Actor{Type: core.ActorUser, Name: "jonas", Role: core.RoleAdmin}))
	res, err := s.CreateHosts(req, CreateRequest{Items: []CreateItem{
		{EndpointID: LocalEndpointID, ContainerID: "s1", Domain: "old.home.lan"},
		{EndpointID: "nas1", ContainerID: "i1", Domain: "whoami.nas.lan"},
		{EndpointID: LocalEndpointID, ContainerID: "c1b", Domain: "logs.home.lan"},
	}, KeepInSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || len(res.Errors) != 2 || !strings.Contains(res.Errors[0].Error, "no published port on nas") || !strings.Contains(res.Errors[1].Error, "already used") {
		t.Fatalf("bulk result = %+v", res)
	}
	created, _ := st.Hosts().Get(ctx, res.Created[0].HostID)
	if created.Upstream.Host != "127.0.0.1" || created.Upstream.Port != 8080 || created.SourceRef != "local/old" {
		t.Fatalf("created from stopped container = %+v", created)
	}

	// Destroy: local (auto-remove on) removes host + server; nas (auto-remove off) keeps its host.
	local.mu.Lock()
	local.containers = local.containers[2:]
	local.mu.Unlock()
	nas.mu.Lock()
	nas.containers = nas.containers[1:]
	nas.mu.Unlock()
	s.scheduleRemoval(LocalEndpointID, "loki", time.Now().Add(-time.Minute))
	s.scheduleRemoval(LocalEndpointID, "api-4", time.Now().Add(-time.Minute))
	s.scheduleRemoval("nas1", "grafana", time.Now().Add(-time.Minute))
	s.processRemovals(ctx)
	hosts, _ = st.Hosts().List(ctx)
	b, _ = st.Backends().Get(ctx, web.ID)
	domains := []string{}
	for _, h := range hosts {
		domains = append(domains, h.Domains[0])
	}
	if len(hosts) != 2 || !slices.Contains(domains, "grafana.nas.lan") || !slices.Contains(domains, "old.home.lan") || len(b.Servers) != 0 {
		t.Fatalf("after destroy: hosts=%v servers=%+v", domains, b.Servers)
	}
}
