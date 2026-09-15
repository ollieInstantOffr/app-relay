package engines

// End-to-end tests of the self-update helper scripts against a real Docker
// daemon: a throwaway git "origin", a checkout with a compose project whose
// service plays the relay container, then check + update via helper
// containers. Opt in with RELAY_DOCKER_E2E=1 (set RELAY_E2E_DIR to a folder
// Docker can bind-mount, e.g. under $HOME on Docker Desktop/OrbStack).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/instantoffr/relay/internal/store"
)

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com", "GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type e2eProject struct {
	t       *testing.T
	name    string
	origin  string
	work    string
	self    *selfContainer
	ctx     context.Context
	cleanup []func()
}

// newE2EProject creates origin + checkout with a compose service "relay" built
// from the given Dockerfile, and starts it.
func newE2EProject(t *testing.T, dockerfile, serviceExtra string) *e2eProject {
	if os.Getenv("RELAY_DOCKER_E2E") != "1" {
		t.Skip("set RELAY_DOCKER_E2E=1 to run against Docker")
	}
	base := os.Getenv("RELAY_E2E_DIR")
	if base == "" {
		base = t.TempDir()
	}
	root, err := os.MkdirTemp(base, "selfupdate-")
	if err != nil {
		t.Fatal(err)
	}
	p := &e2eProject{t: t, name: "relaye2e" + strings.ToLower(store.NewID()[:6])}
	p.origin, p.work = filepath.Join(root, "origin"), filepath.Join(root, "work")
	t.Cleanup(func() { os.RemoveAll(root) })

	os.MkdirAll(p.origin, 0o755)
	run(t, p.origin, "git", "init", "-q", "-b", "main")
	p.write("Dockerfile", dockerfile)
	p.write("docker-compose.yml", fmt.Sprintf(`name: %[1]s
services:
  relay:
    build:
      context: .
      args:
        RELAY_COMMIT: ${RELAY_COMMIT:-}
    image: %[1]s-relay:latest
    container_name: %[1]s-relay
    command: ["sleep", "3600"]
%[2]s`, p.name, serviceExtra))
	run(t, p.origin, "git", "add", ".")
	run(t, p.origin, "git", "commit", "-qm", "initial")
	run(t, root, "git", "clone", "-q", p.origin, p.work)
	// The helper only mounts the checkout, so the stand-in "GitHub" origin
	// lives inside it (excluded from git).
	moved := filepath.Join(p.work, ".e2e-origin")
	if err := os.Rename(p.origin, moved); err != nil {
		t.Fatal(err)
	}
	p.origin = moved
	run(t, p.work, "git", "remote", "set-url", "origin", p.origin)
	if err := os.WriteFile(filepath.Join(p.work, ".git", "info", "exclude"), []byte(".e2e-origin/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, p.work, "docker", "compose", "-p", p.name, "up", "-d", "--build", "--wait")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "-p", p.name, "-f", filepath.Join(p.work, "docker-compose.yml"), "down", "--rmi", "all", "-t", "1").Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	p.ctx = ctx
	p.self = &selfContainer{Name: p.name + "-relay", Project: p.name, WorkingDir: p.work, ConfigFiles: []string{filepath.Join(p.work, "docker-compose.yml")}, DockerSock: dockerSocket}
	return p
}

func (p *e2eProject) write(name, body string) {
	if err := os.WriteFile(filepath.Join(p.origin, name), []byte(body), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// commit changes the Dockerfile on origin and returns the new head.
func (p *e2eProject) commit(dockerfile, msg string) string {
	p.write("Dockerfile", dockerfile)
	run(p.t, p.origin, "git", "commit", "-qam", msg)
	return run(p.t, p.origin, "git", "rev-parse", "HEAD")
}

func (p *e2eProject) helper(kind, script string, env ...string) (int64, helperResult) {
	p.t.Helper()
	cli, err := newDockerClient()
	if err != nil {
		p.t.Fatal(err)
	}
	defer cli.Close()
	if err := ensureImage(p.ctx, cli, updaterImage()); err != nil {
		p.t.Fatal(err)
	}
	id, err := startHelper(p.ctx, cli, p.self, kind, store.NewID(), script, env...)
	if err != nil {
		p.t.Fatal(err)
	}
	defer cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true})
	code, out, err := waitHelper(p.ctx, cli, id, 10*time.Minute)
	if err != nil {
		p.t.Fatalf("%s helper: %v\n%s", kind, err, out)
	}
	p.t.Logf("%s helper output:\n%s", kind, out)
	return code, parseHelperOutput(out)
}

func (p *e2eProject) inspect(format string) string {
	return run(p.t, p.work, "docker", "inspect", "-f", format, p.name+"-relay")
}

const e2eDockerfile = "FROM alpine:3.22\nARG RELAY_COMMIT=\nENV BUILT_FROM=${RELAY_COMMIT}\nRUN echo %s > /version%s\n"

func TestRelaySelfUpdateE2E(t *testing.T) {
	p := newE2EProject(t, fmt.Sprintf(e2eDockerfile, "v1", ""), "")
	newHead := p.commit(fmt.Sprintf(e2eDockerfile, "v2", ""), "Bump to v2")

	code, check := p.helper("check", relayCheckScript)
	if code != 0 || check.Error != "" || check.Behind != 1 || len(check.Commits) != 1 || check.Commits[0].Subject != "Bump to v2" || check.Ref != "main" {
		t.Fatalf("check: code %d, %+v", code, check)
	}

	code, upd := p.helper("update", relayUpdateScript, "RELAY_RESTART_ENGINES=0")
	if code != 0 || !upd.Done || upd.PulledTo != newHead {
		t.Fatalf("update: code %d, %+v", code, upd)
	}
	if head := run(t, p.work, "git", "rev-parse", "HEAD"); head != newHead {
		t.Fatalf("checkout HEAD = %s, want %s", head, newHead)
	}
	if env := p.inspect("{{range .Config.Env}}{{println .}}{{end}}"); !strings.Contains(env, "BUILT_FROM="+newHead) {
		t.Fatalf("relay container wasn't rebuilt from %s:\n%s", newHead, env)
	}
	if v := run(t, p.work, "docker", "exec", p.name+"-relay", "cat", "/version"); v != "v2" {
		t.Fatalf("/version = %q", v)
	}

	code, check = p.helper("check", relayCheckScript)
	if code != 0 || check.Behind != 0 {
		t.Fatalf("second check: code %d, %+v", code, check)
	}
}

// A new version that never becomes healthy is rolled back to the previous image.
func TestRelaySelfUpdateRollbackE2E(t *testing.T) {
	health := `    healthcheck:
      test: ["CMD", "test", "-f", "/healthy"]
      interval: 1s
      timeout: 1s
      retries: 2
`
	p := newE2EProject(t, fmt.Sprintf(e2eDockerfile, "v1", " && touch /healthy"), health)
	prevImage := p.inspect("{{.Image}}")
	p.commit(fmt.Sprintf(e2eDockerfile, "v2-broken", ""), "Break health")

	code, upd := p.helper("update", relayUpdateScript, "RELAY_RESTART_ENGINES=0")
	if code == 0 || upd.Done {
		t.Fatalf("update of a broken version succeeded: code %d, %+v", code, upd)
	}
	if last := upd.Steps[len(upd.Steps)-1]; last != "rollback" || !strings.Contains(upd.Error, "rolled back") {
		t.Fatalf("expected a rollback, steps %v error %q", upd.Steps, upd.Error)
	}
	if img := p.inspect("{{.Image}}"); img != prevImage {
		t.Fatalf("relay runs %s after rollback, want previous image %s", img, prevImage)
	}
	if v := run(t, p.work, "docker", "exec", p.name+"-relay", "cat", "/version"); v != "v1" {
		t.Fatalf("/version after rollback = %q", v)
	}
	if st := p.inspect("{{.State.Health.Status}}"); st != "healthy" {
		t.Fatalf("health after rollback = %q", st)
	}
}
