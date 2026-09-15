package engines

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/instantoffr/relay/internal/core"
)

func TestParseHelperOutput(t *testing.T) {
	out := strings.Join([]string{
		"::url https://github.com/acme/relay.git",
		"::ref main",
		"::head 1111111111111111111111111111111111111111",
		"::remote 3333333333333333333333333333333333333333",
		"::behind 2",
		"::ahead 0",
		"::dirty 1",
		"::commit 3333333333333333333333333333333333333333\t3333333\tOllie\t2026-09-15T10:01:06+02:00\tAdd docs page",
		"::commit 2222222222222222222222222222222222222222\t2222222\tOllie\t2026-09-14T09:00:00+02:00\tFix\ttabs in subject",
		"::step pull",
		"::pulled 1111111111111111111111111111111111111111 3333333333333333333333333333333333333333",
		"#12 [build 4/6] RUN go build",
		"::step build",
		"::done 3333333333333333333333333333333333333333",
	}, "\n")
	r := parseHelperOutput(out)
	if r.Ref != "main" || r.Behind != 2 || r.Dirty != 1 || r.Head[:3] != "111" || r.Remote[:3] != "333" {
		t.Fatalf("fields = %+v", r)
	}
	if len(r.Commits) != 2 || r.Commits[0].Subject != "Add docs page" || r.Commits[0].Date == nil || r.Commits[1].Subject != "Fix\ttabs in subject" {
		t.Fatalf("commits = %+v", r.Commits)
	}
	if strings.Join(r.Steps, ",") != "pull,build" || r.PulledTo[:3] != "333" || !r.Done {
		t.Fatalf("steps/pulled/done = %v %q %v", r.Steps, r.PulledTo, r.Done)
	}
	if len(r.Log) != 1 || !strings.Contains(r.Log[0], "go build") {
		t.Fatalf("log = %v", r.Log)
	}

	fail := parseHelperOutput("::step pull\nerror: Your local changes would be overwritten\n::error Can't fast-forward to origin/main")
	if fail.Error != "Can't fast-forward to origin/main" || fail.Done {
		t.Fatalf("fail = %+v", fail)
	}
}

func TestGithubRemotes(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/ollieInstantOffr/app-relay.git":   "https://github.com/ollieInstantOffr/app-relay",
		"https://x-access-token:secret@github.com/acme/relay": "https://github.com/acme/relay",
		"git@github.com:acme/relay.git":                       "https://github.com/acme/relay",
		"https://gitlab.com/acme/relay.git":                   "",
	} {
		if got := githubRepoURL(in); got != want {
			t.Errorf("githubRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeRemote("https://user:tok@github.com/acme/relay.git"); strings.Contains(got, "tok") {
		t.Errorf("sanitizeRemote leaked credentials: %s", got)
	}
}

func TestSelfFromInspect(t *testing.T) {
	info := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: "abc", Name: "/relay"},
		Config: &container.Config{Labels: map[string]string{
			labelComposeProj:    "relay",
			labelComposeWorkDir: "/home/ollie/relay",
			labelComposeFiles:   "/home/ollie/relay/docker-compose.yml,/etc/relay/override.yml",
		}},
		Mounts: []container.MountPoint{{Destination: "/data", Source: "/var/lib/docker/volumes/relay_relay-data/_data"}, {Destination: dockerSocket, Source: "/run/docker.sock"}},
	}
	self, err := selfFromInspect(info)
	if err != nil {
		t.Fatal(err)
	}
	if self.Name != "relay" || self.WorkingDir != "/home/ollie/relay" || len(self.ConfigFiles) != 2 || self.DockerSock != "/run/docker.sock" {
		t.Fatalf("self = %+v", self)
	}
	info.Config.Labels = map[string]string{}
	if _, err := selfFromInspect(info); err != errNotCompose {
		t.Fatalf("non-compose container: err = %v", err)
	}
}

func TestFinishRelayInfo(t *testing.T) {
	info := core.RelayUpdateInfo{Branch: "main", Commit: "aaa", CheckoutRef: "main", CheckoutHead: "aaa", Behind: 3}
	finishRelayInfo(&info)
	if !info.UpdateAvailable || info.RebuildNeeded || !info.CanUpdate {
		t.Fatalf("behind: %+v", info)
	}

	info = core.RelayUpdateInfo{Branch: "main", Commit: "aaa", CheckoutRef: "main", CheckoutHead: "bbb"}
	finishRelayInfo(&info)
	if !info.UpdateAvailable || !info.RebuildNeeded {
		t.Fatalf("checkout newer than build: %+v", info)
	}

	info = core.RelayUpdateInfo{Branch: "main", CheckoutRef: "feature", CheckoutHead: "bbb", Behind: 1, WorkingDir: "/srv/relay"}
	finishRelayInfo(&info)
	if info.UpdateAvailable || info.CanUpdate || !strings.Contains(info.Blocker, "branch feature") {
		t.Fatalf("wrong branch: %+v", info)
	}

	info = core.RelayUpdateInfo{Branch: "main", Commit: "", CheckoutRef: "main", CheckoutHead: "bbb"}
	finishRelayInfo(&info)
	if info.UpdateAvailable {
		t.Fatalf("unknown build commit must not claim an update: %+v", info)
	}
}

func TestHelperScriptsAreValidShell(t *testing.T) {
	for name, script := range map[string]string{"check": relayCheckScript, "update": relayUpdateScript} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script: %v\n%s", name, err, out)
		}
	}
}
