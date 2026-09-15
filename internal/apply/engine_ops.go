package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Operations used by the engine image upgrade (internal/engines).

// TryLockApply takes the apply mutex so no apply, rollback or reconcile runs
// while an engine container is being swapped.
func (s *Service) TryLockApply() (func(), bool) {
	if !s.applyMu.TryLock() {
		return nil, false
	}
	return s.applyMu.Unlock, true
}

// LiveRelease returns the live version's files for an engine (nil when
// nothing was applied yet).
func (s *Service) LiveRelease(ctx context.Context, engine string) (agent.Files, string, bool, int64, error) {
	live, err := s.app.Store.LiveVersion(ctx, true)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", false, 0, nil
	}
	if err != nil {
		return nil, "", false, 0, err
	}
	if engine == agent.EngineHAProxy {
		return agent.Files{"haproxy.cfg": live.HAProxyCfg}, live.HAProxyHash, live.HAProxyRunning, live.ID, nil
	}
	var files agent.Files
	if err := json.Unmarshal([]byte(live.NginxFiles), &files); err != nil {
		return nil, "", false, 0, fmt.Errorf("live version v%d files: %w", live.ID, err)
	}
	return files, live.NginxHash, true, live.ID, nil
}

// PushLive makes the engine run the live version (no new version). Callers
// hold the apply lock.
func (s *Service) PushLive(ctx context.Context, engine string) error {
	files, hash, running, _, err := s.LiveRelease(ctx, engine)
	if err != nil || files == nil {
		return err
	}
	c := s.app.Nginx
	if engine == agent.EngineHAProxy {
		c = s.app.HAProxy
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if st.ConfigHash == hash && st.Running == running {
		return nil
	}
	resp, err := c.Apply(ctx, agent.ApplyRequest{Files: files, Hash: hash, Stop: !running})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s %s failed: %s", engine, resp.Stage, firstErrorLine(resp.Output))
	}
	return nil
}

// HealthyHosts probes every enabled live host through nginx and returns the
// ids that answer without 502/503/504.
func (s *Service) HealthyHosts(ctx context.Context) []string {
	_, live, err := s.liveVersion(ctx)
	if err != nil || live == nil {
		return nil
	}
	var ids []string
	for _, h := range live.Hosts {
		if h.Enabled && len(h.Domains) > 0 {
			ids = append(ids, h.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) > 20 {
		ids = ids[:20]
	}
	var out []string
	for id, res := range s.probeAll(ctx, live, ids) {
		if res.ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// WatchHosts probes ids once per second for window; it returns a failure
// description when a host failed every probe or nginx stopped running.
func (s *Service) WatchHosts(ctx context.Context, ids []string, window time.Duration, tick func(sec, total int)) string {
	_, live, err := s.liveVersion(ctx)
	if err != nil || live == nil {
		return ""
	}
	secs := int(window / time.Second)
	passed := map[string]bool{}
	last := map[string]probeResult{}
	t0 := time.Now()
	for i := 1; i <= secs; i++ {
		if d := time.Until(t0.Add(time.Duration(i) * time.Second)); d > 0 {
			time.Sleep(d)
		}
		if tick != nil {
			tick(i, secs)
		}
		if st, err := s.app.Nginx.Status(ctx); err == nil && !st.Running && st.Configured {
			return "nginx stopped running (" + orText(st.ExitError, "exited") + ")"
		}
		var pending []string
		for _, id := range ids {
			if !passed[id] {
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			if i >= 3 {
				return ""
			}
			continue
		}
		for id, res := range s.probeAll(ctx, live, pending) {
			last[id] = res
			if res.ok {
				passed[id] = true
			}
		}
	}
	for _, id := range ids {
		if !passed[id] {
			return fmt.Sprintf("%s %s for %d s", hostName(live, id), last[id].detail, secs)
		}
	}
	return ""
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func hostName(snap *model.Snapshot, id string) string {
	for _, h := range snap.Hosts {
		if h.ID == id && len(h.Domains) > 0 {
			return h.Domains[0]
		}
	}
	return id
}
