// Package lb implements the load balancer slice: runtime stats, server admin
// states, the Expose wizard and the related REST API. It talks to the active
// load balancer engine (HAProxy or Relay Balancer); both answer the same
// runtime API subset (show stat, show info, set server …) in HAProxy's format.
package lb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

type Service struct {
	app *core.App
	smp *sampler

	mu     sync.Mutex
	root   context.Context
	drains map[string]context.CancelFunc // "backendID/serverID" → drain watcher
}

func New(app *core.App) *Service {
	s := &Service{app: app, drains: map[string]context.CancelFunc{}, root: context.Background()}
	s.smp = newSampler(s)
	return s
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	s.root = ctx
	s.mu.Unlock()
	go s.smp.run(ctx)
	go s.watchApply(ctx)
	return nil
}

func (s *Service) rootCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// watchApply re-applies persisted drain/maint states after every apply.
func (s *Service) watchApply(ctx context.Context) {
	ch, cancel := s.app.Bus.Subscribe(32)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Topic != events.ApplyFinished {
				continue
			}
			go func() {
				select {
				case <-ctx.Done():
				case <-time.After(1500 * time.Millisecond):
					s.reapplyStates(ctx, "apply finished")
				}
			}()
		}
	}
}

// runtime runs one command on the active load balancer engine's runtime API
// and returns its output and the engine name.
func (s *Service) runtime(ctx context.Context, command string) (string, string, error) {
	c, engine := s.app.LBClient(ctx)
	if c == nil {
		return "", engine, agent.ErrUnavailable{Err: errors.New("agent client not configured")}
	}
	out, err := c.Runtime(ctx, command)
	return out, engine, err
}

// reapplyStates sets persisted drain/maint admin states on the running load
// balancer (reloads start every server in the state rendered in the config:
// maint is rendered as disabled, drain has no config keyword).
func (s *Service) reapplyStates(ctx context.Context, reason string) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := s.runtime(cctx, "show stat")
	if err != nil {
		return
	}
	ls, err := parseLive(out, time.Now())
	if err != nil {
		return
	}
	backends, err := s.app.Store.Backends().List(cctx)
	if err != nil {
		return
	}
	n := 0
	for bi := range backends {
		b := &backends[bi]
		live := map[string]string{}
		for _, sv := range ls.Servers[b.Name] {
			live[sv.Server] = sv.Status
		}
		for i, srv := range b.Servers {
			name := serverName(b, i)
			st, ok := live[name]
			if !ok {
				continue
			}
			var want string
			switch {
			case srv.State == model.ServerStateDrain && st != "DRAIN" && st != "MAINT":
				want = "drain"
			case srv.State == model.ServerStateMaint && st != "MAINT":
				want = "maint"
			default:
				continue
			}
			if res, _, err := s.runtime(cctx, fmt.Sprintf("set server %s/%s state %s", b.Name, name, want)); err == nil && strings.TrimSpace(res) == "" {
				n++
			}
		}
	}
	if n > 0 {
		s.app.Log.Info("lb: re-applied server admin states", "servers", n, "reason", reason)
	}
}

// ---------------------------------------------------------------- stats

func (s *Service) Stats(ctx context.Context) (*core.LBStats, error) {
	ls, _ := s.smp.snapshot(3 * time.Second)
	if ls == nil {
		out, _, err := s.runtime(ctx, "show stat")
		if err != nil {
			return &core.LBStats{Backends: []core.BackendStats{}, Frontends: []core.FrontendStats{}}, nil
		}
		if ls, err = parseLive(out, time.Now()); err != nil {
			return nil, err
		}
	}
	backends, err := s.app.Store.Backends().List(ctx)
	if err != nil {
		return nil, err
	}
	frontends, err := s.app.Store.Frontends().List(ctx)
	if err != nil {
		return nil, err
	}
	st := buildStats(ls, backends, frontends, s.smp.p95)
	now := time.Now().Unix()
	hour, err := s.minutes(ctx, totalKey, now-3600, now)
	if err != nil {
		return nil, err
	}
	for _, r := range hour {
		st.ConnErrors += r.Econ
		st.RespErrors += r.Eresp
		st.ReqErrors += r.Ereq
		st.Retries += r.Wretr
		st.Redispatches += r.Wredis
	}
	day, err := s.minutes(ctx, totalKey, now-86400, now)
	if err != nil {
		return nil, err
	}
	st.PeakSessRate = st.SessRate
	for _, r := range day {
		if r.SessMax > st.PeakSessRate {
			st.PeakSessRate = r.SessMax
		}
	}
	return st, nil
}

// minutes merges stored minute aggregates with the not-yet-flushed ones.
func (s *Service) minutes(ctx context.Context, name string, since, until int64) ([]store.LBMinute, error) {
	rows, err := s.app.Store.LBMinutes(ctx, name, since-since%60, until+60)
	if err != nil {
		return nil, err
	}
	byAt := map[int64]int{}
	for i, r := range rows {
		byAt[r.At] = i
	}
	for _, r := range s.smp.unflushed(name) {
		if r.At+60 <= since {
			continue
		}
		if i, ok := byAt[r.At]; ok {
			rows[i] = r
		} else {
			byAt[r.At] = len(rows)
			rows = append(rows, r)
		}
	}
	return rows, nil
}

type SeriesPoint struct {
	T        int64 `json:"t"`
	SessRate int64 `json:"sessRate"`
	Errors   int64 `json:"errors"`
	Queue    int64 `json:"queue"`
}

type Series struct {
	Range        string        `json:"range"`
	Step         int64         `json:"step"`
	BackendID    string        `json:"backendId,omitempty"`
	Points       []SeriesPoint `json:"points"`
	Peak         int64         `json:"peak"`
	ConnErrors   int64         `json:"connErrors"`
	RespErrors   int64         `json:"respErrors"`
	ReqErrors    int64         `json:"reqErrors"`
	Retries      int64         `json:"retries"`
	Redispatches int64         `json:"redispatches"`
}

var seriesRanges = map[string][2]int64{ // range → {seconds, step}
	"1h":  {3600, 30},
	"24h": {86400, 900},
	"7d":  {7 * 86400, 7200},
}

// Series returns sessions/s, errors and queue over a range for one backend
// (backendName "" = all backends).
func (s *Service) Series(ctx context.Context, backendName, rng string) (*Series, error) {
	spec, ok := seriesRanges[rng]
	if !ok {
		return nil, httpx.Errorf(400, "bad_request", "range must be 1h, 24h or 7d")
	}
	span, step := spec[0], spec[1]
	now := time.Now().Unix()
	end := now - now%step + step
	start := end - span
	rows, err := s.minutes(ctx, backendName, start, end)
	if err != nil {
		return nil, err
	}
	mem := s.smp.memPoints(backendName, start)
	out := &Series{Range: rng, Step: step, Points: make([]SeriesPoint, 0, span/step)}
	for bs := start; bs < end; bs += step {
		be := bs + step
		p := SeriesPoint{T: bs}
		var n int64
		var sum int64
		for _, m := range mem {
			if m.At >= bs && m.At < be {
				n++
				sum += m.SessRate
				p.Errors += m.Errors
				if m.Queue > p.Queue {
					p.Queue = m.Queue
				}
			}
		}
		if n > 0 {
			p.SessRate = sum / n
		} else {
			var wsum, wtot float64
			for _, r := range rows {
				lo, hi := max(r.At, bs), min(r.At+60, be)
				if hi <= lo {
					continue
				}
				w := float64(hi - lo)
				wsum += float64(r.SessAvg) * w
				wtot += w
				p.Errors += int64(float64(r.Econ+r.Eresp) * w / 60)
				if r.QueueMax > p.Queue {
					p.Queue = r.QueueMax
				}
			}
			if wtot > 0 {
				p.SessRate = int64(wsum / wtot)
			}
		}
		out.Points = append(out.Points, p)
	}
	for _, r := range rows {
		if r.At+60 <= start {
			continue
		}
		if r.SessMax > out.Peak {
			out.Peak = r.SessMax
		}
		out.ConnErrors += r.Econ
		out.RespErrors += r.Eresp
		out.ReqErrors += r.Ereq
		out.Retries += r.Wretr
		out.Redispatches += r.Wredis
	}
	return out, nil
}

// ---------------------------------------------------------------- admin state

// SetServerState changes a server's admin state at runtime and persists it.
func (s *Service) SetServerState(ctx context.Context, backendID, serverID, state string) error {
	s.cancelDrain(backendID + "/" + serverID)
	_, err := s.setState(ctx, backendID, serverID, state, "")
	return err
}

// StateResult reports what SetServerStateGrace did.
type StateResult struct {
	State   string `json:"state"`
	Runtime bool   `json:"runtime"`        // applied to the running load balancer
	Note    string `json:"note,omitempty"` // why not applied at runtime
}

// SetServerStateGrace is SetServerState with an optional drain grace period:
// after graceSeconds (or once the server has no sessions) it switches to maint.
func (s *Service) SetServerStateGrace(ctx context.Context, backendID, serverID, state string, graceSeconds int) (*StateResult, error) {
	key := backendID + "/" + serverID
	s.cancelDrain(key)
	res, err := s.setState(ctx, backendID, serverID, state, "")
	if err != nil {
		return nil, err
	}
	if state == model.ServerStateDrain && graceSeconds > 0 && res.Runtime {
		wctx, cancel := context.WithCancel(s.rootCtx())
		actor := core.ActorFrom(ctx)
		s.mu.Lock()
		s.drains[key] = cancel
		s.mu.Unlock()
		go s.watchDrain(core.WithActor(wctx, actor), backendID, serverID, time.Duration(graceSeconds)*time.Second)
	}
	return res, nil
}

func (s *Service) cancelDrain(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.drains[key]; ok {
		c()
		delete(s.drains, key)
	}
}

func (s *Service) watchDrain(ctx context.Context, backendID, serverID string, grace time.Duration) {
	key := backendID + "/" + serverID
	deadline := time.Now().Add(grace)
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b, err := s.app.Store.Backends().Get(ctx, backendID)
		if err != nil {
			return
		}
		i := serverIndex(b, serverID)
		if i < 0 || b.Servers[i].State != model.ServerStateDrain {
			return
		}
		name := serverName(b, i)
		idle := false
		if ls, _ := s.smp.snapshot(5 * time.Second); ls != nil {
			for _, sv := range ls.Servers[b.Name] {
				if sv.Server == name && sv.Scur == 0 {
					idle = true
				}
			}
		}
		timedOut := time.Now().After(deadline)
		if !idle && !timedOut {
			continue
		}
		reason := "drain finished · no active sessions"
		if !idle {
			reason = "drain grace timeout elapsed"
		}
		s.mu.Lock()
		delete(s.drains, key)
		s.mu.Unlock()
		if _, err := s.setState(ctx, backendID, serverID, model.ServerStateMaint, reason); err != nil {
			s.app.Log.Warn("lb: drain → maint", "backend", b.Name, "server", name, "err", err)
		}
		return
	}
}

func (s *Service) setState(ctx context.Context, backendID, serverID, state, reason string) (*StateResult, error) {
	switch state {
	case model.ServerStateReady, model.ServerStateDrain, model.ServerStateMaint:
	default:
		return nil, &model.ValidationError{Fields: map[string]string{"state": "State must be ready, drain or maint"}}
	}
	b, err := s.app.Store.Backends().Get(ctx, backendID)
	if err != nil {
		return nil, err
	}
	i := serverIndex(b, serverID)
	if i < 0 {
		return nil, store.ErrNotFound
	}
	name := serverName(b, i)
	res := &StateResult{State: state}
	if err := s.runtimeServerCmd(ctx, b.Name, name, "state "+state, res); err != nil {
		return nil, err
	}
	srv := &b.Servers[i]
	if srv.State != state {
		srv.State = state
		if err := s.app.Store.Backends().Update(ctx, b); err != nil {
			return nil, err
		}
		s.app.Changed(ctx, model.KindBackend, b.ID, b.Name, core.ActionUpdated)
	}
	addr := fmt.Sprintf("%s:%d", srv.Address, srv.Port)
	detail := addr
	if reason != "" {
		detail += " · " + reason
	}
	if !res.Runtime && res.Note != "" {
		detail += " · " + res.Note
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "server." + state, Target: b.Name + "/" + name, Detail: detail})
	verb := map[string]string{model.ServerStateReady: "is back in rotation", model.ServerStateDrain: "set to drain", model.ServerStateMaint: "in maintenance"}[state]
	level := "info"
	if state == model.ServerStateReady {
		level = "ok"
	}
	s.app.Activity(ctx, "server."+state, level, fmt.Sprintf("Server %s %s", addr, verb), b.Name, detail)
	s.app.Bus.Publish(events.HealthChanged, map[string]any{"target": "server:" + b.Name + "/" + name, "status": state})
	return res, nil
}

// SetServerWeight changes a server's weight at runtime and persists it.
func (s *Service) SetServerWeight(ctx context.Context, backendID, serverID string, weight int) (*StateResult, error) {
	if weight < 1 || weight > 256 {
		return nil, &model.ValidationError{Fields: map[string]string{"weight": "Weight must be 1–256"}}
	}
	b, err := s.app.Store.Backends().Get(ctx, backendID)
	if err != nil {
		return nil, err
	}
	i := serverIndex(b, serverID)
	if i < 0 {
		return nil, store.ErrNotFound
	}
	name := serverName(b, i)
	res := &StateResult{State: b.Servers[i].State}
	if err := s.runtimeServerCmd(ctx, b.Name, name, fmt.Sprintf("weight %d", weight), res); err != nil {
		return nil, err
	}
	prev := b.Servers[i].Weight
	if prev != weight {
		b.Servers[i].Weight = weight
		if err := s.app.Store.Backends().Update(ctx, b); err != nil {
			return nil, err
		}
		s.app.Changed(ctx, model.KindBackend, b.ID, b.Name, core.ActionUpdated)
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "server.weight", Target: b.Name + "/" + name, Detail: fmt.Sprintf("weight %d → %d", prev, weight)})
	return res, nil
}

// runtimeServerCmd runs `set server <backend>/<server> <args>`. A missing
// agent / load balancer or a server that is not in the live config yet is not
// an error: the change is persisted and takes effect on the next apply.
func (s *Service) runtimeServerCmd(ctx context.Context, backend, server, args string, res *StateResult) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, engine, err := s.runtime(cctx, fmt.Sprintf("set server %s/%s %s", backend, server, args))
	label := core.ProxyEngineLabel(engine)
	if err != nil {
		var ua agent.ErrUnavailable
		if errors.As(err, &ua) {
			res.Note = label + " is not reachable; takes effect on the next apply"
		} else {
			res.Note = label + " is not running; takes effect on the next apply"
			s.app.Log.Debug("lb: runtime command", "err", err)
		}
		return nil
	}
	out = strings.TrimSpace(out)
	switch {
	case out == "":
		res.Runtime = true
	case strings.Contains(out, "No such"):
		res.Note = "not in the live config yet; takes effect on the next apply"
	default:
		return httpx.Errorf(502, "runtime_error", label+": "+out)
	}
	return nil
}
