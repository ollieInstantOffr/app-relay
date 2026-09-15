package engines

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// applyOps is implemented by the apply service (same slice).
type applyOps interface {
	TryLockApply() (unlock func(), ok bool)
	LiveRelease(ctx context.Context, engine string) (files agent.Files, hash string, running bool, version int64, err error)
	PushLive(ctx context.Context, engine string) error
	HealthyHosts(ctx context.Context) []string
	WatchHosts(ctx context.Context, ids []string, window time.Duration, tick func(sec, total int)) string
}

type backupCreator interface {
	Create(ctx context.Context, trigger, passphrase string) (*store.BackupRow, error)
}

var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

var upgradeSteps = []core.UpgradeStep{
	{ID: "preflight", Label: "Preflight"},
	{ID: "pull", Label: "Pull image"},
	{ID: "validate", Label: "Validate config"},
	{ID: "backup", Label: "Backup"},
	{ID: "swap", Label: "Swap container"},
	{ID: "health", Label: "Health check"},
}

// Upgrade validates the request, then runs the upgrade in the background.
func (s *Service) Upgrade(ctx context.Context, engine, version string) (*core.UpgradeJob, error) {
	if engine != "nginx" && engine != "haproxy" {
		return nil, httpx.Errorf(http.StatusNotFound, "not_found", "unknown engine (nginx | haproxy)")
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !versionRe.MatchString(version) {
		return nil, httpx.Errorf(http.StatusBadRequest, "bad_request", "version must look like 1.30.4")
	}
	ops, ok := s.app.Engine.(applyOps)
	if !ok {
		return nil, core.ErrNotImplemented
	}
	s.mu.Lock()
	if s.job != nil && s.job.Status == core.UpgradeRunning {
		s.mu.Unlock()
		return nil, httpx.Errorf(http.StatusConflict, "upgrade_running", fmt.Sprintf("An %s upgrade is already running", engineName(s.job.Engine)))
	}
	s.mu.Unlock()

	cli, err := newDockerClient()
	if err != nil {
		return nil, httpx.Errorf(http.StatusServiceUnavailable, "docker_unavailable", "Docker API unavailable: "+err.Error())
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx); err != nil {
		cli.Close()
		return nil, httpx.Errorf(http.StatusServiceUnavailable, "docker_unavailable", "Docker API unreachable: "+err.Error())
	}
	project := composeProject(pctx, cli)
	c, err := findEngineContainer(pctx, cli, project, engine)
	if err != nil {
		cli.Close()
		return nil, httpx.Errorf(http.StatusConflict, "engine_container_not_found", err.Error())
	}
	_, tag, official := officialRef(c.Image)
	if !official {
		cli.Close()
		return nil, httpx.Errorf(http.StatusConflict, "not_official_image", fmt.Sprintf("%s runs %s, not the official %s image; switch docker-compose.yml first (deploy/UPGRADES.md)", c.Name, c.Image, engine))
	}
	target := imageForVersion(engine, version)
	if c.Image == target && c.State == "running" {
		cli.Close()
		return nil, httpx.Errorf(http.StatusConflict, "already_running", fmt.Sprintf("%s already runs %s", c.Name, target))
	}
	unlock, ok := ops.TryLockApply()
	if !ok {
		cli.Close()
		return nil, httpx.Errorf(http.StatusConflict, "apply_running", "A config apply is in progress; try again when it finishes")
	}

	from := ""
	if v, ok := parseVersion(tag); ok {
		from = v.String()
	} else if st, err := s.agentClient(engine).Status(pctx); err == nil {
		from = st.Version
	}
	job := &core.UpgradeJob{
		ID: store.NewID(), Engine: engine, From: from, To: version, FromImage: c.Image, ToImage: target,
		Actor: core.ActorFrom(ctx).Label(), Status: core.UpgradeRunning, StartedAt: time.Now().UTC(),
		Steps: append([]core.UpgradeStep(nil), upgradeSteps...),
	}
	for i := range job.Steps {
		job.Steps[i].Status = "pending"
	}
	s.mu.Lock()
	s.job = job
	s.mu.Unlock()
	s.persistJob(ctx)
	s.step("preflight", "done", fmt.Sprintf("Docker reachable · container %s · %s", c.Name, c.Image), 3, "Preflight passed")

	actorCtx := core.WithActor(context.WithoutCancel(s.ctx), core.ActorFrom(ctx))
	go func() {
		defer unlock()
		defer cli.Close()
		s.runUpgrade(actorCtx, cli, ops, c)
	}()
	return s.UpgradeStatus(), nil
}

// step updates a step and publishes the job.
func (s *Service) step(id, status, detail string, progress int, message string) {
	s.mu.Lock()
	if s.job == nil {
		s.mu.Unlock()
		return
	}
	for i := range s.job.Steps {
		if s.job.Steps[i].ID == id {
			s.job.Steps[i].Status = status
			if detail != "" || status != "running" {
				s.job.Steps[i].Detail = detail
			}
		}
	}
	if progress > 0 {
		s.job.Progress = progress
	}
	if message != "" {
		s.job.Message = message
	}
	s.mu.Unlock()
	// Persist step transitions (not pull progress ticks) so a restarted relay
	// knows a swap may be half done.
	if status != "running" || id == "swap" || id == "health" {
		s.persistJob(context.Background())
	}
	s.publishJob()
}

func (s *Service) persistJob(ctx context.Context) {
	if j := s.UpgradeStatus(); j != nil {
		if b, err := json.Marshal(j); err == nil {
			s.app.Store.PutKV(context.WithoutCancel(ctx), kvLastJob, b)
		}
	}
}

func (s *Service) publishJob() {
	j := s.UpgradeStatus()
	if j == nil {
		return
	}
	s.app.Bus.Publish(events.EngineUpgrade, j)
}

func (s *Service) finishJob(ctx context.Context, status, message, errText, output string) {
	s.mu.Lock()
	now := time.Now().UTC()
	s.job.Status, s.job.Message, s.job.Error, s.job.FinishedAt = status, message, errText, &now
	if output != "" {
		s.job.Output = output
	}
	if status == core.UpgradeSucceeded {
		s.job.Progress = 100
	}
	for i := range s.job.Steps {
		if s.job.Steps[i].Status == "pending" || s.job.Steps[i].Status == "running" {
			if status == core.UpgradeSucceeded {
				s.job.Steps[i].Status = "done"
			} else if s.job.Steps[i].Status == "running" {
				s.job.Steps[i].Status = "failed"
			} else {
				s.job.Steps[i].Status = "skipped"
			}
		}
	}
	s.mu.Unlock()
	s.persistJob(ctx)
	s.publishJob()
	s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": s.job.Engine})
	s.app.Bus.Publish(events.EngineUpdates, map[string]any{"reason": "upgrade"})
}

func (s *Service) runUpgrade(ctx context.Context, cli *client.Client, ops applyOps, c *engineContainer) {
	job := s.UpgradeStatus()
	engine, name := job.Engine, engineName(job.Engine)
	fail := func(stepID, message, errText, output string) {
		s.step(stepID, "failed", errText, 0, "")
		s.finishJob(ctx, core.UpgradeFailed, message, errText, output)
		sys := core.ActorFrom(ctx)
		s.app.Audit(ctx, core.AuditEntry{Actor: &sys, Action: "engine.upgrade", Target: engine, Detail: fmt.Sprintf("%s → %s · %s", job.From, job.To, errText), Result: "failed"})
		s.log.Warn("engine upgrade failed", "engine", engine, "step", stepID, "err", errText)
	}

	// 2. Pull.
	s.step("pull", "running", job.ToImage, 5, "Pulling "+job.ToImage)
	if err := s.pull(ctx, cli, job.ToImage); err != nil {
		fail("pull", fmt.Sprintf("Couldn't pull %s — nothing was changed.", job.ToImage), err.Error(), "")
		return
	}
	s.step("pull", "done", job.ToImage, 35, "Pulled "+job.ToImage)

	// 3. Validate the live config on the new version.
	old, err := cli.ContainerInspect(ctx, c.ID)
	if err != nil || old.Config == nil {
		fail("validate", "Couldn't inspect the engine container — nothing was changed.", errString(err, "no config"), "")
		return
	}
	files, _, running, liveVersion, err := ops.LiveRelease(ctx, engine)
	if err != nil {
		fail("validate", "Couldn't load the live configuration — nothing was changed.", err.Error(), "")
		return
	}
	active := s.activeProxy(ctx, engine)
	switch {
	case agent.IsProxyEngine(engine) && !active:
		s.step("validate", "skipped", name+" isn't the selected proxy engine", 50, "")
	case agent.IsLBEngine(engine) && s.app.LBEngine(ctx) != engine:
		s.step("validate", "skipped", name+" isn't the selected load balancer engine", 50, "")
	case files == nil:
		s.step("validate", "skipped", "Nothing applied yet", 50, "")
	case engine == "haproxy" && !running:
		s.step("validate", "skipped", "HAProxy is stopped", 50, "")
	default:
		s.step("validate", "running", fmt.Sprintf("v%d on %s", liveVersion, job.To), 40, fmt.Sprintf("Validating v%d with %s %s", liveVersion, name, job.To))
		out, err := s.validateOn(ctx, cli, old, engine, job.ToImage, files)
		if err != nil {
			fail("validate", fmt.Sprintf("The live configuration (v%d) doesn't validate on %s %s — nothing was changed.", liveVersion, name, job.To), err.Error(), out)
			return
		}
		s.step("validate", "done", validSummary(engine, out), 50, fmt.Sprintf("v%d is valid on %s %s", liveVersion, name, job.To))
	}

	// 4. Backup (best effort).
	s.step("backup", "running", "", 52, "Creating a backup")
	if b, ok := s.app.Backup.(backupCreator); ok {
		row, err := b.Create(ctx, "before-upgrade", "")
		switch {
		case err != nil:
			s.step("backup", "skipped", "Skipped: "+err.Error(), 55, "")
		default:
			s.step("backup", "done", row.File, 55, "")
		}
	} else {
		s.step("backup", "skipped", "Backups unavailable", 55, "")
	}

	// Which hosts are healthy now (only those can regress).
	var healthy []string
	if active || running {
		healthy = ops.HealthyHosts(ctx)
	}

	// 5. Swap.
	s.step("swap", "running", "", 60, "Swapping "+c.Name)
	oldImg, err := cli.ImageInspect(ctx, old.Image)
	if err != nil {
		fail("swap", "Couldn't inspect the current image — nothing was changed.", err.Error(), "")
		return
	}
	cfg, hc, nc := cloneSpec(old, defaultsOf(oldImg), job.ToImage)
	oldName := strings.TrimPrefix(old.Name, "/")
	backupName := fmt.Sprintf("%s-old-%d", oldName, time.Now().Unix())
	if err := cli.ContainerRename(ctx, old.ID, backupName); err != nil {
		fail("swap", "Couldn't rename the old container — nothing was changed.", err.Error(), "")
		return
	}
	restoreName := func() { cli.ContainerRename(context.WithoutCancel(ctx), old.ID, oldName) }
	created, err := cli.ContainerCreate(ctx, cfg, hc, nc, nil, oldName)
	if err != nil {
		restoreName()
		fail("swap", "Couldn't create the new container — the old one keeps running.", err.Error(), "")
		return
	}
	newID := created.ID
	// The agent stops the engine gracefully (nginx quit / HAProxy soft stop,
	// which closes listeners first); Docker kills it after the timeout.
	stopStart := time.Now()
	stopTimeout := 10
	if err := cli.ContainerStop(ctx, old.ID, container.StopOptions{Timeout: &stopTimeout}); err != nil {
		cli.ContainerRemove(context.WithoutCancel(ctx), newID, container.RemoveOptions{Force: true})
		restoreName()
		fail("swap", "Couldn't stop the old container — it keeps running.", err.Error(), "")
		return
	}
	rollback := func(reason, errText string) {
		rctx := context.WithoutCancel(ctx)
		s.step("health", "failed", errText, 0, "Rolling back to "+job.FromImage)
		t := 10
		cli.ContainerStop(rctx, newID, container.StopOptions{Timeout: &t})
		logs := containerLogs(rctx, cli, newID, 30)
		cli.ContainerRemove(rctx, newID, container.RemoveOptions{Force: true})
		restoreName()
		startErr := cli.ContainerStart(rctx, old.ID, container.StartOptions{})
		if startErr == nil {
			s.waitAgent(rctx, cli, old.ID, engine, "", 60*time.Second, active || running)
		}
		msg := fmt.Sprintf("%s %s failed after the swap (%s). %s is running again.", name, job.To, reason, job.FromImage)
		if startErr != nil {
			msg = fmt.Sprintf("%s %s failed after the swap (%s) and the old container didn't restart: %v", name, job.To, reason, startErr)
		}
		s.finishJob(ctx, core.UpgradeRolledBack, msg, errText, logs)
		sys := core.ActorFrom(ctx)
		s.app.Audit(ctx, core.AuditEntry{Actor: &sys, Action: "engine.upgrade", Target: engine, Detail: fmt.Sprintf("%s → %s · %s", job.From, job.To, reason), Result: "reverted"})
		s.app.Activity(ctx, "engine.upgrade", "warn", fmt.Sprintf("%s upgrade to %s rolled back", name, job.To), engine, msg)
		if s.app.Notify != nil {
			s.app.Notify.Notify(ctx, core.Notification{Event: model.EventReloadFailed, Level: "error", Title: fmt.Sprintf("%s upgrade rolled back", name), Message: msg, URL: "/settings/engines"})
		}
	}
	stopTook := time.Since(stopStart)
	gapStart := time.Now()
	if err := cli.ContainerStart(ctx, newID, container.StartOptions{}); err != nil {
		rollback("the new container didn't start", err.Error())
		return
	}
	s.step("swap", "done", fmt.Sprintf("%s → %s", job.FromImage, job.ToImage), 75, "Waiting for the "+name+" agent")

	// 6. Agent up, live version pushed, hosts healthy.
	needRunning := active || running
	s.step("health", "running", "", 78, "Waiting for "+name+" "+job.To)
	if err := s.waitAgent(ctx, cli, newID, engine, job.To, 90*time.Second, needRunning); err != nil {
		rollback("the agent didn't come up", err.Error())
		return
	}
	gap := time.Since(gapStart)
	if files != nil {
		if err := ops.PushLive(ctx, engine); err != nil {
			rollback("the live configuration couldn't be restored", err.Error())
			return
		}
	}
	if needRunning {
		if st, err := s.agentClient(engine).Status(ctx); err != nil || !st.Running {
			rollback(name+" isn't running", errString(err, firstNonEmpty(st.ExitError, "not running")))
			return
		}
		if failure := ops.WatchHosts(ctx, healthy, 10*time.Second, func(sec, total int) {
			s.step("health", "running", fmt.Sprintf("%d/%d s", sec, total), 80+sec*2, fmt.Sprintf("Health check %d/%d s", sec, total))
		}); failure != "" {
			rollback(failure, failure)
			return
		}
	}
	s.step("health", "done", fmt.Sprintf("%d host(s) healthy · stop %s · start %s", len(healthy), stopTook.Round(100*time.Millisecond), gap.Round(100*time.Millisecond)), 99, "")

	// Success.
	cli.ContainerRemove(context.WithoutCancel(ctx), old.ID, container.RemoveOptions{Force: true})
	set := s.settings(ctx)
	if engine == "nginx" {
		set.NginxImage = job.ToImage
	} else {
		set.HAProxyImage = job.ToImage
	}
	s.app.Store.PutSettings(ctx, model.SettingsEngines, set)
	msg := fmt.Sprintf("%s upgraded %s → %s", name, orDash(job.From), job.To)
	s.finishJob(ctx, core.UpgradeSucceeded, msg, "", "")
	s.app.Audit(ctx, core.AuditEntry{Action: "engine.upgrade", Target: engine, Detail: fmt.Sprintf("%s → %s", job.FromImage, job.ToImage), Result: "applied"})
	s.app.Activity(ctx, "engine.upgrade", "ok", msg, engine, fmt.Sprintf("%s · stopped in %s, new engine ready in %s", job.ToImage, stopTook.Round(100*time.Millisecond), gap.Round(100*time.Millisecond)))
	s.log.Info("engine upgraded", "engine", engine, "from", job.FromImage, "to", job.ToImage, "stop", stopTook, "start", gap)
}

func errString(err error, def string) string {
	if err != nil {
		return err.Error()
	}
	return def
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

type pullMessage struct {
	Status         string `json:"status"`
	ID             string `json:"id"`
	Error          string `json:"error"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
}

func (s *Service) pull(ctx context.Context, cli *client.Client, ref string) error {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	rc, err := cli.ImagePull(pctx, "docker.io/library/"+ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	type layer struct{ cur, total int64 }
	layers := map[string]*layer{}
	lastPub := time.Time{}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var m pullMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.ID != "" && m.ProgressDetail.Total > 0 && strings.HasPrefix(m.Status, "Downloading") {
			l := layers[m.ID]
			if l == nil {
				l = &layer{}
				layers[m.ID] = l
			}
			l.cur, l.total = m.ProgressDetail.Current, m.ProgressDetail.Total
		}
		if m.ID != "" && (m.Status == "Download complete" || m.Status == "Pull complete" || m.Status == "Already exists") {
			if l := layers[m.ID]; l != nil {
				l.cur = l.total
			}
		}
		if time.Since(lastPub) > 500*time.Millisecond {
			lastPub = time.Now()
			var cur, total int64
			for _, l := range layers {
				cur += l.cur
				total += l.total
			}
			pct := 5
			detail := ref
			if total > 0 {
				pct = 5 + int(30*cur/total)
				detail = fmt.Sprintf("%s · %.1f / %.1f MB", ref, float64(cur)/1e6, float64(total)/1e6)
			}
			s.step("pull", "running", detail, pct, "Pulling "+ref)
		}
	}
	return sc.Err()
}

// validateOn runs nginx -t / haproxy -c with the live files in a throwaway
// container of the new image (same mounts and network as the engine).
func (s *Service) validateOn(ctx context.Context, cli *client.Client, old container.InspectResponse, engine, ref string, files agent.Files) (string, error) {
	script := "mkdir -p /run/nginx /var/cache/nginx /var/log/relay && exec nginx -t -c /tmp/relay-validate/nginx.conf"
	if engine == "haproxy" {
		script = "exec haproxy -c -f /tmp/relay-validate/haproxy.cfg"
	}
	hc := &container.HostConfig{}
	if old.HostConfig != nil {
		hc.NetworkMode = old.HostConfig.NetworkMode
		hc.Binds = old.HostConfig.Binds
		hc.Mounts = old.HostConfig.Mounts
		hc.ExtraHosts = old.HostConfig.ExtraHosts
	}
	cfg := &container.Config{
		Image: ref, User: "0", Entrypoint: []string{"sh", "-c", script},
		Labels: map[string]string{labelValidate: engine},
	}
	name := fmt.Sprintf("%s-validate-%d", strings.TrimPrefix(old.Name, "/"), time.Now().Unix())
	created, err := cli.ContainerCreate(ctx, cfg, hc, nil, nil, name)
	if err != nil {
		return "", fmt.Errorf("create validation container: %w", err)
	}
	defer cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	tarball, err := tarFiles("relay-validate", files)
	if err != nil {
		return "", err
	}
	if err := cli.CopyToContainer(ctx, created.ID, "/tmp", tarball, container.CopyToContainerOptions{}); err != nil {
		return "", fmt.Errorf("copy config into validation container: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	waitC, errC := cli.ContainerWait(wctx, created.ID, container.WaitConditionNextExit)
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start validation container: %w", err)
	}
	var code int64
	select {
	case res := <-waitC:
		code = res.StatusCode
	case err := <-errC:
		return "", fmt.Errorf("wait for validation: %w", err)
	}
	out := containerLogs(ctx, cli, created.ID, 200)
	out = strings.ReplaceAll(out, "/tmp/relay-validate/", "")
	if code != 0 {
		return out, fmt.Errorf("%s exited with %d: %s", map[string]string{"nginx": "nginx -t", "haproxy": "haproxy -c"}[engine], code, firstErrorLine(out))
	}
	return out, nil
}

// validSummary describes a successful nginx -t / haproxy -c run.
func validSummary(engine, out string) string {
	warnings := strings.Count(out, "[warn]") + strings.Count(out, "[WARNING]")
	msg := "nginx -t passed"
	if engine == "haproxy" {
		msg = "haproxy -c passed"
	}
	if warnings > 0 {
		msg += fmt.Sprintf(" · %d warning%s", warnings, map[bool]string{true: "s", false: ""}[warnings > 1])
	}
	return msg
}

func firstErrorLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[emerg]") || strings.Contains(l, "[ALERT]") || strings.Contains(l, "error") {
			return strings.TrimSpace(l)
		}
	}
	return firstLine(out)
}

func containerLogs(ctx context.Context, cli *client.Client, id string, tail int) string {
	rc, err := cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: fmt.Sprint(tail)})
	if err != nil {
		return ""
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, io.LimitReader(rc, 1<<20)); err != nil && buf.Len() == 0 {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

// waitAgent waits until the engine agent answers (and the engine runs the
// expected version when version != "").
func (s *Service) waitAgent(ctx context.Context, cli *client.Client, containerID, engine, version string, timeout time.Duration, needRunning bool) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		// Fail fast when the container itself died (crash loop, killed, bad entrypoint).
		if cli != nil && containerID != "" {
			if info, err := cli.ContainerInspect(ctx, containerID); err == nil && info.State != nil && !info.State.Running && !info.State.Restarting {
				return fmt.Errorf("the container exited (code %d)%s", info.State.ExitCode, map[bool]string{true: ": " + info.State.Error, false: ""}[info.State.Error != ""])
			}
		}
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := s.agentClient(engine).Status(sctx)
		cancel()
		switch {
		case err != nil:
			last = "agent unreachable"
		case version != "" && st.Version != version:
			last = fmt.Sprintf("agent reports %s %s", engine, st.Version)
		case needRunning && !st.Running && st.Configured:
			last = firstNonEmpty(st.ExitError, engine+" not running yet")
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timed out after %s: %s", timeout, last)
}
