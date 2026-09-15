package lb

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	sampleEvery  = 2 * time.Second
	memoryWindow = time.Hour
	retention    = 7 * 24 * time.Hour
	reloadQuiet  = 20 * time.Second // ignore state flaps right after a reload
	totalKey     = ""               // aggregate of all backends
)

type point struct {
	At       int64 // unix seconds
	SessRate int64
	Errors   int64 // conn + resp errors during the interval
	Queue    int64
}

type rtSample struct {
	At    int64
	Rtime int64
}

type counters struct{ Econ, Eresp, Ereq, Wretr, Wredis int64 }

type minuteAcc struct {
	SessSum, N, SessMax, QueueMax int64
	counters
}

// sampler polls the runtime API, keeps an in-memory hour of 2 s points,
// persists per-minute aggregates and detects server state transitions.
type sampler struct {
	svc *Service

	mu         sync.Mutex
	last       *liveStats
	lastErr    error
	prev       map[string]counters // cumulative counters at previous sample, by backend ("" = frontends ereq)
	points     map[string][]point
	rt         map[string][]rtSample // "backend" or "backend/server"
	minute     int64
	acc        map[string]*minuteAcc
	status     map[string]string // "backend/server" → status class
	pid        string
	engine     string // load balancer engine of the last sample
	quietUntil time.Time
	lastPrune  time.Time
	queue      []store.LBMinute // completed minutes waiting to be written
}

func newSampler(svc *Service) *sampler {
	return &sampler{
		svc: svc, prev: map[string]counters{}, points: map[string][]point{}, rt: map[string][]rtSample{},
		acc: map[string]*minuteAcc{}, status: map[string]string{},
	}
}

func (s *sampler) run(ctx context.Context) {
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			s.flush(context.WithoutCancel(ctx), true)
			return
		case <-t.C:
		}
	}
}

func (s *sampler) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	now := time.Now()
	out, engine, err := s.svc.runtime(cctx, "show stat")
	var ls *liveStats
	if err == nil {
		ls, err = parseLive(out, now)
	}
	if err != nil {
		s.mu.Lock()
		s.last, s.lastErr = nil, err
		s.mu.Unlock()
		s.flush(ctx, false)
		return
	}
	pid := ""
	if info, _, ierr := s.svc.runtime(cctx, "show info"); ierr == nil {
		pid = parseInfo(info)["Pid"]
	}

	s.mu.Lock()
	// A new process (reload, or a switch to the other load balancer engine).
	reloaded := pid != "" && (pid != s.pid || engine != s.engine)
	firstSight := s.pid == ""
	if reloaded {
		s.pid, s.engine = pid, engine
		if !firstSight {
			s.quietUntil = now.Add(reloadQuiet)
		}
	}
	s.last, s.lastErr = ls, nil
	s.record(ls)
	transitions := s.detectTransitions(ls, now, firstSight)
	s.mu.Unlock()

	if reloaded {
		go s.svc.reapplyStates(context.WithoutCancel(ctx), engine+" process started")
	}
	for _, tr := range transitions {
		s.svc.announce(ctx, tr)
	}
	s.flush(ctx, false)
}

func delta(cur, prev int64) int64 {
	if cur >= prev {
		return cur - prev
	}
	return cur // counters reset (reload)
}

// record must be called with mu held.
func (s *sampler) record(ls *liveStats) {
	at := ls.At.Unix()
	cutoff := at - int64(memoryWindow/time.Second)
	minute := at - at%60
	if s.minute != 0 && minute != s.minute {
		s.pendingFlush()
	}
	s.minute = minute

	total := point{At: at}
	var totalC counters
	seen := map[string]bool{}
	for _, b := range ls.Backends {
		seen[b.Name] = true
		cur := counters{Econ: b.Econ, Eresp: b.Eresp, Wretr: b.Wretr, Wredis: b.Wredis}
		p, had := s.prev[b.Name]
		d := counters{}
		if had {
			d = counters{Econ: delta(cur.Econ, p.Econ), Eresp: delta(cur.Eresp, p.Eresp), Wretr: delta(cur.Wretr, p.Wretr), Wredis: delta(cur.Wredis, p.Wredis)}
		}
		s.prev[b.Name] = cur
		pt := point{At: at, SessRate: b.Rate, Errors: d.Econ + d.Eresp, Queue: b.Qcur}
		s.points[b.Name] = appendTrim(s.points[b.Name], pt, cutoff)
		s.addAcc(b.Name, pt, d)
		total.SessRate += pt.SessRate
		total.Errors += pt.Errors
		total.Queue += pt.Queue
		totalC.Econ += d.Econ
		totalC.Eresp += d.Eresp
		totalC.Wretr += d.Wretr
		totalC.Wredis += d.Wredis

		if b.Rtime > 0 {
			s.rt[b.Name] = appendRT(s.rt[b.Name], rtSample{at, b.Rtime}, cutoff)
		}
		for _, sv := range ls.Servers[b.Name] {
			key := b.Name + "/" + sv.Server
			if sv.Rtime > 0 && statusClass(sv.Status) == "up" {
				s.rt[key] = appendRT(s.rt[key], rtSample{at, sv.Rtime}, cutoff)
			}
		}
	}
	var ereq int64
	for _, f := range ls.Frontends {
		ereq += f.Ereq
	}
	if p, had := s.prev["\x00frontends"]; had {
		totalC.Ereq = delta(ereq, p.Ereq)
	}
	s.prev["\x00frontends"] = counters{Ereq: ereq}
	s.points[totalKey] = appendTrim(s.points[totalKey], total, cutoff)
	s.addAcc(totalKey, total, totalC)

	for name := range s.points {
		if name != totalKey && !seen[name] {
			s.points[name] = trimPoints(s.points[name], cutoff)
			if len(s.points[name]) == 0 {
				delete(s.points, name)
				delete(s.prev, name)
			}
		}
	}
	for key, v := range s.rt {
		v = trimRT(v, cutoff)
		if len(v) == 0 {
			delete(s.rt, key)
		} else {
			s.rt[key] = v
		}
	}
}

func (s *sampler) addAcc(name string, pt point, d counters) {
	a := s.acc[name]
	if a == nil {
		a = &minuteAcc{}
		s.acc[name] = a
	}
	a.SessSum += pt.SessRate
	a.N++
	if pt.SessRate > a.SessMax {
		a.SessMax = pt.SessRate
	}
	if pt.Queue > a.QueueMax {
		a.QueueMax = pt.Queue
	}
	a.Econ += d.Econ
	a.Eresp += d.Eresp
	a.Ereq += d.Ereq
	a.Wretr += d.Wretr
	a.Wredis += d.Wredis
}

func (s *sampler) pendingFlush() {
	for name, a := range s.acc {
		if a.N == 0 {
			continue
		}
		s.queue = append(s.queue, store.LBMinute{
			At: s.minute, Backend: name, SessAvg: a.SessSum / a.N, SessMax: a.SessMax,
			Econ: a.Econ, Eresp: a.Eresp, Ereq: a.Ereq, Wretr: a.Wretr, Wredis: a.Wredis, QueueMax: a.QueueMax,
		})
	}
	s.acc = map[string]*minuteAcc{}
}

// flush writes queued minute rows (and the current minute when final).
func (s *sampler) flush(ctx context.Context, final bool) {
	s.mu.Lock()
	if final {
		s.pendingFlush()
	}
	rows := s.queue
	s.queue = nil
	prune := time.Since(s.lastPrune) > time.Hour
	if prune {
		s.lastPrune = time.Now()
	}
	s.mu.Unlock()
	if len(rows) > 0 {
		if err := s.svc.app.Store.InsertLBMinutes(ctx, rows); err != nil {
			s.svc.app.Log.Warn("lb: store samples", "err", err)
		}
	}
	if prune {
		if err := s.svc.app.Store.PruneLBSamples(ctx, time.Now().Add(-retention).Unix()); err != nil {
			s.svc.app.Log.Warn("lb: prune samples", "err", err)
		}
	}
}

func appendTrim(v []point, p point, cutoff int64) []point { return trimPoints(append(v, p), cutoff) }

func trimPoints(v []point, cutoff int64) []point {
	i := sort.Search(len(v), func(i int) bool { return v[i].At > cutoff })
	if i > 0 {
		v = append(v[:0:0], v[i:]...)
	}
	return v
}

func appendRT(v []rtSample, r rtSample, cutoff int64) []rtSample { return trimRT(append(v, r), cutoff) }

func trimRT(v []rtSample, cutoff int64) []rtSample {
	i := sort.Search(len(v), func(i int) bool { return v[i].At > cutoff })
	if i > 0 {
		v = append(v[:0:0], v[i:]...)
	}
	return v
}

// ---------------------------------------------------------------- transitions

type transition struct {
	Backend, Server, Address string
	From, To                 string // status classes
	Detail                   string
}

// detectTransitions must be called with mu held.
func (s *sampler) detectTransitions(ls *liveStats, now time.Time, firstSight bool) []transition {
	quiet := now.Before(s.quietUntil)
	var out []transition
	seen := map[string]bool{}
	for backend, servers := range ls.Servers {
		for _, sv := range servers {
			key := backend + "/" + sv.Server
			seen[key] = true
			cls := statusClass(sv.Status)
			if cls == "" {
				continue
			}
			prev, had := s.status[key]
			if quiet {
				// Keep the pre-reload state; compare once checks have settled.
				if !had {
					s.status[key] = cls
				}
				continue
			}
			if had && !firstSight && prev != cls && ((prev == "up" && cls == "down") || (prev == "down" && cls == "up")) {
				out = append(out, transition{Backend: backend, Server: sv.Server, Address: sv.Addr, From: prev, To: cls, Detail: checkDetail(sv)})
			}
			s.status[key] = cls
		}
	}
	if !quiet {
		for key := range s.status {
			if !seen[key] {
				delete(s.status, key)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend+out[i].Server < out[j].Backend+out[j].Server })
	return out
}

// announce publishes a server up/down transition.
func (svc *Service) announce(ctx context.Context, tr transition) {
	app := svc.app
	addr := tr.Address
	if addr == "" {
		addr = tr.Server
	}
	target := "server:" + tr.Backend + "/" + tr.Server
	if tr.To == "down" {
		title := fmt.Sprintf("Server %s is down", addr)
		detail := "backend " + tr.Backend
		if tr.Detail != "" {
			detail += " · " + tr.Detail
		}
		app.Activity(ctx, "upstream.down", "error", title, tr.Backend, detail)
		if app.Notify != nil {
			app.Notify.Notify(ctx, core.Notification{
				Event: model.EventUpstreamDown, Level: "error", Title: title,
				Message: detail + " · traffic rerouted to the remaining servers", URL: "/load-balancer/backends",
			})
		}
		app.Bus.Publish(events.HealthChanged, map[string]any{"target": target, "status": core.HealthDown, "detail": tr.Detail})
		return
	}
	title := fmt.Sprintf("Server %s is back up", addr)
	app.Activity(ctx, "upstream.up", "ok", title, tr.Backend, "backend "+tr.Backend)
	app.Bus.Publish(events.HealthChanged, map[string]any{"target": target, "status": core.HealthHealthy})
}

// ---------------------------------------------------------------- queries

func (s *sampler) snapshot(maxAge time.Duration) (*liveStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil && time.Since(s.last.At) <= maxAge {
		return s.last, nil
	}
	if s.lastErr != nil && s.last == nil {
		return nil, s.lastErr
	}
	return nil, nil
}

func (s *sampler) p95(backend, server string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := backend
	if server != "" {
		key += "/" + server
	}
	v := s.rt[key]
	vals := make([]int64, len(v))
	for i := range v {
		vals[i] = v[i].Rtime
	}
	return percentile(vals, 95)
}

// memPoints returns in-memory points for a backend name at or after since.
func (s *sampler) memPoints(name string, since int64) []point {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []point
	for _, p := range s.points[name] {
		if p.At >= since {
			out = append(out, p)
		}
	}
	return out
}

// unflushed returns minute aggregates for name that are not in the database
// yet (queued completed minutes plus the current partial minute).
func (s *sampler) unflushed(name string) []store.LBMinute {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []store.LBMinute
	for _, r := range s.queue {
		if r.Backend == name {
			rows = append(rows, r)
		}
	}
	if a := s.acc[name]; a != nil && a.N > 0 {
		rows = append(rows, store.LBMinute{
			At: s.minute, Backend: name, SessAvg: a.SessSum / a.N, SessMax: a.SessMax, QueueMax: a.QueueMax,
			Econ: a.Econ, Eresp: a.Eresp, Ereq: a.Ereq, Wretr: a.Wretr, Wredis: a.Wredis,
		})
	}
	return rows
}
