// Package logs implements Relay's observability backend (slice observe):
// nginx log ingestion, load balancer (HAProxy / Relay Balancer) engine log polling, per-minute traffic metrics,
// retention, and the REST endpoints for logs, audit, activity, metrics,
// health and the global blocklist.
package logs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render/nginx"
	"github.com/instantoffr/relay/internal/store"
)

const (
	tailInterval       = 250 * time.Millisecond
	haproxyPollEvery   = 5 * time.Second
	kvTailPrefix       = "observe.tail."
	retentionFirstRun  = time.Minute
	retentionInterval  = time.Hour
	accessRetention    = 7 * 24 * time.Hour
	accessMaxRows      = 2_000_000
	errorRetention     = 30 * 24 * time.Hour
	metricsRetention   = 30 * 24 * time.Hour
	activityRetention  = 90 * 24 * time.Hour
	maxPageLimit       = 500
	defaultPageLimit   = 100
	exportMaxRows      = 250_000
	exportPageRows     = 1000
	sameClientWindow   = 10 * time.Minute
	sameClientMaxRows  = 25
	streamClientWindow = 5 * time.Minute
	streamBytesWindow  = time.Minute
)

type Service struct {
	app   *core.App
	names *nameCache
	ing   *ingester
}

func New(app *core.App) *Service { return &Service{app: app, names: newNameCache()} }

func (s *Service) Start(ctx context.Context) error {
	s.ing = newIngester(s.app, s.names)
	go s.ing.run(ctx)
	go s.watchConfig(ctx)
	if dir := s.app.Config.LogDir; dir != "" {
		go s.tail(ctx, filepath.Join(dir, nginx.AccessLogFile), srcAccess)
		go s.tail(ctx, filepath.Join(dir, nginx.StreamAccessLogFile), srcStream)
		go s.tail(ctx, filepath.Join(dir, nginx.ErrorLogFile), srcNginxError)
	}
	if s.app.HAProxy != nil || s.app.Balancer != nil {
		go s.pollLB(ctx)
	}
	go s.retentionLoop(ctx)
	return nil
}

func (s *Service) QueryAccess(ctx context.Context, q core.AccessQuery) (*core.AccessPage, error) {
	return queryAccess(ctx, s.app.Store, q)
}

func (s *Service) tail(ctx context.Context, path string, src source) {
	key := kvTailPrefix + filepath.Base(path)
	var saved *tailState
	if b, err := s.app.Store.GetKV(ctx, key); err == nil {
		var st tailState
		if json.Unmarshal(b, &st) == nil {
			saved = &st
		}
	}
	t := newTailer(path, saved, func(lines [][]byte, st tailState) {
		val, _ := json.Marshal(st)
		select {
		case s.ing.in <- chunk{src: src, lines: lines, kvKey: key, kvVal: val}:
		case <-ctx.Done():
		}
	})
	defer t.close()
	tick := time.NewTicker(tailInterval)
	defer tick.Stop()
	lastErr := ""
	for {
		if err := t.poll(); err != nil {
			if err.Error() != lastErr {
				s.app.Log.Warn("observe: tail log file", "path", path, "err", err)
				lastErr = err.Error()
			}
		} else {
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pollLB copies the active load balancer engine's output (HAProxy alerts,
// warnings and notices, or Relay Balancer's HAProxy-format lines) into
// error_log with the engine as the source. Each engine has its own cursor,
// committed with the rows so restarts don't duplicate.
func (s *Service) pollLB(ctx context.Context) {
	cursors := map[string]time.Time{}
	tick := time.NewTicker(haproxyPollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.pollLBOnce(ctx, cursors)
	}
}

// kvLBCursor is the poll cursor of a load balancer engine
// ("observe.haproxy.since", "observe.balancer.since").
func kvLBCursor(engine string) string { return "observe." + engine + ".since" }

// pollLBOnce fetches new output of the active load balancer engine and hands
// it to the ingester. cursors holds the per-engine positions.
func (s *Service) pollLBOnce(ctx context.Context, cursors map[string]time.Time) {
	c, engine := s.app.LBClient(ctx)
	if c == nil {
		return
	}
	since, ok := cursors[engine]
	if !ok {
		since = time.Now()
		if b, err := s.app.Store.GetKV(ctx, kvLBCursor(engine)); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, string(b)); err == nil {
				since = t
			}
		}
		cursors[engine] = since
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	resp, err := c.Logs(cctx, since, 1000)
	cancel()
	if err != nil || resp == nil {
		return // agent not running (dev), restarting or its container stopped
	}
	last := since
	var recs []store.ErrorRecord
	for _, l := range resp.Lines {
		if !l.At.After(since) {
			continue
		}
		if l.At.After(last) {
			last = l.At
		}
		if rec, ok := ParseHAProxyLine(l); ok {
			rec.Source = engine
			recs = append(recs, rec)
		}
	}
	if !last.After(since) {
		return
	}
	cursors[engine] = last
	select {
	case s.ing.in <- chunk{src: srcHAProxy, errs: recs, kvKey: kvLBCursor(engine), kvVal: []byte(last.UTC().Format(time.RFC3339Nano))}:
	case <-ctx.Done():
	}
}

func (s *Service) watchConfig(ctx context.Context) {
	s.names.refresh(ctx, s.app.Store)
	ch, cancel := s.app.Bus.Subscribe(64)
	defer cancel()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.names.refresh(ctx, s.app.Store)
		case ev, ok := <-ch:
			if !ok {
				return
			}
			switch ev.Topic {
			case events.ConfigChanged:
				if c, ok := ev.Data.(core.ConfigChange); ok && c.Kind == model.KindStream {
					s.names.refresh(ctx, s.app.Store)
				}
			case events.ApplyFinished:
				s.names.refresh(ctx, s.app.Store)
			}
		}
	}
}

func (s *Service) retentionLoop(ctx context.Context) {
	timer := time.NewTimer(retentionFirstRun)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		removed, err := s.app.Store.PruneObserve(ctx, time.Now(), store.RetentionPolicy{
			AccessMaxAge:   accessRetention,
			AccessMaxRows:  accessMaxRows,
			ErrorMaxAge:    errorRetention,
			MetricsMaxAge:  metricsRetention,
			ActivityMaxAge: activityRetention,
		})
		if err != nil && ctx.Err() == nil {
			s.app.Log.Warn("observe: retention", "err", err)
		} else if len(removed) > 0 {
			s.app.Log.Info("observe: retention pruned rows", "removed", removed)
		}
		timer.Reset(retentionInterval)
	}
}
