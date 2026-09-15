package engines

// Starting and stopping engine containers. Relay stops the container of an
// engine that isn't needed (the proxy engine that isn't selected, HAProxy
// when it's stopped or has no backends) and starts it again before the
// engine is used (internal/apply decides when).

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/events"
)

const containerStateTTL = 5 * time.Second

type containerCache struct {
	mu          sync.Mutex
	project     string
	haveProject bool
	states      map[string]cachedContainer
}

type cachedContainer struct {
	state string
	at    time.Time
}

func containerEngine(engine string) bool {
	return engine == agent.EngineNginx || engine == agent.EngineEdge || engine == agent.EngineHAProxy
}

func containerLabel(engine string) string {
	switch engine {
	case agent.EngineEdge:
		return "Relay Edge"
	case agent.EngineHAProxy:
		return "HAProxy"
	}
	return "nginx"
}

// docker connects to the Docker API and resolves Relay's compose project once.
func (s *Service) docker(ctx context.Context) (*client.Client, string, error) {
	cli, err := newDockerClient()
	if err != nil {
		return nil, "", err
	}
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		return nil, "", err
	}
	s.ctr.mu.Lock()
	project, ok := s.ctr.project, s.ctr.haveProject
	s.ctr.mu.Unlock()
	if !ok {
		project = composeProject(ctx, cli)
		s.ctr.mu.Lock()
		s.ctr.project, s.ctr.haveProject = project, true
		s.ctr.mu.Unlock()
	}
	return cli, project, nil
}

func (s *Service) rememberContainer(engine, state string) {
	s.ctr.mu.Lock()
	defer s.ctr.mu.Unlock()
	if s.ctr.states == nil {
		s.ctr.states = map[string]cachedContainer{}
	}
	s.ctr.states[engine] = cachedContainer{state: state, at: time.Now()}
}

// ContainerState reports "running", "stopped" or "missing" for an engine's
// container, or "" when the Docker API isn't reachable.
func (s *Service) ContainerState(ctx context.Context, engine string) string {
	if !containerEngine(engine) {
		return ""
	}
	s.ctr.mu.Lock()
	if c, ok := s.ctr.states[engine]; ok && time.Since(c.at) < containerStateTTL {
		s.ctr.mu.Unlock()
		return c.state
	}
	s.ctr.mu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	state := ""
	if cli, project, err := s.docker(dctx); err == nil {
		c, err := findEngineContainer(dctx, cli, project, engine)
		switch {
		case err != nil:
			if dctx.Err() == nil {
				state = "missing"
			}
		case c.State == "running" || c.State == "restarting":
			state = "running"
		default:
			state = "stopped"
		}
		cli.Close()
	}
	s.rememberContainer(engine, state)
	return state
}

// StartContainer starts an engine's stopped container and waits until its
// agent answers.
func (s *Service) StartContainer(ctx context.Context, engine string) error {
	if !containerEngine(engine) {
		return fmt.Errorf("unknown engine %q", engine)
	}
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cli, project, err := s.docker(dctx)
	if err != nil {
		return fmt.Errorf("Docker API unavailable: %w", err)
	}
	defer cli.Close()
	c, err := findEngineContainer(dctx, cli, project, engine)
	if err != nil {
		return err
	}
	started := c.State != "running"
	if started {
		s.log.Info("starting engine container", "engine", engine, "container", c.Name)
		if err := cli.ContainerStart(dctx, c.ID, container.StartOptions{}); err != nil {
			return fmt.Errorf("start %s: %w", c.Name, err)
		}
	}
	s.rememberContainer(engine, "running")
	if err := s.waitAgent(dctx, cli, c.ID, engine, "", 60*time.Second, false); err != nil {
		return fmt.Errorf("%s started but its agent didn't answer: %w", c.Name, err)
	}
	if started {
		s.app.Activity(ctx, "engine.container", "info", "Started the "+c.Name+" container", engine, containerLabel(engine)+" is needed again")
		s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": engine, "reachable": true})
	}
	return nil
}

// StopContainer stops an engine's container gracefully; reason is shown in
// the activity feed.
func (s *Service) StopContainer(ctx context.Context, engine, reason string) error {
	if !containerEngine(engine) {
		return fmt.Errorf("unknown engine %q", engine)
	}
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cli, project, err := s.docker(dctx)
	if err != nil {
		return fmt.Errorf("Docker API unavailable: %w", err)
	}
	defer cli.Close()
	c, err := findEngineContainer(dctx, cli, project, engine)
	if err != nil {
		return err
	}
	if c.State != "running" && c.State != "restarting" {
		s.rememberContainer(engine, "stopped")
		return nil
	}
	s.log.Info("stopping idle engine container", "engine", engine, "container", c.Name, "reason", reason)
	timeout := 30
	if err := cli.ContainerStop(dctx, c.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop %s: %w", c.Name, err)
	}
	s.rememberContainer(engine, "stopped")
	s.app.Activity(ctx, "engine.container", "info", "Stopped the "+c.Name+" container", engine, reason)
	s.app.Bus.Publish(events.EngineChanged, map[string]any{"engine": engine, "running": false, "reachable": false})
	return nil
}
