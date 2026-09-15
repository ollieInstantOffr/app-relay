package logs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

type source int

const (
	srcAccess source = iota
	srcStream
	srcNginxError
	srcHAProxy
)

// chunk is a unit of work handed from a tailer/poller to the ingester.
type chunk struct {
	src   source
	lines [][]byte
	errs  []store.ErrorRecord
	kvKey string // committed together with the rows (tail offset, poll cursor)
	kvVal []byte
}

const (
	flushInterval    = 500 * time.Millisecond
	flushRows        = 500
	maxPendingRows   = 50_000
	liveAccessPerSec = 200
	liveErrorsPerSec = 50
)

type metricKey struct {
	minute int64
	host   string
}

// ingester parses lines, batches rows and metric deltas and writes them in
// one transaction every flushInterval or flushRows, then publishes live
// events for the committed rows.
type ingester struct {
	st    *store.Store
	bus   *events.Bus
	log   *slog.Logger
	names *nameCache
	in    chan chunk
	// proxyEngine returns the active proxy engine; error.log lines are
	// recorded with it as the source (nginx | edge).
	proxyEngine func() string

	access  []store.AccessRecord
	errs    []store.ErrorRecord
	metrics map[metricKey]*store.MetricsDelta
	kv      map[string][]byte

	badLines    int
	lastBadLog  time.Time
	liveSec     int64
	liveAccess  int
	liveErrors  int
	lastFailLog time.Time
}

func newIngester(app *core.App, names *nameCache) *ingester {
	return &ingester{
		st: app.Store, bus: app.Bus, log: app.Log, names: names,
		proxyEngine: func() string { return app.ProxyEngine(context.Background()) },
		in:          make(chan chunk, 64),
		metrics:     map[metricKey]*store.MetricsDelta{},
		kv:          map[string][]byte{},
	}
}

func (g *ingester) run(ctx context.Context) {
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
		drain:
			for {
				select {
				case c := <-g.in:
					g.add(c)
				default:
					break drain
				}
			}
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			g.flush(fctx)
			cancel()
			return
		case c := <-g.in:
			g.add(c)
			if g.pending() >= flushRows {
				g.flush(ctx)
			}
		case <-tick.C:
			g.flush(ctx)
		}
	}
}

func (g *ingester) pending() int { return len(g.access) + len(g.errs) }

func (g *ingester) add(c chunk) {
	now := time.Now()
	switch c.src {
	case srcAccess:
		for _, l := range c.lines {
			r, err := ParseAccessLine(l)
			if err != nil {
				g.bad(err)
				continue
			}
			g.access = append(g.access, r)
			g.aggregate(&r)
		}
	case srcStream:
		for _, l := range c.lines {
			r, err := ParseStreamLine(l)
			if err != nil {
				g.bad(err)
				continue
			}
			r.Host = g.names.streamLabel(r.HostID, r.Extra["server_port"])
			g.access = append(g.access, r)
			g.aggregate(&r)
		}
	case srcNginxError:
		source := "nginx"
		if g.proxyEngine != nil && g.proxyEngine() == "edge" {
			source = "edge"
		}
		for _, l := range c.lines {
			rec := ParseNginxErrorLine(string(l), now)
			rec.Source = source
			g.errs = append(g.errs, rec)
		}
	case srcHAProxy:
		g.errs = append(g.errs, c.errs...)
	}
	if c.kvKey != "" {
		g.kv[c.kvKey] = c.kvVal
	}
}

func (g *ingester) bad(err error) {
	g.badLines++
	if time.Since(g.lastBadLog) > time.Minute {
		g.log.Warn("observe: skipped unparsable log lines", "count", g.badLines, "err", err)
		g.lastBadLog = time.Now()
		g.badLines = 0
	}
}

func (g *ingester) aggregate(r *store.AccessRecord) {
	minute := r.TS.Unix() / 60
	if r.Kind == "stream" {
		g.addMetric(minute, "stream:"+r.HostID, r, false)
		return
	}
	g.addMetric(minute, "", r, true)
	key := r.HostID
	if key == "" {
		key = store.MetricsKeyUnknown
	}
	g.addMetric(minute, key, r, true)
}

func (g *ingester) addMetric(minute int64, host string, r *store.AccessRecord, withHist bool) {
	k := metricKey{minute, host}
	d := g.metrics[k]
	if d == nil {
		d = &store.MetricsDelta{Minute: minute, Host: host}
		g.metrics[k] = d
	}
	d.Requests++
	switch {
	case r.Status >= 500:
		d.S5xx++
	case r.Status >= 400:
		d.S4xx++
	case r.Status >= 300:
		d.S3xx++
	case r.Status > 0:
		d.S2xx++
	}
	d.BytesOut += r.BytesSent
	d.BytesIn += r.BytesReceived
	if withHist {
		if d.Hist == nil {
			d.Hist = make([]int64, HistLen)
		}
		d.Hist[latencyBucket(r.RequestTime*1000)]++
	}
}

func (g *ingester) flush(ctx context.Context) {
	if g.pending() == 0 && len(g.metrics) == 0 && len(g.kv) == 0 {
		return
	}
	b := &store.ObserveBatch{Access: g.access, Errors: g.errs, KV: g.kv}
	for _, d := range g.metrics {
		b.Metrics = append(b.Metrics, *d)
	}
	if err := g.st.WriteObserveBatch(ctx, b); err != nil {
		if time.Since(g.lastFailLog) > 30*time.Second {
			g.log.Error("observe: write log batch", "err", err, "rows", g.pending())
			g.lastFailLog = time.Now()
		}
		if g.pending() > maxPendingRows {
			g.log.Error("observe: dropping buffered log rows after repeated write failures", "rows", g.pending())
			g.reset()
		}
		return
	}
	g.publish(b)
	g.reset()
}

func (g *ingester) reset() {
	g.access, g.errs = nil, nil
	g.metrics = map[metricKey]*store.MetricsDelta{}
	g.kv = map[string][]byte{}
}

// publish sends committed rows to live subscribers, at most
// liveAccessPerSec / liveErrorsPerSec per second (newest rows win).
func (g *ingester) publish(b *store.ObserveBatch) {
	sec := time.Now().Unix()
	if sec != g.liveSec {
		g.liveSec, g.liveAccess, g.liveErrors = sec, 0, 0
	}
	if n := min(len(b.Access), liveAccessPerSec-g.liveAccess); n > 0 {
		for _, r := range b.Access[len(b.Access)-n:] {
			g.bus.Publish(events.LogLine, toEntry(r))
		}
		g.liveAccess += n
	}
	if n := min(len(b.Errors), liveErrorsPerSec-g.liveErrors); n > 0 {
		for _, r := range b.Errors[len(b.Errors)-n:] {
			g.bus.Publish(events.ErrorLogLine, r)
		}
		g.liveErrors += n
	}
}

// nameCache maps stream ids to names for the Host column of stream rows.
type nameCache struct {
	mu      sync.RWMutex
	streams map[string]string
}

func newNameCache() *nameCache { return &nameCache{streams: map[string]string{}} }

func (n *nameCache) streamLabel(id, port string) string {
	n.mu.RLock()
	name := n.streams[id]
	n.mu.RUnlock()
	if name != "" {
		return name
	}
	if port != "" {
		return ":" + port
	}
	return ""
}

func (n *nameCache) refresh(ctx context.Context, st *store.Store) {
	streams, err := st.Streams().List(ctx)
	if err != nil {
		return
	}
	m := make(map[string]string, len(streams))
	for _, s := range streams {
		m[s.ID] = s.Name
	}
	n.mu.Lock()
	n.streams = m
	n.mu.Unlock()
}
