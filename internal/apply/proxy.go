package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/lb/lbengine"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/render/edge"
	"github.com/instantoffr/relay/internal/render/nginx"
	"github.com/instantoffr/relay/internal/store"
)

// proxyRenderer renders the files of one proxy engine (nginx or Relay Edge).
type proxyRenderer struct {
	render func(*model.Snapshot, render.Env) (agent.Files, error)
	host   func(*model.Snapshot, *model.ProxyHost, render.Env) (string, error)
	stream func(*model.Snapshot, *model.Stream, render.Env) (string, error)
}

// proxyRenderers is keyed by agent engine name.
var proxyRenderers = map[string]proxyRenderer{
	agent.EngineNginx: {render: nginx.Render, host: nginx.RenderHost, stream: nginx.RenderStream},
	agent.EngineEdge:  {render: edge.Render, host: edge.RenderHost, stream: edge.RenderStream},
}

func rendererFor(engine string) (proxyRenderer, error) {
	r, ok := proxyRenderers[engine]
	if !ok {
		return proxyRenderer{}, fmt.Errorf("no renderer for proxy engine %q", engine)
	}
	return r, nil
}

// checkName is the validation command shown in progress and errors.
func checkName(engine string) string {
	switch engine {
	case agent.EngineEdge:
		return "relay edge check"
	case agent.EngineHAProxy, agent.EngineBalancer:
		return lbengine.CheckName(engine)
	}
	return "nginx -t"
}

// snapshotEngine is the proxy engine a snapshot selects.
func snapshotEngine(snap *model.Snapshot) string {
	if snap == nil {
		return agent.EngineNginx
	}
	return agent.NormalizeProxyEngine(snap.General.ProxyEngine)
}

// rowEngine is the proxy engine a stored version was applied with.
func rowEngine(row *store.VersionRow) string {
	if row == nil {
		return agent.EngineNginx
	}
	return agent.NormalizeProxyEngine(row.ProxyEngine)
}

// proxyFileMap returns the proxy engine files as shown in diffs and
// downloads: nginx files keep their paths, Relay Edge files get "edge/".
func proxyFileMap(engine string, files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for p, c := range files {
		if engine == agent.EngineEdge {
			p = "edge/" + p
		}
		out[p] = c
	}
	return out
}

// switchTargets lists enabled hosts with a usable route: when the proxy
// engine changes every one of them can regress, not only edited hosts.
func switchTargets(cur *model.Snapshot) []string {
	var out []string
	for _, h := range cur.Hosts {
		if !h.Enabled || len(h.Domains) == 0 || (h.Upstream.Host == "" && h.Upstream.BackendID == "") {
			continue
		}
		out = append(out, h.ID)
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// liveProxyFiles loads the live version's proxy engine files.
func (s *Service) liveProxyFiles(ctx context.Context, id int64) (agent.Files, string, error) {
	row, err := s.app.Store.GetVersion(ctx, id, true)
	if err != nil {
		return nil, "", err
	}
	var files agent.Files
	if err := json.Unmarshal([]byte(row.NginxFiles), &files); err != nil {
		return nil, "", fmt.Errorf("v%d files: %w", id, err)
	}
	return files, row.NginxHash, nil
}

// switching reports the proxy engine an apply is switching to ("" = none).
func (s *Service) switching() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchTo
}

func (s *Service) setSwitching(engine string) {
	s.mu.Lock()
	s.switchTo = engine
	s.mu.Unlock()
}

// proxySwap tracks what an apply changed on the proxy engines so a failure
// can undo it.
type proxySwap struct {
	engine  string      // proxy engine of the new version
	files   agent.Files // its files
	hash    string
	from    string // previous proxy engine when switching engines ("" otherwise)
	oldStop bool   // the previous engine was asked to stop
	started bool   // the new engine was asked to apply its release (switch)
	swapped bool   // the new release is active on engine

	lbEngine    string      // load balancer engine of the new version
	lbFiles     agent.Files // its release
	lbHash      string
	lbChanged   bool        // the load balancer engine was changed (same engine)
	lbFrom      string      // previous load balancer engine when switching ("" otherwise)
	lbFromFiles agent.Files // its live release (nil before the first apply)
	lbFromHash  string
	lbFromRun   bool // it ran the live release
	lbOldStop   bool // the previous load balancer engine was asked to stop
	lbStarted   bool // the new load balancer engine was asked to apply its release

	tunnelChanged bool // the tunnel engine got the new release
}

func engineLabel(engine string) string { return core.ProxyEngineLabel(engine) }
