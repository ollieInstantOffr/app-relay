// Package lbengine describes the two load balancer engines, HAProxy (default)
// and Relay Balancer, for the apply pipeline and the load balancer API:
// how each renders its release, its main file and its preview renderers.
// Exactly one of them is the active load balancer engine
// (general.lbEngine, recorded on each config version).
package lbengine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/render/balancer"
	"github.com/instantoffr/relay/internal/render/haproxy"
)

// Renderer renders the files of one load balancer engine.
type Renderer struct {
	// MainFile is the release's only file (haproxy.cfg | balancer.json); its
	// content is stored in config_versions.haproxy_cfg.
	MainFile string
	// Comment starts a comment line in previews ("#" | "//").
	Comment string
	// HasBackends reports whether the engine should run for a snapshot.
	HasBackends func(*model.Snapshot) bool
	// Render returns the release files ({MainFile: …}).
	Render func(*model.Snapshot, render.Env) (agent.Files, error)
	// Backend / Frontend render one section for previews.
	Backend  func(*model.Snapshot, *model.Backend) string
	Frontend func(*model.Snapshot, *model.Frontend) string
}

// Renderers is keyed by agent engine name (tests may replace entries).
var Renderers = map[string]Renderer{
	agent.EngineHAProxy: {
		MainFile: "haproxy.cfg", Comment: "#", HasBackends: haproxy.HasBackends, Render: renderHAProxy,
		Backend: haproxy.RenderBackend, Frontend: haproxy.RenderFrontend,
	},
	agent.EngineBalancer: {
		MainFile: balancer.ConfigFile, Comment: "//", HasBackends: balancer.HasBackends, Render: renderBalancer,
		Backend: balancer.RenderBackend, Frontend: balancer.RenderFrontend,
	},
}

// For returns the renderer of a load balancer engine.
func For(engine string) (Renderer, error) {
	r, ok := Renderers[engine]
	if !ok {
		return Renderer{}, fmt.Errorf("no renderer for load balancer engine %q", engine)
	}
	return r, nil
}

// MainFile returns the main file name of an engine ("haproxy.cfg" for unknown).
func MainFile(engine string) string {
	if r, ok := Renderers[agent.NormalizeLBEngine(engine)]; ok && r.MainFile != "" {
		return r.MainFile
	}
	return "haproxy.cfg"
}

// Label is the display name: HAProxy | Relay Balancer.
func Label(engine string) string {
	if engine == agent.EngineBalancer {
		return "Relay Balancer"
	}
	return "HAProxy"
}

// CheckName is the validation command shown in progress and errors.
func CheckName(engine string) string {
	if engine == agent.EngineBalancer {
		return "relay balancer check"
	}
	return "haproxy -c"
}

// DiffPath is where an engine's main file appears in diffs: haproxy.cfg
// keeps its historic path, Relay Balancer's file lives under balancer/.
func DiffPath(engine string) string {
	if engine == agent.EngineBalancer {
		return "balancer/" + balancer.ConfigFile
	}
	return "haproxy.cfg"
}

// ArchivePath is the path of the main file inside a version download.
func ArchivePath(engine string) string {
	if engine == agent.EngineBalancer {
		return "balancer/" + balancer.ConfigFile
	}
	return "haproxy/haproxy.cfg"
}

// Files reconstructs a stored release from its main file content.
func Files(engine, content string) agent.Files {
	return agent.Files{MainFile(engine): content}
}

func renderHAProxy(snap *model.Snapshot, env render.Env) (agent.Files, error) {
	cfg, err := haproxy.Render(snap, env)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg) == "" {
		if haproxy.HasBackends(snap) {
			return nil, errors.New("the HAProxy renderer produced no configuration")
		}
		cfg = "# HAProxy is stopped: no load-balancer backends are configured.\n"
	}
	return agent.Files{"haproxy.cfg": cfg}, nil
}

func renderBalancer(snap *model.Snapshot, env render.Env) (agent.Files, error) {
	files, err := balancer.Render(snap, env)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(files[balancer.ConfigFile]) == "" {
		if balancer.HasBackends(snap) {
			return nil, errors.New("the Relay Balancer renderer produced no configuration")
		}
		// Stored only: Relay Balancer is stopped without backends.
		files = agent.Files{balancer.ConfigFile: "{\"schema\": 1, \"notes\": [\"Relay Balancer is stopped: no load-balancer backends are configured.\"], \"frontends\": [], \"backends\": []}\n"}
	}
	return files, nil
}
