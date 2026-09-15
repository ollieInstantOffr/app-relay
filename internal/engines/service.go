// Package engines checks Docker Hub for newer nginx/HAProxy images and
// upgrades the engine containers in place (slice: engine).
package engines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	kvReleases = "engines.releases"
	kvNotified = "engines.notified"
	kvLastJob  = "engines.lastUpgrade"
)

type Service struct {
	app *core.App
	log *slog.Logger
	hub *hubClient

	ctx context.Context

	mu        sync.Mutex
	releases  map[string][]core.EngineRelease
	checkedAt *time.Time
	checkErr  string
	lastCheck time.Time
	job       *core.UpgradeJob
	checkMu   sync.Mutex
	// Relay self-update (relay.go, relay_update.go).
	relayInfo *core.RelayUpdateInfo
	relayJob  *core.RelayUpdateJob
	// interrupted: relay restarted during an upgrade; recover containers once.
	interrupted bool
	// Engine container states and the compose project (containers.go).
	ctr containerCache
}

func New(app *core.App) *Service {
	log := app.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{app: app, log: log.With("svc", "engines"), hub: newHubClient(), releases: map[string][]core.EngineRelease{}, ctx: context.Background()}
}

type releaseCache struct {
	CheckedAt *time.Time                      `json:"checkedAt"`
	Error     string                          `json:"error"`
	Releases  map[string][]core.EngineRelease `json:"releases"`
}

func (s *Service) Start(ctx context.Context) error {
	s.ctx = ctx
	if b, err := s.app.Store.GetKV(ctx, kvReleases); err == nil {
		var c releaseCache
		if json.Unmarshal(b, &c) == nil {
			s.mu.Lock()
			s.releases, s.checkedAt, s.checkErr = c.Releases, c.CheckedAt, c.Error
			if s.releases == nil {
				s.releases = map[string][]core.EngineRelease{}
			}
			s.mu.Unlock()
		}
	}
	if b, err := s.app.Store.GetKV(ctx, kvLastJob); err == nil {
		var j core.UpgradeJob
		if json.Unmarshal(b, &j) == nil && j.ID != "" {
			if j.Status == core.UpgradeRunning { // relay restarted mid-upgrade
				j.Status, j.Error = core.UpgradeFailed, "Relay restarted while the upgrade was running."
				for i := range j.Steps {
					switch j.Steps[i].Status {
					case "running":
						j.Steps[i].Status = "failed"
					case "pending":
						j.Steps[i].Status = "skipped"
					}
				}
				s.interrupted = true
			}
			s.job = &j
		}
	}
	s.loadRelay(ctx)
	go s.loop(ctx)
	return nil
}

func (s *Service) settings(ctx context.Context) model.EnginesSettings {
	v, err := store.LoadSettings[model.EnginesSettings](ctx, s.app.Store, model.SettingsEngines)
	if err != nil {
		return store.DefaultEngines()
	}
	return v
}

func (s *Service) loop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	if s.interrupted {
		s.recoverInterrupted(ctx)
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		s.adoptImages(ctx)
		set := s.settings(ctx)
		s.mu.Lock()
		due := s.checkedAt == nil || time.Since(*s.checkedAt) >= time.Duration(set.CheckIntervalHours)*time.Hour
		if s.checkErr != "" && s.checkedAt != nil && time.Since(*s.checkedAt) >= time.Hour {
			due = true // retry failed checks hourly
		}
		s.mu.Unlock()
		if set.AutoCheck && due {
			if _, err := s.CheckUpdates(ctx); err != nil {
				s.log.Warn("engine update check", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// adoptImages records the running official images as desired when nothing
// is recorded yet (first start with the official-image compose file).
func (s *Service) adoptImages(ctx context.Context) {
	cli, err := newDockerClient()
	if err != nil {
		return
	}
	defer cli.Close()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	project := composeProject(dctx, cli)
	set := s.settings(ctx)
	changed := false
	for _, engine := range []string{"nginx", "haproxy"} {
		c, err := findEngineContainer(dctx, cli, project, engine)
		if err != nil {
			continue
		}
		if _, _, ok := officialRef(c.Image); !ok {
			continue
		}
		if engine == "nginx" && set.NginxImage == "" {
			set.NginxImage, changed = c.Image, true
		}
		if engine == "haproxy" && set.HAProxyImage == "" {
			set.HAProxyImage, changed = c.Image, true
		}
	}
	if changed {
		if err := s.app.Store.PutSettings(ctx, model.SettingsEngines, set); err == nil {
			s.app.Bus.Publish(events.EngineUpdates, map[string]any{"reason": "adopted"})
		}
	}
}

// CheckUpdates queries Docker Hub for both engines.
func (s *Service) CheckUpdates(ctx context.Context) (*core.EngineUpdates, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.mu.Lock()
	recent := time.Since(s.lastCheck) < 20*time.Second
	s.mu.Unlock()
	if !recent {
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		fetched := map[string][]core.EngineRelease{}
		var errs []string
		for _, engine := range []string{"nginx", "haproxy"} {
			rel, err := s.hub.releases(cctx, engine)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", engine, err))
				continue
			}
			fetched[engine] = rel
		}
		now := time.Now().UTC()
		s.mu.Lock()
		s.lastCheck = now
		s.checkedAt = &now
		s.checkErr = ""
		if len(errs) > 0 {
			s.checkErr = "Couldn't check for updates — " + joinErrs(errs)
		}
		for k, v := range fetched {
			s.releases[k] = v
		}
		cache := releaseCache{CheckedAt: s.checkedAt, Error: s.checkErr, Releases: s.releases}
		s.mu.Unlock()
		if b, err := json.Marshal(cache); err == nil {
			s.app.Store.PutKV(context.WithoutCancel(ctx), kvReleases, b)
		}
		if j := s.RelayUpdateStatus(); j == nil || j.Status != core.UpgradeRunning {
			s.checkRelay(ctx)
		}
	}
	u, err := s.Updates(ctx)
	if err != nil {
		return nil, err
	}
	s.notifyNew(ctx, u)
	s.app.Bus.Publish(events.EngineUpdates, map[string]any{"nginx": u.Nginx.UpdateAvailable, "haproxy": u.HAProxy.UpdateAvailable})
	return u, nil
}

func joinErrs(errs []string) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += e
	}
	return out
}

// notifyNew sends one activity + notification per newly available version.
func (s *Service) notifyNew(ctx context.Context, u *core.EngineUpdates) {
	notified := map[string]string{}
	if b, err := s.app.Store.GetKV(ctx, kvNotified); err == nil {
		json.Unmarshal(b, &notified)
	}
	changed := s.notifyRelay(ctx, u.Relay, notified)
	for _, info := range []core.EngineUpdateInfo{u.Nginx, u.HAProxy} {
		if info.Inactive || !info.UpdateAvailable || info.Latest == nil || notified[info.Engine] == info.Latest.Version {
			continue
		}
		notified[info.Engine] = info.Latest.Version
		changed = true
		name := engineName(info.Engine)
		title := fmt.Sprintf("%s %s is available", name, info.Latest.Version)
		detail := fmt.Sprintf("Running %s · %s channel", orDash(info.Version), info.Channel)
		s.app.Activity(ctx, "engine.update", "info", title, info.Engine, detail)
		if s.app.Notify != nil {
			s.app.Notify.Notify(ctx, core.Notification{Event: model.EventEngineUpdateAvailable, Level: "info", Title: title, Message: detail + ". Upgrade from Settings → Updates.", URL: "/settings/engines"})
		}
	}
	if changed {
		b, _ := json.Marshal(notified)
		s.app.Store.PutKV(context.WithoutCancel(ctx), kvNotified, b)
	}
}

func engineName(e string) string {
	if e == "haproxy" {
		return "HAProxy"
	}
	return "nginx"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// Updates assembles cached release data with the live container state.
func (s *Service) Updates(ctx context.Context) (*core.EngineUpdates, error) {
	set := s.settings(ctx)
	s.mu.Lock()
	out := &core.EngineUpdates{AutoCheck: set.AutoCheck, CheckedAt: s.checkedAt, CheckError: s.checkErr}
	releases := map[string][]core.EngineRelease{}
	for k, v := range s.releases {
		releases[k] = v
	}
	s.mu.Unlock()
	if out.CheckedAt != nil && set.AutoCheck {
		next := out.CheckedAt.Add(time.Duration(set.CheckIntervalHours) * time.Hour)
		out.NextCheckAt = &next
	}

	var statuses core.EnginesStatus
	if s.app.Engine != nil {
		if st, err := s.app.Engine.Status(ctx); err == nil {
			statuses = *st
		}
	}
	out.ProxyEngine = statuses.Proxy
	if out.ProxyEngine == "" {
		out.ProxyEngine = s.app.ProxyEngine(ctx)
	}
	out.LBEngine = statuses.LB
	if out.LBEngine == "" {
		out.LBEngine = s.app.LBEngine(ctx)
	}

	containers := map[string]*engineContainer{}
	containerErr := map[string]string{}
	if cli, err := newDockerClient(); err != nil {
		out.DockerError = err.Error()
	} else {
		dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		if _, err := cli.Ping(dctx); err != nil {
			out.DockerError = "Docker API unreachable: " + err.Error()
		} else {
			project := composeProject(dctx, cli)
			out.ComposeProject = project
			for _, engine := range []string{"nginx", "haproxy"} {
				c, err := findEngineContainer(dctx, cli, project, engine)
				if err != nil {
					containerErr[engine] = err.Error()
					continue
				}
				containers[engine] = c
			}
		}
		cancel()
		cli.Close()
	}

	build := func(engine string, st core.EngineState, channel, desired string) core.EngineUpdateInfo {
		info := core.EngineUpdateInfo{Engine: engine, Channel: channel, Version: st.Version, Reachable: st.Reachable, Running: st.Running,
			DesiredImage: desired, Channels: map[string]*core.EngineRelease{}, Modules: st.Modules, MissingModules: []string{},
			Inactive: inactiveEngine(engine, out.ProxyEngine, out.LBEngine), Standby: st.Standby}
		if info.Modules == nil {
			info.Modules = []string{}
		}
		rel := releases[engine]
		for _, ch := range engineChannels[engine] {
			info.Channels[ch] = newestInChannel(engine, ch, rel)
		}
		info.Latest = info.Channels[channel]
		if c := containers[engine]; c != nil {
			info.Container, info.Image = c.Name, c.Image
			if _, tag, ok := officialRef(c.Image); ok {
				info.Official = true
				if v, ok := parseVersion(tag); ok {
					info.ImageVersion = v.String()
				}
			}
		}
		current := info.Version
		if current == "" {
			current = info.ImageVersion
		}
		if cur, ok := parseVersion(current); ok && info.Latest != nil {
			if lv, ok := parseVersion(info.Latest.Version); ok && cur.less(lv) {
				info.UpdateAvailable = true
			}
		}
		info.ChangesURL = changesURL(engine, firstNonEmpty(versionOf(info.Latest), current))
		if info.Image != "" && desired != "" && info.Official && info.Image != desired {
			dv, _ := parseVersion(tagOf(desired))
			info.Drift = &core.EngineDrift{RunningImage: info.Image, RunningVersion: firstNonEmpty(info.ImageVersion, info.Version), DesiredImage: desired, DesiredVersion: dv.String()}
		}
		switch {
		case info.Inactive && (out.DockerError != "" || containers[engine] == nil || !info.Official):
			// Not the selected proxy / load balancer engine: a missing
			// container or image problem isn't an error.
		case out.DockerError != "":
			info.UpgradeBlocker = "Relay can't reach the Docker API (mount /var/run/docker.sock into the relay container)."
		case containers[engine] == nil:
			info.UpgradeBlocker = "Relay can't find the " + engineName(engine) + " container: " + containerErr[engine]
		case !info.Official:
			info.UpgradeBlocker = fmt.Sprintf("%s runs %s, not the official %s image. Switch docker-compose.yml to the official image first (deploy/UPGRADES.md).", info.Container, info.Image, engine)
		default:
			info.CanUpgrade = true
		}
		return info
	}
	out.Nginx = build("nginx", statuses.Nginx, set.NginxChannel, set.NginxImage)
	out.HAProxy = build("haproxy", statuses.HAProxy, set.HAProxyChannel, set.HAProxyImage)
	out.Relay = s.relayInfoLive()
	return out, nil
}

func versionOf(r *core.EngineRelease) string {
	if r == nil {
		return ""
	}
	return r.Version
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func tagOf(ref string) string {
	_, tag, _ := officialRef(ref)
	return tag
}

func hasModule(mods []string, m string) bool {
	for _, x := range mods {
		if x == m {
			return true
		}
	}
	return false
}

// KeepRunningImage accepts the image compose started as the desired one.
func (s *Service) KeepRunningImage(ctx context.Context, engine string) error {
	if engine != "nginx" && engine != "haproxy" {
		return errors.New("unknown engine")
	}
	u, err := s.Updates(ctx)
	if err != nil {
		return err
	}
	info := u.Nginx
	if engine == "haproxy" {
		info = u.HAProxy
	}
	if info.Image == "" {
		return fmt.Errorf("the %s container wasn't found", engineName(engine))
	}
	set := s.settings(ctx)
	if engine == "nginx" {
		set.NginxImage = info.Image
	} else {
		set.HAProxyImage = info.Image
	}
	if err := s.app.Store.PutSettings(ctx, model.SettingsEngines, set); err != nil {
		return err
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "engine.keep_image", Target: engine, Detail: info.Image, Result: "ok"})
	s.app.Bus.Publish(events.EngineUpdates, map[string]any{"reason": "keep"})
	return nil
}

func (s *Service) UpgradeStatus() *core.UpgradeJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil
	}
	j := *s.job
	j.Steps = append([]core.UpgradeStep(nil), s.job.Steps...)
	return &j
}

// inactiveEngine reports whether an image engine isn't selected: nginx while
// Relay Edge is the proxy engine, HAProxy while Relay Balancer is the load
// balancer engine.
func inactiveEngine(engine, proxyEngine, lbEngine string) bool {
	switch {
	case agent.IsProxyEngine(engine):
		return engine != proxyEngine
	case agent.IsLBEngine(engine):
		return engine != agent.NormalizeLBEngine(lbEngine)
	}
	return false
}

// activeEngine reports whether engine must be running: HAProxy decides by
// itself (backends), nginx only while it is the selected proxy engine.
func (s *Service) activeProxy(ctx context.Context, engine string) bool {
	return agent.IsProxyEngine(engine) && s.app.ProxyEngine(ctx) == engine
}

// agentClient returns the agent client for an engine.
func (s *Service) agentClient(engine string) *agent.Client {
	return s.app.Client(engine)
}
