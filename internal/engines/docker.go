package engines

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Labels on engine containers (docker-compose.yml).
const (
	labelEngine       = "relay.engine"
	labelValidate     = "relay.upgrade-validate"
	labelComposeProj  = "com.docker.compose.project"
	labelComposeSvc   = "com.docker.compose.service"
	defaultDockerHost = "unix:///var/run/docker.sock"
	envComposeProject = "RELAY_COMPOSE_PROJECT"
)

func newDockerClient() (*client.Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if os.Getenv("DOCKER_HOST") == "" {
		opts = append(opts, client.WithHost(defaultDockerHost))
	}
	return client.NewClientWithOpts(opts...)
}

var containerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// selfContainerID finds the relay container's id from /proc/self/mountinfo
// (its /etc/hostname etc. are bind-mounted from /var/lib/docker/containers/<id>/).
func selfContainerID(mountinfo string) string {
	counts := map[string]int{}
	for _, m := range containerIDRe.FindAllStringSubmatch(mountinfo, -1) {
		counts[m[1]]++
	}
	best, n := "", 0
	for id, c := range counts {
		if c > n {
			best, n = id, c
		}
	}
	return best
}

// composeProject returns the compose project of the relay container ("" when unknown).
func composeProject(ctx context.Context, cli *client.Client) string {
	if p := os.Getenv(envComposeProject); p != "" {
		return p
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	id := selfContainerID(string(b))
	if id == "" {
		return ""
	}
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil || info.Config == nil {
		return ""
	}
	return info.Config.Labels[labelComposeProj]
}

type engineContainer struct {
	ID      string
	Name    string // without leading slash
	Image   string
	State   string
	Project string
}

// findEngineContainer locates the container labeled relay.engine=<engine>
// in the relay container's compose project.
func findEngineContainer(ctx context.Context, cli *client.Client, project, engine string) (*engineContainer, error) {
	args := filters.NewArgs(filters.Arg("label", labelEngine+"="+engine))
	if project != "" {
		args.Add("label", labelComposeProj+"="+project)
	}
	list, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, err
	}
	var found []engineContainer
	for _, c := range list {
		if _, ok := c.Labels[labelValidate]; ok {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if strings.Contains(name, "-old-") {
			continue
		}
		found = append(found, engineContainer{ID: c.ID, Name: name, Image: c.Image, State: c.State, Project: c.Labels[labelComposeProj]})
	}
	if len(found) == 0 {
		where := ""
		if project != "" {
			where = " in compose project " + project
		}
		return nil, fmt.Errorf("no container labeled %s=%s%s — update docker-compose.yml (see deploy/UPGRADES.md)", labelEngine, engine, where)
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].State == "running" && found[j].State != "running" })
	if len(found) > 1 && found[1].State == "running" {
		return nil, fmt.Errorf("%d running containers are labeled %s=%s; set %s so Relay knows which compose project is its own", len(found), labelEngine, engine, envComposeProject)
	}
	return &found[0], nil
}

// officialRef parses nginx:1.30.4-alpine / docker.io/library/nginx:… into
// (repo, tag). ok is false for non-official images.
func officialRef(ref string) (repo, tag string, ok bool) {
	r := ref
	if i := strings.Index(r, "@"); i >= 0 {
		r = r[:i]
	}
	for _, p := range []string{"docker.io/library/", "index.docker.io/library/", "registry-1.docker.io/library/", "library/"} {
		r = strings.TrimPrefix(r, p)
	}
	name, t, hasTag := strings.Cut(r, ":")
	if !hasTag {
		t = "latest"
	}
	if name != "nginx" && name != "haproxy" {
		return "", "", false
	}
	return name, t, true
}

func imageForVersion(engine, version string) string { return engine + ":" + version + "-alpine" }

// cloneSpec builds a container spec identical to old but running newImage.
// Env/labels that only came from the old image are dropped so the new image's
// own defaults apply.
func cloneSpec(old container.InspectResponse, oldImageEnv []string, oldImageLabels map[string]string, oldImageStopSignal, newImage string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	cfg := *old.Config
	cfg.Image = newImage
	imgEnv := map[string]bool{}
	for _, e := range oldImageEnv {
		imgEnv[e] = true
	}
	cfg.Env = nil
	for _, e := range old.Config.Env {
		if !imgEnv[e] {
			cfg.Env = append(cfg.Env, e)
		}
	}
	cfg.Labels = map[string]string{}
	for k, v := range old.Config.Labels {
		if iv, ok := oldImageLabels[k]; ok && iv == v {
			continue
		}
		cfg.Labels[k] = v
	}
	if oldImageStopSignal != "" && cfg.StopSignal == oldImageStopSignal {
		cfg.StopSignal = ""
	}
	var hc container.HostConfig
	if old.HostConfig != nil {
		hc = *old.HostConfig
	}
	var nc *network.NetworkingConfig
	if hc.NetworkMode.IsHost() {
		cfg.Hostname, cfg.Domainname = "", ""
	} else if old.NetworkSettings != nil && len(old.NetworkSettings.Networks) > 0 {
		nc = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
		for name, ep := range old.NetworkSettings.Networks {
			es := &network.EndpointSettings{}
			if ep != nil {
				es.Aliases = ep.Aliases
			}
			nc.EndpointsConfig[name] = es
		}
		cfg.Hostname = ""
	}
	return &cfg, &hc, nc
}

// tarFiles packs files under dir/ for CopyToContainer.
func tarFiles(dir string, files map[string]string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return nil, errors.New("invalid file path " + p)
		}
		full := dir + "/" + p
		parts := strings.Split(full, "/")
		for i := 1; i < len(parts); i++ {
			d := strings.Join(parts[:i], "/") + "/"
			if !dirs[d] {
				dirs[d] = true
				if err := tw.WriteHeader(&tar.Header{Name: d, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
					return nil, err
				}
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: full, Mode: 0o644, Size: int64(len(files[p])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(files[p])); err != nil {
			return nil, err
		}
	}
	return &buf, tw.Close()
}
