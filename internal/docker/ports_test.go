package docker

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

func TestDetectPorts(t *testing.T) {
	cases := []struct {
		name   string
		image  string
		ports  []core.ContainerPort
		labels map[string]string
		hints  Hints
		port   int
		http   bool
		reason string
		cands  []int
	}{
		{"env PORT", "acme/api", nil, nil, Hints{Env: []string{"NODE_ENV=production", "PORT=4000"}}, 4000, true, "", []int{4000}},
		{"env order", "acme/api", nil, nil, Hints{Env: []string{"SERVER_PORT=8081", "APP_PORT=7000"}}, 7000, true, "", []int{7000, 8081}},
		{"node image", "node:22-alpine", nil, nil, Hints{Command: "node -e require('http')"}, 3000, true, "", []int{3000}},
		{"npm command", "myorg/sondr-app:latest", nil, nil, Hints{Command: "docker-entrypoint.sh npm start"}, 3000, true, "", []int{3000}},
		{"flask", "myorg/api", nil, nil, Hints{Command: "flask run --host 0.0.0.0"}, 5000, true, "", []int{5000}},
		{"gunicorn", "python:3.12-slim", nil, nil, Hints{Command: "gunicorn app:app"}, 8000, true, "", []int{8000}},
		{"grafana hint", "grafana/grafana", nil, nil, Hints{}, 3000, true, "", []int{3000}},
		{"ordering", "myorg/app", []core.ContainerPort{{Private: 9229, Proto: "tcp"}, {Private: 5432, Proto: "tcp"}, {Private: 8080, Public: 18080, Proto: "tcp"}},
			nil, Hints{Env: []string{"PORT=3000"}, Exposed: []int{9229}}, 3000, true, "", []int{3000, 9229, 5432, 8080}},
		{"label beats env", "myorg/app", nil, map[string]string{"relay.port": "7000"}, Hints{Env: []string{"PORT=3000"}}, 7000, true, "", []int{7000, 3000}},
		{"exposed beats hint", "node:22", []core.ContainerPort{{Private: 8080, Proto: "tcp"}}, nil, Hints{Command: "node server.js"}, 8080, true, "", []int{8080, 3000}},
		{"non-HTTP warns", "postgres:16", []core.ContainerPort{{Private: 5432, Proto: "tcp"}}, nil, Hints{}, 5432, false, "not HTTP (5432)", []int{5432}},
		{"web beats db", "acme/app", []core.ContainerPort{{Private: 5432, Proto: "tcp"}, {Private: 4000, Proto: "tcp"}}, nil, Hints{}, 4000, true, "", []int{4000, 5432}},
		{"unknown", "busybox", nil, nil, Hints{Command: "sleep 3600"}, 0, false, "no port detected — enter the app's port", []int{}},
	}
	for _, c := range cases {
		g := Detect(c.image, c.ports, c.labels, c.hints)
		if g.Port != c.port || g.HTTP != c.http || g.Reason != c.reason || !slices.Equal(g.Candidates, c.cands) {
			t.Errorf("%s: got %+v", c.name, g)
		}
	}
}

func TestResolveAnyPort(t *testing.T) {
	node := Hints{Command: "node server.js"}
	cases := []struct {
		name   string
		in     upstreamInput
		host   string
		port   int
		link   bool
		reason string
		cands  []int
	}{
		{"local running no published port", upstreamInput{Local: true, Running: true, IP: "172.17.0.5", Image: "node:22-alpine", Hints: node}, "172.17.0.5", 3000, false, "", []int{3000}},
		{"local running unknown port keeps address", upstreamInput{Local: true, Running: true, IP: "172.17.0.6", Image: "busybox"}, "172.17.0.6", 0, false, "no port detected — enter the app's port", []int{}},
		{"local host network unknown", upstreamInput{Local: true, Running: true, HostNetwork: true, Image: "myorg/app"}, "127.0.0.1", 0, false, "no port detected — enter the app's port", []int{}},
		{"local running postgres listed", upstreamInput{Local: true, Running: true, IP: "172.17.0.7", Image: "postgres", Ports: []core.ContainerPort{cport(5432, 0)}}, "172.17.0.7", 5432, false, "not HTTP (5432)", []int{5432}},
		{"local stopped env port links on start", upstreamInput{Local: true, Image: "node:22-alpine", Hints: Hints{Env: []string{"PORT=3000"}}}, "", 3000, true, reasonLinkOnStart, []int{3000}},
		{"local stopped published", upstreamInput{Local: true, Image: "myorg/app", Hints: Hints{Env: []string{"PORT=3000"}}, Ports: []core.ContainerPort{cport(3000, 8300)}, Bindings: []binding{{3000, 8300, ""}}},
			"127.0.0.1", 8300, false, "stopped · starts on 127.0.0.1:8300", []int{8300}},
		{"remote published candidates", upstreamInput{EndpointName: "nas", UpstreamAddress: "10.0.0.5", Running: true, IP: "172.20.0.2", Image: "myorg/app",
			Hints: Hints{Env: []string{"PORT=3000"}}, Ports: []core.ContainerPort{cport(3000, 13000), cport(9229, 19229)}, Bindings: []binding{{3000, 13000, ""}, {9229, 19229, ""}}},
			"10.0.0.5", 13000, false, "", []int{13000, 19229}},
		{"remote stopped unpublished stays uncreatable", upstreamInput{EndpointName: "nas", UpstreamAddress: "10.0.0.5", Image: "node:22", Hints: node}, "", 3000, false,
			"stopped · no published port on nas — publish a port to proxy it", []int{3000}},
	}
	for _, c := range cases {
		r := resolveUpstream(c.in)
		if r.Host != c.host || r.Port != c.port || r.Link != c.link || r.Reason != c.reason || !slices.Equal(r.Candidates, c.cands) {
			t.Errorf("%s: got %+v", c.name, r)
		}
	}
	if placeholderHost("My_App.v2") != "my-app-v2" || placeholderHost("__") != "container" {
		t.Errorf("placeholder = %q / %q", placeholderHost("My_App.v2"), placeholderHost("__"))
	}
}

// eventAPI is a fake Docker API whose event stream the test controls.
type eventAPI struct {
	*fakeAPI
	msgs chan events.Message
}

func (e *eventAPI) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	return e.msgs, make(chan error)
}

func running(id, name, image, ip, cmd string, ports ...container.Port) container.Summary {
	return container.Summary{
		ID: id, Names: []string{"/" + name}, Image: image, Command: cmd, State: "running", Labels: map[string]string{}, Ports: ports,
		NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"bridge": {IPAddress: ip}}},
	}
}

func TestPendingLinkAndCustomPort(t *testing.T) {
	app, st := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ep := model.DockerEndpoint{ID: LocalEndpointID, Name: "local", Type: model.DockerSocket, URL: "unix:///var/run/docker.sock", Enabled: true}
	set := store.DefaultDocker()
	set.Enabled, set.KeepInSync, set.Endpoints = true, true, []model.DockerEndpoint{ep}
	st.PutSettings(ctx, model.SettingsDocker, set)

	worker := stopped("s1", "worker", "node:22-alpine")
	fake := &fakeAPI{
		containers: []container.Summary{
			running("n1", "app", "node:22-alpine", "172.17.0.5", "node -e require('http')"),
			worker,
			running("p1", "db", "postgres:16", "172.17.0.8", "postgres", container.Port{PrivatePort: 5432, Type: "tcp"}),
		},
		inspect: map[string]container.InspectResponse{
			"s1": {ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{}}, Config: &container.Config{Env: []string{"PORT=3000"}, Cmd: []string{"node", "worker.js"}}},
		},
	}
	api := &eventAPI{fakeAPI: fake, msgs: make(chan events.Message, 4)}
	s := New(app)
	c := newConn(s, ep)
	c.d = &dialed{api: api}
	c.status.Connected = true
	s.conns[ep.ID] = c

	list, err := s.Containers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]core.Container{}
	for _, x := range list {
		byName[x.Name] = x
	}
	if a := byName["app"]; a.UpstreamHost != "172.17.0.5" || a.SuggestedPort != 3000 || !a.HTTP {
		t.Fatalf("app = %+v", a)
	}
	if w := byName["worker"]; !w.LinkOnStart || w.UpstreamHost != "" || w.SuggestedPort != 3000 || w.Reason != reasonLinkOnStart {
		t.Fatalf("worker = %+v", w)
	}
	if d := byName["db"]; d.UpstreamHost != "172.17.0.8" || d.HTTP || d.Reason != "not HTTP (5432)" {
		t.Fatalf("db = %+v", d)
	}

	// Dialog payload: custom port for app, suggested port for the stopped worker, non-HTTP db.
	req := httptest.NewRequest("POST", "/api/docker/hosts", nil).WithContext(core.WithActor(ctx, core.Actor{Type: core.ActorUser, Name: "jonas", Role: core.RoleAdmin}))
	res, err := s.CreateHosts(req, CreateRequest{KeepInSync: true, Items: []CreateItem{
		{EndpointID: LocalEndpointID, ContainerID: "n1", Domain: "app.home.lan", Port: 4000},
		{EndpointID: LocalEndpointID, ContainerID: "s1", Domain: "worker.home.lan"},
		{EndpointID: LocalEndpointID, ContainerID: "p1", Domain: "db.home.lan", Port: 5432},
	}})
	if err != nil || len(res.Created) != 3 || len(res.Errors) != 0 {
		t.Fatalf("create: %v %+v", err, res)
	}
	ids := map[string]string{}
	for _, cr := range res.Created {
		ids[cr.Domain] = cr.HostID
	}
	if !res.Created[1].LinkOnStart || res.Created[1].Enabled || !res.Created[0].Enabled {
		t.Errorf("created flags = %+v", res.Created)
	}
	host := func(domain string) *model.ProxyHost {
		h, err := st.Hosts().Get(ctx, ids[domain])
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if h := host("app.home.lan"); !h.Enabled || h.Upstream.Host != "172.17.0.5" || h.Upstream.Port != 4000 {
		t.Fatalf("app host = %+v", h.Upstream)
	}
	if h := host("worker.home.lan"); h.Enabled || h.Upstream.Host != "worker" || h.Upstream.Port != 3000 || h.SourceRef != "local/worker" || h.Source != model.SourceDocker {
		t.Fatalf("pending host = %+v %v", h.Upstream, h.Enabled)
	}

	// Watch events: the worker starts, app is recreated with a new IP.
	go c.session(ctx, c.d)
	fake.mu.Lock()
	fake.containers[1] = running("s1", "worker", "node:22-alpine", "172.17.0.9", "node worker.js")
	fake.containers[0] = running("n2", "app", "node:22-alpine", "172.17.0.6", "node -e require('http')")
	fake.mu.Unlock()
	api.msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, Actor: events.Actor{ID: "s1", Attributes: map[string]string{"name": "worker"}}}
	api.msgs <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, Actor: events.Actor{ID: "n2", Attributes: map[string]string{"name": "app"}}}

	deadline := time.Now().Add(8 * time.Second)
	for {
		w, a := host("worker.home.lan"), host("app.home.lan")
		if w.Enabled && w.Upstream.Host == "172.17.0.9" && w.Upstream.Port == 3000 && a.Upstream.Host == "172.17.0.6" && a.Upstream.Port == 4000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not linked: worker=%+v enabled=%v app=%+v", w.Upstream, w.Enabled, a.Upstream)
		}
		time.Sleep(100 * time.Millisecond)
	}
	audit, _ := st.ListAudit(ctx, store.AuditQuery{})
	linked := false
	for _, a := range audit {
		if a.ActorType == core.ActorDocker && a.Target == "worker.home.lan" && strings.Contains(a.Detail, "linked to container worker (started)") {
			linked = true
		}
	}
	if !linked {
		t.Errorf("no docker audit row for the link: %+v", audit)
	}
	s.syncMu.Lock()
	m := s.loadManaged(ctx)
	s.syncMu.Unlock()
	if e := m.Hosts[ids["worker.home.lan"]]; e == nil || e.Link || e.Port != 3000 {
		t.Errorf("managed entry = %+v", e)
	}
}
