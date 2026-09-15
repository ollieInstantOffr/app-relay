package apply

// Load balancer engine selection. Exactly one of HAProxy (default) and Relay
// Balancer runs the backends and frontends; both bind the same frontends
// (the localhost frontends proxy hosts and streams route through included).
// Switching is a pending change: the apply validates the new engine, stops
// the old one, starts the new one, health-checks and rolls back on failure.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/lb/lbengine"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// snapshotLBEngine is the load balancer engine a snapshot selects.
func snapshotLBEngine(snap *model.Snapshot) string {
	if snap == nil {
		return agent.EngineHAProxy
	}
	return agent.NormalizeLBEngine(snap.General.LBEngine)
}

// rowLBEngine is the load balancer engine a stored version was applied with.
func rowLBEngine(row *store.VersionRow) string {
	if row == nil {
		return agent.EngineHAProxy
	}
	return agent.NormalizeLBEngine(row.LBEngine)
}

// lbRendererFor returns the renderer of a load balancer engine.
func lbRendererFor(engine string) (lbengine.Renderer, error) { return lbengine.For(engine) }

// lbFileMap returns a version's load balancer file as shown in diffs
// (haproxy.cfg or balancer/balancer.json); empty content yields no file.
func lbFileMap(engine, content string) map[string]string {
	if content == "" {
		return map[string]string{}
	}
	return map[string]string{lbengine.DiffPath(engine): content}
}

// liveLBFiles loads the live version's load balancer release.
func (s *Service) liveLBFiles(ctx context.Context, id int64) (agent.Files, string, error) {
	row, err := s.app.Store.GetVersion(ctx, id, true)
	if err != nil {
		return nil, "", err
	}
	if row.HAProxyHash == "" {
		return nil, "", nil
	}
	return lbengine.Files(rowLBEngine(row), row.HAProxyCfg), row.HAProxyHash, nil
}

// lbSwitching reports the load balancer engine an apply is switching to.
func (s *Service) lbSwitching() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lbSwitchTo
}

func (s *Service) setLBSwitching(engine string) {
	s.mu.Lock()
	s.lbSwitchTo = engine
	s.mu.Unlock()
}

// anySwitching reports whether an apply is switching the proxy or the load
// balancer engine (reconcile, container management and outage alerts wait).
func (s *Service) anySwitching() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchTo != "" || s.lbSwitchTo != ""
}

// lbSwitchTargets lists enabled hosts routed through a backend: when the load
// balancer engine changes every one of them can regress.
func lbSwitchTargets(cur *model.Snapshot) []string {
	var out []string
	for _, h := range cur.Hosts {
		if h.Enabled && len(h.Domains) > 0 && h.Upstream.BackendID != "" {
			out = append(out, h.ID)
		}
	}
	return out
}

// mergeTargets joins host id lists (sorted, unique, at most 20).
func mergeTargets(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, id := range l {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// frontendTargets lists the enabled frontends of a snapshot (streams and
// backend-routed hosts reach their backends through them), at most 20.
func frontendTargets(snap *model.Snapshot) []model.Frontend {
	if snap == nil {
		return nil
	}
	var out []model.Frontend
	for _, f := range snap.Frontends {
		if f.Enabled && f.Bind != "" {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// watchedFrontends returns the frontends of cur that accepted connections
// before the switch (same id and bind in live).
func (s *Service) watchedFrontends(ctx context.Context, live, cur *model.Snapshot) []model.Frontend {
	liveBind := map[string]string{}
	for _, f := range frontendTargets(live) {
		liveBind[f.ID] = f.Bind
	}
	var candidates []model.Frontend
	for _, f := range frontendTargets(cur) {
		if liveBind[f.ID] == f.Bind {
			candidates = append(candidates, f)
		}
	}
	var out []model.Frontend
	for _, f := range candidates {
		if probeFrontend(ctx, f.Bind).ok {
			out = append(out, f)
		}
	}
	return out
}

// probeFrontend opens a TCP connection to a frontend's bind address.
func probeFrontend(ctx context.Context, bind string) probeResult {
	addr, port, err := model.SplitBind(bind)
	if err != nil {
		return probeResult{ok: false, detail: "has an invalid bind address"}
	}
	switch addr {
	case "", "*", "0.0.0.0", "::", "[::]":
		addr = "127.0.0.1"
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", net.JoinHostPort(addr, fmt.Sprint(port)))
	if err != nil {
		return probeResult{ok: false, detail: "refused connections"}
	}
	conn.Close()
	return probeResult{ok: true, detail: "accepted connections"}
}

// lbWatch is what the health check watches after a load balancer switch.
type lbWatch struct {
	engine string
	run    bool // the new engine must be running
	fronts []model.Frontend
	blame  bool // host failures are the load balancer's (the proxy engine didn't switch)
	// before maps backend names to whether they had a usable server on the
	// previous engine (nil when it couldn't be read).
	before map[string]bool
}
