package engines

// Relay self-update: Relay finds its own compose project folder on the Docker
// host (compose labels on the relay container) and runs short-lived helper
// containers there. A check does `git fetch` and lists new commits on the
// update branch; an update fast-forwards the checkout, rebuilds the image and
// runs `docker compose up -d` (see relay_update.go). The helper outlives the
// relay container it recreates, and the restarted Relay reads the result.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	kvRelayInfo = "engines.relay.info"
	kvRelayJob  = "engines.relay.lastUpdate"

	labelSelfUpdate     = "relay.self-update" // check | update
	labelSelfUpdateJob  = "relay.self-update.job"
	labelComposeWorkDir = "com.docker.compose.project.working_dir"
	labelComposeFiles   = "com.docker.compose.project.config_files"

	envUpdaterImage     = "RELAY_UPDATER_IMAGE"
	envUpdateBranch     = "RELAY_UPDATE_BRANCH"
	defaultUpdaterImage = "docker:29.8.0-cli" // docker CLI + compose + buildx on Alpine
	dockerSocket        = "/var/run/docker.sock"
)

func updaterImage() string {
	if v := strings.TrimSpace(os.Getenv(envUpdaterImage)); v != "" {
		return v
	}
	return defaultUpdaterImage
}

func updateBranch() string {
	if v := strings.TrimSpace(os.Getenv(envUpdateBranch)); v != "" {
		return v
	}
	return "main"
}

// ---------------------------------------------------------------- helper scripts

// helperPreamble prepares git in the checkout. Git runs as the owner of the
// folder so pulled files don't end up owned by root on the host.
const helperPreamble = `set -u
fail() { echo "::error $*"; exit 1; }
apk add --no-cache git su-exec >/dev/null 2>&1 || fail "Couldn't install git in the updater container. Is the Docker host online?"
cd "$RELAY_WORKDIR" 2>/dev/null || fail "The compose project folder $RELAY_WORKDIR doesn't exist on the Docker host."
[ -e .git ] || fail "$RELAY_WORKDIR is not a git checkout, so Relay can't pull updates. Clone the repository there or update manually."
OWNER=$(stat -c %u:%g .)
as_owner() { su-exec "$OWNER" env HOME=/tmp GIT_TERMINAL_PROMPT=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*' "$@"; }
g() { as_owner git "$@"; }
# relay_version REV prints the version of REV using that revision's scripts/version.sh.
relay_version() {
  g show "$1:scripts/version.sh" > /tmp/relay-version.sh 2>/dev/null || return 0
  as_owner sh /tmp/relay-version.sh "$1" 2>/dev/null || true
}
B="$RELAY_BRANCH"
echo "::url $(g remote get-url origin 2>/dev/null)"
echo "::ref $(g rev-parse --abbrev-ref HEAD 2>/dev/null)"
out=$(g fetch --quiet --tags --force origin "$B" 2>&1) || fail "git fetch from origin failed: $(echo "$out" | tail -n 3 | tr '\n' ' ')"
echo "::head $(g rev-parse HEAD)"
echo "::remote $(g rev-parse "origin/$B")"
echo "::headversion $(relay_version HEAD)"
echo "::remoteversion $(relay_version "origin/$B")"
echo "::behind $(g rev-list --count "HEAD..origin/$B")"
echo "::ahead $(g rev-list --count "origin/$B..HEAD")"
echo "::dirty $(g status --porcelain --untracked-files=no | wc -l | tr -d ' ')"
`

const relayCheckScript = helperPreamble + `g log --max-count=30 --format='::commit %H%x09%h%x09%an%x09%aI%x09%s' "HEAD..origin/$B"
echo "::ok"
`

const relayUpdateScript = helperPreamble + `step() { echo "::step $1"; }
REF=$(g rev-parse --abbrev-ref HEAD)
[ "$REF" = "$B" ] || fail "The checkout is on branch $REF, not $B. Switch it with git checkout $B on the Docker host first."

step pull
FROM=$(g rev-parse HEAD)
if [ "$(g rev-list --count "HEAD..origin/$B")" != 0 ]; then
  out=$(g merge --ff-only "origin/$B" 2>&1) || { echo "$out"; fail "Can't fast-forward to origin/$B: the checkout has local commits or changes. Resolve them with git on the Docker host, then try again."; }
fi
TO=$(g rev-parse HEAD)
echo "::pulled $FROM $TO"
VERSION=$(relay_version HEAD)
echo "::version $VERSION"

set --
OLDIFS=$IFS
IFS=','
for f in $RELAY_COMPOSE_FILES; do set -- "$@" -f "$f"; done
IFS=$OLDIFS
export COMPOSE_PROJECT_NAME="$RELAY_PROJECT" RELAY_COMMIT="$TO" RELAY_VERSION="$VERSION"
PREV_IMAGE=$(docker inspect -f '{{.Image}}' "$RELAY_CONTAINER" 2>/dev/null || true)
IMAGE_REF=$(docker inspect -f '{{.Config.Image}}' "$RELAY_CONTAINER" 2>/dev/null || true)

step build
docker compose --progress plain -p "$RELAY_PROJECT" "$@" build --build-arg RELAY_COMMIT="$TO" --build-arg RELAY_VERSION="$VERSION" 2>&1 || fail "The build failed. Relay keeps running the current version."

wait_healthy() {
  i=0
  while [ $i -lt 180 ]; do
    st=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$RELAY_CONTAINER" 2>/dev/null || true)
    case "$st" in
      healthy|running) return 0 ;;
      unhealthy|exited|dead) return 1 ;;
    esac
    sleep 3
    i=$((i + 3))
  done
  return 1
}

step restart
docker compose --progress plain -p "$RELAY_PROJECT" "$@" up -d 2>&1 || fail "docker compose up failed. See the output above."
sleep 3
if ! wait_healthy; then
  step rollback
  docker logs --tail 30 "$RELAY_CONTAINER" 2>&1 || true
  if [ -n "$PREV_IMAGE" ] && [ -n "$IMAGE_REF" ] && docker tag "$PREV_IMAGE" "$IMAGE_REF" 2>&1 && docker compose -p "$RELAY_PROJECT" "$@" up -d --no-build 2>&1 && sleep 3 && wait_healthy; then
    fail "The new version didn't become healthy, so Relay was rolled back to the previous image. The checkout stays at $TO."
  fi
  fail "The new version didn't become healthy after the restart."
fi

if [ "${RELAY_RESTART_ENGINES:-0}" = 1 ]; then
  ids=$(docker ps -q --filter label=relay.engine --filter "label=com.docker.compose.project=$RELAY_PROJECT")
  if [ -n "$ids" ]; then docker restart $ids >/dev/null 2>&1 && echo "Restarted the engines"; fi
fi
echo "::done $TO"
`

// ---------------------------------------------------------------- self container

type selfContainer struct {
	ID          string
	Name        string
	Project     string
	WorkingDir  string
	ConfigFiles []string
	DockerSock  string // host path of the Docker socket mounted into relay
}

var errNotCompose = errors.New("Relay isn't running from docker compose, so it can't rebuild itself. Update it on the Docker host with git pull && docker compose up -d --build.")

// findSelf locates the relay container and its compose project.
func findSelf(ctx context.Context, cli *client.Client) (*selfContainer, error) {
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if id := selfContainerID(string(b)); id != "" {
			if info, err := cli.ContainerInspect(ctx, id); err == nil {
				return selfFromInspect(info)
			}
		}
	}
	args := filters.NewArgs(filters.Arg("label", labelComposeSvc+"=relay"))
	if project := composeProject(ctx, cli); project != "" {
		args.Add("label", labelComposeProj+"="+project)
	}
	list, err := cli.ContainerList(ctx, container.ListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	switch len(list) {
	case 0:
		return nil, errNotCompose
	case 1:
		info, err := cli.ContainerInspect(ctx, list[0].ID)
		if err != nil {
			return nil, err
		}
		return selfFromInspect(info)
	}
	return nil, fmt.Errorf("found %d running relay containers; set %s so Relay knows which compose project is its own", len(list), envComposeProject)
}

func selfFromInspect(info container.InspectResponse) (*selfContainer, error) {
	if info.ContainerJSONBase == nil || info.Config == nil {
		return nil, errNotCompose
	}
	labels := info.Config.Labels
	s := &selfContainer{ID: info.ID, Name: strings.TrimPrefix(info.Name, "/"), Project: labels[labelComposeProj], WorkingDir: labels[labelComposeWorkDir]}
	for _, f := range strings.Split(labels[labelComposeFiles], ",") {
		if f = strings.TrimSpace(f); f != "" {
			s.ConfigFiles = append(s.ConfigFiles, f)
		}
	}
	for _, m := range info.Mounts {
		if m.Destination == dockerSocket {
			s.DockerSock = m.Source
		}
	}
	if s.Project == "" || s.WorkingDir == "" {
		return nil, errNotCompose
	}
	return s, nil
}

// ---------------------------------------------------------------- helper containers

func ensureImage(ctx context.Context, cli *client.Client, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	rc, err := cli.ImagePull(pctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Error != "" {
			return errors.New(m.Error)
		}
	}
	return sc.Err()
}

func within(dir, root string) bool {
	return dir == root || strings.HasPrefix(dir, strings.TrimSuffix(root, "/")+"/")
}

// startHelper runs script in a new helper container with the compose project
// folder mounted at the same path as on the host.
func startHelper(ctx context.Context, cli *client.Client, self *selfContainer, kind, jobID, script string, extraEnv ...string) (string, error) {
	binds := []string{self.WorkingDir + ":" + self.WorkingDir, self.DockerSock + ":" + dockerSocket}
	seen := map[string]bool{}
	for _, f := range self.ConfigFiles {
		if dir := filepath.Dir(f); !within(dir, self.WorkingDir) && !seen[dir] {
			seen[dir] = true
			binds = append(binds, dir+":"+dir)
		}
	}
	env := append([]string{
		"RELAY_WORKDIR=" + self.WorkingDir,
		"RELAY_PROJECT=" + self.Project,
		"RELAY_COMPOSE_FILES=" + strings.Join(self.ConfigFiles, ","),
		"RELAY_BRANCH=" + updateBranch(),
		"RELAY_CONTAINER=" + self.Name,
	}, extraEnv...)
	cfg := &container.Config{
		Image: updaterImage(), Entrypoint: []string{"sh", "-c", script}, Env: env,
		Labels: map[string]string{labelSelfUpdate: kind, labelSelfUpdateJob: jobID},
	}
	hc := &container.HostConfig{Binds: binds, NetworkMode: "host"}
	name := fmt.Sprintf("%s-updater-%s-%s", self.Project, kind, shortSHA(jobID))
	cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}) // leftover from an interrupted run
	created, err := cli.ContainerCreate(ctx, cfg, hc, nil, nil, name)
	if err != nil {
		return "", err
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return created.ID, nil
}

// waitHelper waits for a helper to exit and returns its exit code and output.
func waitHelper(ctx context.Context, cli *client.Client, id string, timeout time.Duration) (int64, string, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	statusCh, errCh := cli.ContainerWait(wctx, id, container.WaitConditionNotRunning)
	var code int64 = -1
	var err error
	select {
	case st := <-statusCh:
		code = st.StatusCode
	case err = <-errCh:
	}
	return code, containerLogs(context.WithoutCancel(ctx), cli, id, 2000), err
}

// ---------------------------------------------------------------- output parsing

type helperResult struct {
	URL, Ref, Head, Remote     string
	HeadVersion, RemoteVersion string
	Version                    string // version of the pulled commit
	Behind, Ahead, Dirty       int
	Commits                    []core.RelayCommit
	Steps                      []string
	PulledFrom, PulledTo       string
	Done                       bool
	DoneCommit                 string
	Error                      string
	Log                        []string // lines without a marker
}

// feed consumes one output line and returns its marker ("" for plain output).
func (r *helperResult) feed(line string) string {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "::") {
		if strings.TrimSpace(line) != "" {
			r.Log = append(r.Log, line)
		}
		return ""
	}
	key, val, _ := strings.Cut(line[2:], " ")
	num := func() int { n, _ := strconv.Atoi(strings.TrimSpace(val)); return n }
	switch key {
	case "url":
		r.URL = strings.TrimSpace(val)
	case "ref":
		r.Ref = strings.TrimSpace(val)
	case "head":
		r.Head = strings.TrimSpace(val)
	case "remote":
		r.Remote = strings.TrimSpace(val)
	case "headversion":
		r.HeadVersion = strings.TrimSpace(val)
	case "remoteversion":
		r.RemoteVersion = strings.TrimSpace(val)
	case "version":
		r.Version = strings.TrimSpace(val)
	case "behind":
		r.Behind = num()
	case "ahead":
		r.Ahead = num()
	case "dirty":
		r.Dirty = num()
	case "commit":
		f := strings.SplitN(val, "\t", 5)
		if len(f) == 5 {
			c := core.RelayCommit{SHA: f[0], Short: f[1], Author: f[2], Subject: f[4]}
			if t, err := time.Parse(time.RFC3339, f[3]); err == nil {
				c.Date = &t
			}
			r.Commits = append(r.Commits, c)
		}
	case "step":
		r.Steps = append(r.Steps, strings.TrimSpace(val))
	case "pulled":
		r.PulledFrom, r.PulledTo, _ = strings.Cut(strings.TrimSpace(val), " ")
	case "done":
		r.Done, r.DoneCommit = true, strings.TrimSpace(val)
	case "error":
		r.Error = strings.TrimSpace(val)
	case "ok":
	default:
		r.Log = append(r.Log, line)
		return ""
	}
	return key
}

func parseHelperOutput(out string) helperResult {
	var r helperResult
	for _, line := range strings.Split(out, "\n") {
		r.feed(line)
	}
	return r
}

func lastLines(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

var githubRemoteRe = regexp.MustCompile(`^(?:https?://github\.com/|git@github\.com:|ssh://git@github\.com/)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// sanitizeRemote strips credentials from an https remote URL.
func sanitizeRemote(remote string) string {
	if u, err := url.Parse(remote); err == nil && u.User != nil && (u.Scheme == "https" || u.Scheme == "http") {
		u.User = nil
		return u.String()
	}
	return remote
}

// githubRepoURL returns https://github.com/<owner>/<repo> for GitHub remotes.
func githubRepoURL(remote string) string {
	m := githubRemoteRe.FindStringSubmatch(sanitizeRemote(remote))
	if m == nil {
		return ""
	}
	return "https://github.com/" + m[1] + "/" + m[2]
}

// ---------------------------------------------------------------- info

func (s *Service) relayBase() core.RelayUpdateInfo {
	return core.RelayUpdateInfo{Version: s.app.Config.Version, Commit: s.app.Config.Commit, Branch: updateBranch(), Commits: []core.RelayCommit{}}
}

// finishRelayInfo derives the computed fields.
func finishRelayInfo(info *core.RelayUpdateInfo) {
	if info.Commits == nil {
		info.Commits = []core.RelayCommit{}
	}
	if info.Blocker == "" && info.CheckError == "" && info.CheckoutRef != "" && info.CheckoutRef != info.Branch {
		info.Blocker = fmt.Sprintf("The checkout in %s is on branch %s, not %s. Switch it with git checkout %s on the Docker host.", info.WorkingDir, info.CheckoutRef, info.Branch, info.Branch)
	}
	ok := info.Blocker == "" && info.CheckError == "" && info.CheckoutHead != ""
	info.RebuildNeeded = ok && info.Commit != "" && info.Behind == 0 && info.Commit != info.CheckoutHead
	info.UpdateAvailable = ok && (info.Behind > 0 || info.RebuildNeeded)
	info.CanUpdate = ok
}

// probeRelay runs the check helper and returns fresh update info.
func (s *Service) probeRelay(ctx context.Context) core.RelayUpdateInfo {
	info := s.relayBase()
	now := time.Now().UTC()
	info.CheckedAt = &now
	defer finishRelayInfo(&info)

	cli, err := newDockerClient()
	if err != nil {
		info.Blocker = "Relay can't reach the Docker API: " + err.Error()
		return info
	}
	defer cli.Close()
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := cli.Ping(dctx); err != nil {
		info.Blocker = "Relay can't reach the Docker API. Mount /var/run/docker.sock into the relay container to update from the UI."
		return info
	}
	self, err := findSelf(dctx, cli)
	if err != nil {
		info.Blocker = err.Error()
		return info
	}
	info.Container, info.WorkingDir = self.Name, self.WorkingDir
	if self.DockerSock == "" {
		info.Blocker = "The Docker socket isn't mounted into the relay container (/var/run/docker.sock), which in-app updates need."
		return info
	}
	if err := ensureImage(dctx, cli, updaterImage()); err != nil {
		info.CheckError = fmt.Sprintf("Couldn't pull the updater image %s: %v", updaterImage(), err)
		return info
	}
	id, err := startHelper(dctx, cli, self, "check", store.NewID(), relayCheckScript)
	if err != nil {
		info.CheckError = "Couldn't start the updater container: " + err.Error()
		return info
	}
	code, out, werr := waitHelper(dctx, cli, id, 3*time.Minute)
	cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})

	r := parseHelperOutput(out)
	info.Remote = sanitizeRemote(r.URL)
	info.CheckoutRef, info.CheckoutHead, info.RemoteHead = r.Ref, r.Head, r.Remote
	info.CheckoutVersion, info.RemoteVersion = r.HeadVersion, r.RemoteVersion
	info.Behind, info.Ahead, info.Dirty = r.Behind, r.Ahead, r.Dirty
	repo := githubRepoURL(r.URL)
	for _, c := range r.Commits {
		if repo != "" {
			c.URL = repo + "/commit/" + c.SHA
		}
		info.Commits = append(info.Commits, c)
	}
	switch {
	case r.Error != "":
		info.CheckError = r.Error
	case werr != nil:
		info.CheckError = "The update check didn't finish: " + werr.Error()
	case code != 0:
		info.CheckError = fmt.Sprintf("The update check failed (exit %d): %s", code, lastLines(r.Log, 3))
	}
	return info
}

// checkRelay refreshes, stores and publishes Relay's update info.
func (s *Service) checkRelay(ctx context.Context) *core.RelayUpdateInfo {
	info := s.probeRelay(ctx)
	s.mu.Lock()
	s.relayInfo = &info
	s.mu.Unlock()
	if b, err := json.Marshal(info); err == nil {
		s.app.Store.PutKV(context.WithoutCancel(ctx), kvRelayInfo, b)
	}
	if info.CheckError != "" {
		s.log.Warn("relay update check", "err", info.CheckError)
	}
	s.app.Bus.Publish(events.EngineUpdates, map[string]any{"relay": info.UpdateAvailable})
	return &info
}

// relayInfoLive returns the cached info with the running build filled in.
func (s *Service) relayInfoLive() *core.RelayUpdateInfo {
	s.mu.Lock()
	info := s.relayBase()
	if s.relayInfo != nil {
		info = *s.relayInfo
		info.Commits = append([]core.RelayCommit(nil), s.relayInfo.Commits...)
	}
	s.mu.Unlock()
	info.Version, info.Commit, info.Branch = s.app.Config.Version, s.app.Config.Commit, updateBranch()
	finishRelayInfo(&info)
	return &info
}

// loadRelay restores cached info and resumes an update that was running when
// Relay restarted (usually because the update recreated this container).
func (s *Service) loadRelay(ctx context.Context) {
	if b, err := s.app.Store.GetKV(ctx, kvRelayInfo); err == nil {
		var info core.RelayUpdateInfo
		if json.Unmarshal(b, &info) == nil {
			s.relayInfo = &info
		}
	}
	if b, err := s.app.Store.GetKV(ctx, kvRelayJob); err == nil {
		var j core.RelayUpdateJob
		if json.Unmarshal(b, &j) == nil && j.ID != "" {
			s.relayJob = &j
			if j.Status == core.UpgradeRunning {
				go s.resumeRelayUpdate(ctx)
			}
		}
	}
}

// notifyRelay sends one activity + notification per new remote head.
func (s *Service) notifyRelay(ctx context.Context, info *core.RelayUpdateInfo, notified map[string]string) bool {
	if info == nil || !info.UpdateAvailable || info.Behind == 0 || info.RemoteHead == "" || notified["relay"] == info.RemoteHead {
		return false
	}
	notified["relay"] = info.RemoteHead
	title := "Relay upgrade available"
	if info.RemoteVersion != "" {
		title = fmt.Sprintf("Relay %s is available", info.RemoteVersion)
	}
	detail := fmt.Sprintf("Running %s · %d new commit(s) on %s", firstNonEmpty(info.Version, shortSHA(info.Commit)), info.Behind, info.Branch)
	if len(info.Commits) > 0 {
		detail += " · latest: " + info.Commits[0].Subject
	}
	s.app.Activity(ctx, "relay.update", "info", title, "relay", detail)
	if s.app.Notify != nil {
		s.app.Notify.Notify(ctx, core.Notification{Event: model.EventEngineUpdateAvailable, Level: "info", Title: title, Message: detail + ". Upgrade from Settings → Updates.", URL: "/settings/engines"})
	}
	return true
}
