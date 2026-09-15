package apply

// Engine containers. An engine that isn't needed (the proxy engine that
// isn't selected, HAProxy stopped by an admin or without backends) doesn't
// only stop its process: Relay stops its container too, and starts it again
// before the engine is used. Without Docker access (app.Containers nil or
// the Docker API unreachable) engines are only stopped inside their containers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/store"
)

// kvStoppedEngines records engines an admin stopped (HAProxy), so their
// stopped container reads as standby and PushLive keeps them stopped.
const kvStoppedEngines = "apply.stoppedEngines"

func (s *Service) stoppedEngines(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if b, err := s.app.Store.GetKV(ctx, kvStoppedEngines); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (s *Service) setStoppedEngine(ctx context.Context, engine string, stopped bool) {
	m := s.stoppedEngines(ctx)
	if m[engine] == stopped {
		return
	}
	if stopped {
		m[engine] = true
	} else {
		delete(m, engine)
	}
	b, _ := json.Marshal(m)
	if err := s.app.Store.PutKV(ctx, kvStoppedEngines, b); err != nil {
		s.log.Warn("save stopped engines", "err", err)
	}
}

func anyEngineLabel(engine string) string {
	if engine == agent.EngineHAProxy {
		return "HAProxy"
	}
	return engineLabel(engine)
}

// ensureEngine starts an engine's stopped container. It does nothing when the
// agent answers or the container isn't stopped (the usual "unreachable" error
// then explains the problem).
func (s *Service) ensureEngine(ctx context.Context, engine string, starting func()) error {
	ctrs, c := s.app.Containers, s.app.Client(engine)
	if ctrs == nil || c == nil {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err := c.Status(sctx)
	cancel()
	if err == nil || ctrs.ContainerState(ctx, engine) != "stopped" {
		return nil
	}
	if starting != nil {
		starting()
	}
	s.log.Info("starting stopped engine container", "engine", engine)
	return ctrs.StartContainer(ctx, engine)
}

// engineIdle reports whether an engine that isn't running may have its
// container stopped.
func (s *Service) engineIdle(ctx context.Context, engine, selected string, live *store.VersionRow, st core.EngineState) bool {
	if agent.IsProxyEngine(engine) {
		return engine != selected
	}
	return live == nil || !live.HAProxyRunning || st.Stopped || s.stoppedEngines(ctx)[engine]
}

// manageContainers stops the containers of idle engines and starts the
// selected proxy engine's container when it is stopped. reconcile calls it
// with the apply lock held; stoppedNow lists engines it just stopped.
func (s *Service) manageContainers(ctx context.Context, st *core.EnginesStatus, live *store.VersionRow, selected string, stoppedNow map[string]bool) {
	ctrs := s.app.Containers
	if ctrs == nil || st == nil || s.switching() != "" {
		return
	}
	states := map[string]core.EngineState{agent.EngineNginx: st.Nginx, agent.EngineEdge: st.Edge, agent.EngineHAProxy: st.HAProxy}
	if ps := states[selected]; !ps.Reachable && ctrs.ContainerState(ctx, selected) == "stopped" {
		s.log.Info("the selected proxy engine's container is stopped, starting it", "engine", selected)
		if err := ctrs.StartContainer(ctx, selected); err != nil {
			s.log.Warn("start "+selected+" container", "err", err)
		}
	}
	for _, engine := range []string{agent.EngineNginx, agent.EngineEdge, agent.EngineHAProxy} {
		es := states[engine]
		if engine == selected || !es.Reachable || (es.Running && !stoppedNow[engine]) || !s.engineIdle(ctx, engine, selected, live, es) {
			continue
		}
		reason := "HAProxy is stopped"
		if agent.IsProxyEngine(engine) {
			reason = engineLabel(selected) + " is the selected proxy engine"
		} else if live != nil && !live.HAProxyRunning {
			reason = "HAProxy has no backends or frontends"
		}
		if err := ctrs.StopContainer(ctx, engine, reason); err != nil {
			s.log.Warn("stop idle "+engine+" container", "err", err)
		}
	}
}

// annotateContainers adds the container state of engines whose agent is
// unreachable, and whether the container is stopped on purpose (standby).
func (s *Service) annotateContainers(ctx context.Context, out *core.EnginesStatus) {
	ctrs := s.app.Containers
	if ctrs == nil {
		return
	}
	for _, e := range []struct {
		name string
		st   *core.EngineState
	}{{agent.EngineNginx, &out.Nginx}, {agent.EngineEdge, &out.Edge}, {agent.EngineHAProxy, &out.HAProxy}} {
		if e.st.Reachable {
			continue
		}
		e.st.Container = ctrs.ContainerState(ctx, e.name)
		if e.st.Container != "stopped" {
			continue
		}
		e.st.Error = "the " + anyEngineLabel(e.name) + " container is stopped"
		if agent.IsProxyEngine(e.name) {
			e.st.Standby = e.name != out.Proxy
			continue
		}
		live, _ := s.app.Store.LiveVersion(ctx, false)
		e.st.Standby = live == nil || !live.HAProxyRunning || s.stoppedEngines(ctx)[e.name]
	}
}

// EngineAction starts, stops or reloads an engine (admin UI / API). Start
// brings a stopped container back and pushes the live version first; stopping
// HAProxy or a proxy engine that isn't selected stops its container too.
func (s *Service) EngineAction(ctx context.Context, engine, action string) (*agent.ActionResponse, error) {
	c := s.app.Client(engine)
	if c == nil {
		return nil, httpx.Errorf(http.StatusNotFound, "not_found", "unknown engine (nginx | haproxy | edge)")
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	selected := s.app.ProxyEngine(ctx)
	ctrs := s.app.Containers
	containerStopped := func() bool { return ctrs != nil && ctrs.ContainerState(ctx, engine) == "stopped" }
	label := anyEngineLabel(engine)

	switch action {
	case "start":
		if agent.IsProxyEngine(engine) && engine != selected {
			return nil, httpx.Errorf(http.StatusConflict, "not_selected", fmt.Sprintf("%s isn't the selected proxy engine; switch engines in Settings → Proxy engine.", label))
		}
		if err := s.ensureEngine(ctx, engine, nil); err != nil {
			return nil, httpx.Errorf(http.StatusServiceUnavailable, "engine_unavailable", err.Error())
		}
		s.setStoppedEngine(ctx, engine, false)
		// The container may hold an older release than the live version.
		if err := s.PushLive(ctx, engine); err != nil {
			s.log.Warn("push the live version before starting", "engine", engine, "err", err)
		}
		return c.Start(ctx)
	case "stop":
		resp, err := c.Stop(ctx)
		if err != nil {
			if containerStopped() {
				if engine == agent.EngineHAProxy {
					s.setStoppedEngine(ctx, engine, true)
				}
				return &agent.ActionResponse{OK: true, Output: "already stopped"}, nil
			}
			return nil, err
		}
		if !resp.OK {
			return resp, nil
		}
		if engine == agent.EngineHAProxy {
			s.setStoppedEngine(ctx, engine, true)
		}
		if ctrs != nil && (engine == agent.EngineHAProxy || engine != selected) {
			if err := ctrs.StopContainer(ctx, engine, "stopped by "+core.ActorFrom(ctx).Label()); err != nil {
				s.log.Warn("stop "+engine+" container", "err", err)
			}
		}
		return resp, nil
	case "reload":
		resp, err := c.Reload(ctx)
		if err != nil && containerStopped() {
			return nil, httpx.Errorf(http.StatusConflict, "engine_stopped", fmt.Sprintf("The %s container is stopped; start %s first.", label, label))
		}
		return resp, err
	}
	return nil, httpx.Errorf(http.StatusNotFound, "not_found", "unknown action (start | stop | reload)")
}
