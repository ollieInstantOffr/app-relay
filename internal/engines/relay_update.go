package engines

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

var relaySteps = []core.UpgradeStep{
	{ID: "prepare", Label: "Prepare"},
	{ID: "pull", Label: "Pull from GitHub"},
	{ID: "build", Label: "Build"},
	{ID: "restart", Label: "Restart"},
	{ID: "rollback", Label: "Roll back"},
	{ID: "verify", Label: "Verify"},
}

var relayStepInfo = map[string]struct {
	progress int
	message  string
}{
	"pull":     {12, "Pulling the latest commits"},
	"build":    {25, "Building the new Relay image. This can take a few minutes."},
	"restart":  {85, "Restarting Relay. This page reconnects by itself."},
	"rollback": {90, "The new version didn't start. Rolling back"},
}

const relayOutputLines = 60

// UpdateRelay validates the request and starts a self-update in the background.
func (s *Service) UpdateRelay(ctx context.Context, restartEngines bool) (*core.RelayUpdateJob, error) {
	s.mu.Lock()
	switch {
	case s.relayJob != nil && s.relayJob.Status == core.UpgradeRunning:
		s.mu.Unlock()
		return nil, httpx.Errorf(http.StatusConflict, "relay_update_running", "A Relay update is already running")
	case s.job != nil && s.job.Status == core.UpgradeRunning:
		s.mu.Unlock()
		return nil, httpx.Errorf(http.StatusConflict, "upgrade_running", fmt.Sprintf("An %s upgrade is running; try again when it finishes", engineName(s.job.Engine)))
	}
	s.mu.Unlock()

	cli, err := newDockerClient()
	if err != nil {
		return nil, httpx.Errorf(http.StatusServiceUnavailable, "docker_unavailable", "Docker API unavailable: "+err.Error())
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx); err != nil {
		cli.Close()
		return nil, httpx.Errorf(http.StatusServiceUnavailable, "docker_unavailable", "Docker API unreachable: "+err.Error())
	}
	self, err := findSelf(pctx, cli)
	if err != nil {
		cli.Close()
		return nil, httpx.Errorf(http.StatusConflict, "relay_not_compose", err.Error())
	}
	if self.DockerSock == "" {
		cli.Close()
		return nil, httpx.Errorf(http.StatusConflict, "docker_socket_missing", "The Docker socket isn't mounted into the relay container (/var/run/docker.sock), which in-app updates need.")
	}

	job := &core.RelayUpdateJob{
		ID: store.NewID(), From: s.app.Config.Commit, Actor: core.ActorFrom(ctx).Label(), RestartEngines: restartEngines,
		Status: core.UpgradeRunning, StartedAt: time.Now().UTC(), Steps: append([]core.UpgradeStep(nil), relaySteps...),
	}
	for i := range job.Steps {
		job.Steps[i].Status = "pending"
	}
	s.mu.Lock()
	s.relayJob = job
	s.mu.Unlock()
	s.relayStep("prepare", "running", updaterImage(), 3, "Preparing the updater")
	s.app.Audit(ctx, core.AuditEntry{Action: "relay.update", Target: "relay", Detail: "started · " + self.WorkingDir + " · branch " + updateBranch(), Result: "started"})

	// Following the helper stops when Relay shuts down (the update restarts it);
	// the restarted Relay resumes from the helper container.
	runCtx := core.WithActor(s.ctx, core.ActorFrom(ctx))
	go func() {
		defer cli.Close()
		s.runRelayUpdate(runCtx, cli, self)
	}()
	return s.RelayUpdateStatus(), nil
}

func (s *Service) RelayUpdateStatus() *core.RelayUpdateJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.relayJob == nil {
		return nil
	}
	j := *s.relayJob
	j.Steps = append([]core.UpgradeStep(nil), s.relayJob.Steps...)
	return &j
}

func (s *Service) runRelayUpdate(ctx context.Context, cli *client.Client, self *selfContainer) {
	if err := ensureImage(ctx, cli, updaterImage()); err != nil {
		s.finishRelay(ctx, core.UpgradeFailed, fmt.Sprintf("Couldn't pull the updater image %s. Nothing was changed.", updaterImage()), err.Error())
		return
	}
	detail := updaterImage()
	if b, ok := s.app.Backup.(backupCreator); ok {
		if row, err := b.Create(ctx, "before-upgrade", ""); err == nil {
			detail = "backup " + row.File
		}
	}
	job := s.RelayUpdateStatus()
	env := []string{"RELAY_RESTART_ENGINES=0"}
	if job.RestartEngines {
		env[0] = "RELAY_RESTART_ENGINES=1"
	}
	id, err := startHelper(ctx, cli, self, "update", job.ID, relayUpdateScript, env...)
	if err != nil {
		s.finishRelay(ctx, core.UpgradeFailed, "Couldn't start the updater container. Nothing was changed.", err.Error())
		return
	}
	s.mu.Lock()
	s.relayJob.HelperID = id
	s.mu.Unlock()
	s.relayStep("prepare", "done", detail, 8, "")
	s.followRelayHelper(ctx, cli, id)
}

// followRelayHelper streams the helper's output into the job and finalizes it
// when the helper exits.
func (s *Service) followRelayHelper(ctx context.Context, cli *client.Client, id string) {
	rc, err := cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	var r helperResult
	if err == nil {
		pr, pw := io.Pipe()
		go func() {
			_, cerr := stdcopy.StdCopy(pw, pw, rc)
			pw.CloseWithError(cerr)
		}()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		lastPub := time.Time{}
		for sc.Scan() {
			line := sc.Text()
			switch r.feed(line) {
			case "step":
				s.relayAdvance(r.Steps[len(r.Steps)-1])
			case "pulled":
				d := shortSHA(r.PulledFrom) + " → " + shortSHA(r.PulledTo)
				if r.PulledFrom == r.PulledTo {
					d = "Already at " + shortSHA(r.PulledTo)
				}
				s.mu.Lock()
				s.relayJob.To = r.PulledTo
				if s.relayJob.From == "" {
					s.relayJob.From = r.PulledFrom
				}
				s.mu.Unlock()
				s.relayStep("pull", "running", d, 0, "")
			case "":
				s.mu.Lock()
				s.relayJob.Output = lastLines(r.Log, relayOutputLines)
				s.mu.Unlock()
				if time.Since(lastPub) > time.Second {
					lastPub = time.Now()
					s.publishRelayJob()
				}
			}
		}
		rc.Close()
	}
	if ctx.Err() != nil {
		return // Relay is shutting down, most likely restarted by this update
	}
	s.finalizeRelay(context.WithoutCancel(ctx), cli, id, r)
}

// relayAdvance marks the running step done and starts step id.
func (s *Service) relayAdvance(id string) {
	s.mu.Lock()
	if s.relayJob == nil {
		s.mu.Unlock()
		return
	}
	for i := range s.relayJob.Steps {
		st := &s.relayJob.Steps[i]
		if st.Status == "running" && st.ID != id {
			st.Status = "done"
		}
		if st.Status == "pending" && st.ID != id && st.ID != "rollback" && stepIndex(st.ID) < stepIndex(id) {
			st.Status = "skipped"
		}
	}
	s.mu.Unlock()
	info := relayStepInfo[id]
	s.relayStep(id, "running", "", info.progress, info.message)
}

func stepIndex(id string) int {
	for i, st := range relaySteps {
		if st.ID == id {
			return i
		}
	}
	return len(relaySteps)
}

func (s *Service) relayStep(id, status, detail string, progress int, message string) {
	s.mu.Lock()
	if s.relayJob == nil {
		s.mu.Unlock()
		return
	}
	for i := range s.relayJob.Steps {
		if s.relayJob.Steps[i].ID == id {
			s.relayJob.Steps[i].Status = status
			if detail != "" {
				s.relayJob.Steps[i].Detail = detail
			}
		}
	}
	if progress > 0 {
		s.relayJob.Progress = progress
	}
	if message != "" {
		s.relayJob.Message = message
	}
	s.mu.Unlock()
	s.persistRelayJob(context.Background())
	s.publishRelayJob()
}

func (s *Service) persistRelayJob(ctx context.Context) {
	if j := s.RelayUpdateStatus(); j != nil {
		if b, err := json.Marshal(j); err == nil {
			s.app.Store.PutKV(context.WithoutCancel(ctx), kvRelayJob, b)
		}
	}
}

func (s *Service) publishRelayJob() {
	if j := s.RelayUpdateStatus(); j != nil {
		s.app.Bus.Publish(events.RelayUpdate, j)
	}
}

func (s *Service) finishRelay(ctx context.Context, status, message, errText string) {
	s.mu.Lock()
	if s.relayJob == nil {
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	j := s.relayJob
	j.Status, j.Message, j.Error, j.FinishedAt = status, message, errText, &now
	if status == core.UpgradeSucceeded {
		j.Progress = 100
	}
	for i := range j.Steps {
		st := &j.Steps[i]
		switch {
		case st.Status == "running" && status == core.UpgradeSucceeded:
			st.Status = "done"
		case st.Status == "running":
			st.Status = "failed"
		case st.Status == "pending" && status == core.UpgradeSucceeded && st.ID != "rollback":
			st.Status = "done"
		case st.Status == "pending":
			st.Status = "skipped"
		}
	}
	s.mu.Unlock()
	s.persistRelayJob(ctx)
	s.publishRelayJob()
	s.app.Bus.Publish(events.EngineUpdates, map[string]any{"reason": "relay-update"})
}

// finalizeRelay decides the outcome from the helper's output and exit code.
func (s *Service) finalizeRelay(ctx context.Context, cli *client.Client, helperID string, r helperResult) {
	defer cli.ContainerRemove(ctx, helperID, container.RemoveOptions{Force: true})
	code := -1
	if info, err := cli.ContainerInspect(ctx, helperID); err == nil && info.State != nil {
		code = info.State.ExitCode
	}
	s.mu.Lock()
	if r.PulledTo != "" {
		s.relayJob.To = r.PulledTo
	}
	s.relayJob.Output = lastLines(r.Log, relayOutputLines)
	job := *s.relayJob
	s.mu.Unlock()
	sys := core.ActorFrom(ctx)

	if r.Done && code == 0 {
		to := firstNonEmpty(r.DoneCommit, job.To)
		running := s.app.Config.Commit
		if running != "" && to != "" && running != to {
			msg := fmt.Sprintf("The checkout and image were updated to %s, but the relay container still runs %s. Run docker compose up -d on the Docker host.", shortSHA(to), shortSHA(running))
			s.relayStep("verify", "running", "", 0, "")
			s.finishRelay(ctx, core.UpgradeFailed, msg, "relay container not recreated")
			s.app.Audit(ctx, core.AuditEntry{Actor: &sys, Action: "relay.update", Target: "relay", Detail: msg, Result: "failed"})
			return
		}
		s.relayStep("verify", "done", "Running "+firstNonEmpty(shortSHA(running), s.app.Config.Version), 98, "")
		msg := "Relay upgraded to " + shortSHA(to)
		s.finishRelay(ctx, core.UpgradeSucceeded, msg+".", "")
		s.app.Audit(ctx, core.AuditEntry{Actor: &sys, Action: "relay.update", Target: "relay", Detail: fmt.Sprintf("%s → %s", shortSHA(job.From), shortSHA(to)), Result: "ok"})
		s.app.Activity(ctx, "relay.update", "ok", msg, "relay", fmt.Sprintf("%s → %s", firstNonEmpty(shortSHA(job.From), "unknown"), shortSHA(to)))
		go s.checkRelay(context.WithoutCancel(ctx))
		return
	}

	failed := "prepare"
	if len(r.Steps) > 0 {
		failed = r.Steps[len(r.Steps)-1]
	}
	var msg string
	switch failed {
	case "pull":
		msg = "Couldn't update the checkout. Nothing was changed."
	case "build":
		msg = "The build failed. Relay keeps running the current version."
	case "restart":
		msg = "docker compose couldn't restart Relay."
	case "rollback":
		msg = "The new version didn't start."
	default:
		msg = "The update couldn't start. Nothing was changed."
	}
	errText := r.Error
	if errText == "" {
		errText = fmt.Sprintf("the updater exited with code %d", code)
	} else {
		msg = errText
	}
	s.finishRelay(ctx, core.UpgradeFailed, msg, errText)
	s.app.Audit(ctx, core.AuditEntry{Actor: &sys, Action: "relay.update", Target: "relay", Detail: errText, Result: "failed"})
	s.app.Activity(ctx, "relay.update", "warn", "Relay upgrade failed", "relay", msg)
	if s.app.Notify != nil {
		s.app.Notify.Notify(ctx, core.Notification{Event: model.EventReloadFailed, Level: "error", Title: "Relay upgrade failed", Message: msg, URL: "/settings/engines"})
	}
	go s.checkRelay(context.WithoutCancel(ctx))
}

// resumeRelayUpdate picks up an update after Relay restarted.
func (s *Service) resumeRelayUpdate(ctx context.Context) {
	job := s.RelayUpdateStatus()
	if job == nil || job.Status != core.UpgradeRunning {
		return
	}
	cli, err := newDockerClient()
	if err != nil {
		s.finishRelay(ctx, core.UpgradeFailed, "Relay restarted during the update and can't reach Docker to see how it ended.", err.Error())
		return
	}
	defer cli.Close()
	if job.HelperID == "" {
		s.finishRelay(ctx, core.UpgradeFailed, "Relay restarted before the updater started. Nothing was changed.", "interrupted")
		return
	}
	if _, err := cli.ContainerInspect(ctx, job.HelperID); err != nil {
		if running := s.app.Config.Commit; running != "" && running == job.To {
			s.finishRelay(ctx, core.UpgradeSucceeded, "Relay upgraded to "+shortSHA(running)+".", "")
			return
		}
		s.finishRelay(ctx, core.UpgradeFailed, "Relay restarted during the update and the updater container is gone.", strings.TrimSpace(err.Error()))
		return
	}
	s.mu.Lock()
	if s.relayJob != nil {
		s.relayJob.Output = ""
	}
	s.mu.Unlock()
	s.followRelayHelper(ctx, cli, job.HelperID)
}
