package apply

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/lb/lbengine"
	"github.com/instantoffr/relay/internal/model"
)

// lbUsable reads which backends of a load balancer engine have a server that
// can take new traffic, from the engine's runtime "show stat".
func (s *Service) lbUsable(ctx context.Context, engine string) (map[string]bool, error) {
	c := s.app.Client(engine)
	if c == nil {
		return nil, errors.New("unknown load balancer engine " + engine)
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := c.Runtime(rctx, "show stat")
	if err != nil {
		return nil, err
	}
	return lbengine.UsableBackends(out), nil
}

// lostBackends lists backends that had a usable server before a load balancer
// switch but have none on the new engine. This covers streams and hosts that
// reach servers through a backend end to end, not just the frontend socket.
// Backends the new engine doesn't report are ignored.
func (s *Service) lostBackends(ctx context.Context, engine string, before map[string]bool) ([]string, error) {
	now, err := s.lbUsable(ctx, engine)
	if err != nil {
		return nil, err
	}
	var lost []string
	for name, ok := range before {
		if up, present := now[name]; ok && present && !up {
			lost = append(lost, name)
		}
	}
	sort.Strings(lost)
	return lost, nil
}

// checkSettleTicks is how many health-check seconds pass before server health
// reported by a freshly started load balancer means something: servers start
// UP and the first failed check marks them DOWN, so one check interval (the
// longest configured) plus a margin.
func checkSettleTicks(snap *model.Snapshot) int {
	longest := int64(2000)
	consider := func(d string) {
		if ms, ok := durationMs(d); ok && ms > longest {
			longest = ms
		}
	}
	if snap != nil {
		consider(snap.HAProxy.CheckInterval)
		for _, b := range snap.Backends {
			consider(b.HealthCheck.Interval)
		}
	}
	return int((longest+999)/1000) + 2
}

// durationMs parses an HAProxy duration (us, ms, s, m, h, d; bare = ms).
func durationMs(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	for _, u := range []struct {
		suffix string
		ms     float64
	}{{"us", 0.001}, {"ms", 1}, {"s", 1000}, {"m", 60000}, {"h", 3600000}, {"d", 86400000}} {
		if n, ok := strings.CutSuffix(v, u.suffix); ok {
			f, err := strconv.ParseFloat(n, 64)
			return int64(f * u.ms), err == nil && f >= 0
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= 0
}
