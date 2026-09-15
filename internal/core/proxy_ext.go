package core

// Proxy engine selection (slice: engine). Exactly one of nginx and Relay Edge
// serves the HTTP/HTTPS ports and streams; see docs/EDGE.md. Likewise exactly
// one of HAProxy and Relay Balancer runs the load balancer; see docs/BALANCER.md.

import (
	"context"
	"errors"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const proxyEngineTTL = 3 * time.Second

// ProxyEngineLabel returns the display name of an engine (any engine, not
// only proxy engines).
func ProxyEngineLabel(engine string) string {
	switch engine {
	case agent.EngineEdge:
		return "Relay Edge"
	case agent.EngineHAProxy:
		return "HAProxy"
	case agent.EngineBalancer:
		return "Relay Balancer"
	}
	return "nginx"
}

// ProxyEngine returns the active proxy engine: the engine of the live config
// version, or the selected one in General settings before the first apply.
// The result is cached for a few seconds.
func (a *App) ProxyEngine(ctx context.Context) string {
	a.proxyMu.Lock()
	if a.proxyEngine != "" && time.Since(a.proxyAt) < proxyEngineTTL {
		e := a.proxyEngine
		a.proxyMu.Unlock()
		return e
	}
	a.proxyMu.Unlock()
	e := agent.EngineNginx
	if a.Store != nil {
		row, err := a.Store.LiveVersion(ctx, false)
		switch {
		case err == nil:
			e = agent.NormalizeProxyEngine(row.ProxyEngine)
		case errors.Is(err, store.ErrNotFound):
			if g, gerr := store.LoadSettings[model.GeneralSettings](ctx, a.Store, model.SettingsGeneral); gerr == nil {
				e = agent.NormalizeProxyEngine(g.ProxyEngine)
			}
		default:
			a.proxyMu.Lock()
			cached := a.proxyEngine
			a.proxyMu.Unlock()
			if cached != "" {
				return cached
			}
		}
	}
	a.proxyMu.Lock()
	a.proxyEngine, a.proxyAt = e, time.Now()
	a.proxyMu.Unlock()
	return e
}

// Proxy returns the agent client and name of the active proxy engine.
func (a *App) Proxy(ctx context.Context) (*agent.Client, string) {
	e := a.ProxyEngine(ctx)
	return a.Client(e), e
}

// Client returns the agent client for an engine name (nil when unknown).
func (a *App) Client(engine string) *agent.Client {
	switch engine {
	case agent.EngineNginx:
		return a.Nginx
	case agent.EngineHAProxy:
		return a.HAProxy
	case agent.EngineEdge:
		return a.Edge
	case agent.EngineBalancer:
		return a.Balancer
	}
	return nil
}

// SetProxyEngine records a new active proxy engine (after an apply) and
// writes the selection file read by the engine agents.
func (a *App) SetProxyEngine(engine string) {
	engine = agent.NormalizeProxyEngine(engine)
	a.proxyMu.Lock()
	a.proxyEngine, a.proxyAt = engine, time.Now()
	a.proxyMu.Unlock()
	a.writeProxyEngineFile(engine)
}

// SyncProxyEngineFile writes the active proxy engine to the selection file.
func (a *App) SyncProxyEngineFile(ctx context.Context) {
	a.writeProxyEngineFile(a.ProxyEngine(ctx))
}

func (a *App) writeProxyEngineFile(engine string) {
	if a.Config.RunDir == "" {
		return
	}
	a.proxyMu.Lock()
	same := a.proxyFile == engine
	a.proxyMu.Unlock()
	if same {
		return
	}
	if err := agent.WriteProxyEngine(a.Config.RunDir, engine); err != nil {
		if a.Log != nil {
			a.Log.Warn("write proxy engine selection", "engine", engine, "err", err)
		}
		return
	}
	a.proxyMu.Lock()
	a.proxyFile = engine
	a.proxyMu.Unlock()
}

// LBEngine returns the active load balancer engine (haproxy | balancer): the
// engine of the live config version, or the one selected in General settings
// before the first apply. The result is cached for a few seconds.
func (a *App) LBEngine(ctx context.Context) string {
	a.lbMu.Lock()
	if a.lbEngine != "" && time.Since(a.lbAt) < proxyEngineTTL {
		e := a.lbEngine
		a.lbMu.Unlock()
		return e
	}
	a.lbMu.Unlock()
	e := agent.EngineHAProxy
	if a.Store != nil {
		row, err := a.Store.LiveVersion(ctx, false)
		switch {
		case err == nil:
			e = agent.NormalizeLBEngine(row.LBEngine)
		case errors.Is(err, store.ErrNotFound):
			if g, gerr := store.LoadSettings[model.GeneralSettings](ctx, a.Store, model.SettingsGeneral); gerr == nil {
				e = agent.NormalizeLBEngine(g.LBEngine)
			}
		default:
			a.lbMu.Lock()
			cached := a.lbEngine
			a.lbMu.Unlock()
			if cached != "" {
				return cached
			}
		}
	}
	a.lbMu.Lock()
	a.lbEngine, a.lbAt = e, time.Now()
	a.lbMu.Unlock()
	return e
}

// SetLBEngine records a new active load balancer engine (after an apply).
func (a *App) SetLBEngine(engine string) {
	engine = agent.NormalizeLBEngine(engine)
	a.lbMu.Lock()
	a.lbEngine, a.lbAt = engine, time.Now()
	a.lbMu.Unlock()
}

// LBClient returns the agent client and name of the active load balancer
// engine. Stats, server admin states and weights, and engine logs go through
// it: both engines answer the same runtime API subset.
func (a *App) LBClient(ctx context.Context) (*agent.Client, string) {
	e := a.LBEngine(ctx)
	return a.Client(e), e
}
