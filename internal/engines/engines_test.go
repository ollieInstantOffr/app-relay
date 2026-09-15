package engines

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"github.com/instantoffr/relay/internal/core"
)

func TestChannels(t *testing.T) {
	rel := []core.EngineRelease{{Version: "1.30.4"}, {Version: "1.31.5"}, {Version: "1.30.10"}, {Version: "1.29.9"}, {Version: "garbage"}}
	if r := newestInChannel("nginx", "stable", rel); r == nil || r.Version != "1.30.10" {
		t.Fatalf("nginx stable = %+v", r)
	}
	if r := newestInChannel("nginx", "mainline", rel); r == nil || r.Version != "1.31.5" {
		t.Fatalf("nginx mainline = %+v", r)
	}
	hap := []core.EngineRelease{{Version: "3.2.23"}, {Version: "3.3.14"}, {Version: "3.4.4"}, {Version: "2.8.28"}}
	if r := newestInChannel("haproxy", "lts", hap); r == nil || r.Version != "3.4.4" {
		t.Fatalf("haproxy lts = %+v", r)
	}
	hap = hap[:2]
	if r := newestInChannel("haproxy", "lts", hap); r == nil || r.Version != "3.2.23" {
		t.Fatalf("haproxy lts = %+v", r)
	}
	if r := newestInChannel("haproxy", "latest", hap); r == nil || r.Version != "3.3.14" {
		t.Fatalf("haproxy latest = %+v", r)
	}
	a, _ := parseVersion("1.9.12")
	b, _ := parseVersion("1.10.0")
	if !a.less(b) || b.less(a) {
		t.Fatal("semver compare")
	}
}

func TestHubReleasesPaginationAndETag(t *testing.T) {
	var hits, notModified atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("name") != "alpine" || !strings.HasPrefix(r.URL.Path, "/v2/namespaces/library/repositories/nginx/tags") {
			http.Error(w, "bad", 400)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			w.Write([]byte(`{"next":null,"results":[{"name":"1.29.8-alpine","tag_last_pushed":"2099-01-01T00:00:00Z"},{"name":"1.30.4-alpine-slim","last_updated":"2099-01-01T00:00:00Z"}]}`))
			return
		}
		if r.Header.Get("If-None-Match") == `"p1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"p1"`)
		w.Write([]byte(`{"next":"` + srv.URL + `/v2/namespaces/library/repositories/nginx/tags?page_size=100&name=alpine&page=2","results":[
			{"name":"1.31.5-alpine","tag_last_pushed":"2099-09-03T22:51:27Z"},
			{"name":"stable-alpine","tag_last_pushed":"2099-09-03T22:51:27Z"},
			{"name":"1.30.4-alpine","tag_last_pushed":"2099-09-03T22:50:53Z"},
			{"name":"1.30-alpine","tag_last_pushed":"2099-09-03T22:50:53Z"},
			{"name":"1.30.4-alpine","tag_last_pushed":"2099-09-03T22:50:53Z"}]}`))
	}))
	defer srv.Close()
	h := newHubClient()
	h.base = srv.URL
	for i := 0; i < 2; i++ {
		rel, err := h.releases(context.Background(), "nginx")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rel {
			got = append(got, r.Image)
		}
		if strings.Join(got, ",") != "nginx:1.31.5-alpine,nginx:1.30.4-alpine,nginx:1.29.8-alpine" {
			t.Fatalf("releases = %v", got)
		}
		if rel[0].Date == nil {
			t.Fatal("missing date")
		}
	}
	if notModified.Load() != 1 {
		t.Fatalf("expected the second check to use the ETag (304s: %d, hits: %d)", notModified.Load(), hits.Load())
	}

	h.base = "http://127.0.0.1:1"
	if _, err := h.releases(context.Background(), "nginx"); err == nil {
		t.Fatal("offline must be an error")
	}
}

func TestOfficialRef(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx:1.30.4-alpine":                     "nginx 1.30.4-alpine true",
		"docker.io/library/haproxy:3.2.23-alpine": "haproxy 3.2.23-alpine true",
		"nginx":                   "nginx latest true",
		"relay-nginx:latest":      "  false",
		"ghcr.io/foo/nginx:1.2.3": "  false",
		"nginx@sha256:abc":        "nginx latest true",
	} {
		repo, tag, ok := officialRef(ref)
		if got := strings.Join([]string{repo, tag, map[bool]string{true: "true", false: "false"}[ok]}, " "); got != want {
			t.Errorf("officialRef(%q) = %q, want %q", ref, got, want)
		}
	}
	if changesURL("haproxy", "3.4.4") != "https://www.haproxy.org/download/3.4/src/CHANGELOG" || changesURL("nginx", "1.30.4") != "https://nginx.org/en/CHANGES" {
		t.Fatal("changes url")
	}
}

func TestSelfContainerID(t *testing.T) {
	id := strings.Repeat("a1", 32)
	other := strings.Repeat("b2", 32)
	mi := "1 2 0:1 / / rw - overlay overlay rw\n" +
		"3 1 0:2 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw\n" +
		"4 1 0:2 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw\n" +
		"5 1 0:2 /var/lib/docker/containers/" + other + "/x /x rw\n"
	if got := selfContainerID(mi); got != id {
		t.Fatalf("id = %q", got)
	}
	if selfContainerID("nothing") != "" {
		t.Fatal("expected empty")
	}
}

func TestCloneSpec(t *testing.T) {
	old := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Name: "/relay-nginx", HostConfig: &container.HostConfig{NetworkMode: "host", RestartPolicy: container.RestartPolicy{Name: "unless-stopped"}}},
		Config: &container.Config{
			Image: "nginx:1.28.0-alpine", Hostname: "orbstack",
			Env:          []string{"PATH=/usr/sbin", "NGINX_VERSION=1.28.0", "TZ=UTC"},
			Labels:       map[string]string{"maintainer": "NGINX Docker Maintainers", "relay.engine": "nginx", "com.docker.compose.project": "relay"},
			Entrypoint:   []string{"sh", "-c", "exec /opt/relay/bin/relay agent --engine nginx"},
			Cmd:          []string{"nginx", "-g", "daemon off;"},
			StopSignal:   "SIGTERM",
			ExposedPorts: map[nat.Port]struct{}{"80/tcp": {}},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"host": {}}},
	}
	img := imageDefaults{
		Env: []string{"PATH=/usr/sbin", "NGINX_VERSION=1.28.0"}, Labels: map[string]string{"maintainer": "NGINX Docker Maintainers"},
		StopSignal: "SIGQUIT", Cmd: []string{"nginx", "-g", "daemon off;"}, Entrypoint: []string{"/docker-entrypoint.sh"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}},
	}
	cfg, hc, nc := cloneSpec(old, img, "nginx:1.30.4-alpine")
	if cfg.Image != "nginx:1.30.4-alpine" || strings.Join(cfg.Env, ",") != "TZ=UTC" || cfg.Hostname != "" || cfg.StopSignal != "SIGTERM" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Cmd != nil || len(cfg.Entrypoint) != 3 || cfg.ExposedPorts != nil {
		t.Fatalf("image defaults copied: cmd=%v entrypoint=%v ports=%v", cfg.Cmd, cfg.Entrypoint, cfg.ExposedPorts)
	}
	if _, ok := cfg.Labels["maintainer"]; ok || cfg.Labels["relay.engine"] != "nginx" || cfg.Labels["com.docker.compose.project"] != "relay" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	if hc.RestartPolicy.Name != "unless-stopped" || nc != nil {
		t.Fatalf("hc = %+v nc = %+v", hc, nc)
	}
	if old.Config.Image != "nginx:1.28.0-alpine" || len(old.Config.Env) != 3 || len(old.Config.ExposedPorts) != 1 {
		t.Fatal("cloneSpec mutated the original")
	}

	// compose network_mode: service:netns → container:<id>
	old.HostConfig = &container.HostConfig{NetworkMode: container.NetworkMode("container:" + strings.Repeat("c", 64)), DNS: []string{"1.1.1.1"}, ExtraHosts: []string{"a:1.2.3.4"}}
	old.Config.Hostname = "abc"
	old.Config.ExposedPorts = map[nat.Port]struct{}{"80/tcp": {}, "9000/tcp": {}}
	old.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}}
	cfg, hc, nc = cloneSpec(old, img, "nginx:1.30.4-alpine")
	if cfg.Hostname != "" || cfg.ExposedPorts != nil || hc.DNS != nil || hc.ExtraHosts != nil || nc != nil || !hc.NetworkMode.IsContainer() {
		t.Fatalf("container mode: cfg=%+v hc=%+v nc=%+v", cfg, hc, nc)
	}

	// user-defined bridge network: endpoints + aliases carried over, generated hostname dropped.
	id := strings.Repeat("d", 64)
	old.ID = id
	old.HostConfig = &container.HostConfig{NetworkMode: "relay_default"}
	old.Config.Hostname = id[:12]
	old.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"relay_default": {Aliases: []string{"nginx"}}}}
	cfg, _, nc = cloneSpec(old, img, "nginx:1.30.4-alpine")
	if cfg.Hostname != "" || nc == nil || strings.Join(nc.EndpointsConfig["relay_default"].Aliases, ",") != "nginx" || len(cfg.ExposedPorts) != 1 {
		t.Fatalf("bridge: cfg=%+v nc=%+v", cfg, nc)
	}
}

func TestTarFiles(t *testing.T) {
	buf, err := tarFiles("relay-validate", map[string]string{"nginx.conf": "events {}\n", "conf.d/hosts/a.conf": "x"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(buf)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	want := "relay-validate/,relay-validate/conf.d/,relay-validate/conf.d/hosts/,relay-validate/conf.d/hosts/a.conf,relay-validate/nginx.conf"
	if strings.Join(names, ",") != want {
		t.Fatalf("names = %v", names)
	}
	if _, err := tarFiles("x", map[string]string{"../etc/passwd": ""}); err == nil {
		t.Fatal("expected unsafe path error")
	}
}

func TestInstallFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("binary"), 0o644)
	dst := filepath.Join(dir, "bin", "relay")
	if err := installFile(src, dst); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Mode().Perm() != 0o755 || !same(src, dst) {
		t.Fatalf("install: %v %v", st, err)
	}
}

func TestValidSummary(t *testing.T) {
	out := "[NOTICE]   (1) : haproxy version is 3.4.4\n[WARNING]  (1) : config : The 'master-worker' keyword is deprecated\nWarnings were found."
	if got := validSummary("haproxy", out); got != "haproxy -c passed · 1 warning" {
		t.Fatalf("got %q", got)
	}
	if got := validSummary("nginx", "nginx: configuration file nginx.conf test is successful"); got != "nginx -t passed" {
		t.Fatalf("got %q", got)
	}
}

func TestInactiveEngine(t *testing.T) {
	cases := []struct {
		engine, proxy, lb string
		want              bool
	}{
		{"nginx", "nginx", "haproxy", false},
		{"nginx", "edge", "haproxy", true},
		{"haproxy", "nginx", "haproxy", false},
		{"haproxy", "nginx", "", false},
		{"haproxy", "nginx", "balancer", true},
		{"balancer", "nginx", "balancer", false},
	}
	for _, c := range cases {
		if got := inactiveEngine(c.engine, c.proxy, c.lb); got != c.want {
			t.Errorf("inactiveEngine(%s, %s, %s) = %v", c.engine, c.proxy, c.lb, got)
		}
	}
	if !containerEngine("balancer") || containerLabel("balancer") != "Relay Balancer" {
		t.Fatal("balancer container engine")
	}
}
