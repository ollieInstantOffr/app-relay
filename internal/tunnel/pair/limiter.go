package pair

import (
	"net/netip"
	"sync"
	"time"
)

// limiterMaxKeys caps per-source state; beyond it only the global limit applies.
const limiterMaxKeys = 4096

// Limiter throttles failed pairing attempts per source IP and globally, using
// fixed windows that start at the first failure. IPv6 sources are grouped by
// /64 so one host cannot rotate addresses. A limit <= 0 disables that
// dimension. Callers check Allow before attempting to pair and call Failed on
// any pairing error. It is safe for concurrent use.
type Limiter struct {
	perIP  int
	global int
	window time.Duration

	mu        sync.Mutex
	all       limiterWindow
	bySource  map[netip.Prefix]limiterWindow
	lastSweep time.Time
}

type limiterWindow struct {
	start time.Time
	count int
}

// NewLimiter allows perIP failures per source and global failures in total per window.
func NewLimiter(perIP int, global int, window time.Duration) *Limiter {
	return &Limiter{perIP: perIP, global: global, window: window, bySource: map[netip.Prefix]limiterWindow{}}
}

// Allow reports whether ip may attempt to pair at now.
func (l *Limiter) Allow(ip netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global > 0 && l.current(l.all, now) >= l.global {
		return false
	}
	return l.perIP <= 0 || l.current(l.bySource[sourceKey(ip)], now) < l.perIP
}

// Failed records a failed pairing attempt from ip at now.
func (l *Limiter) Failed(ip netip.Addr, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = l.bump(l.all, now)
	if now.Sub(l.lastSweep) >= l.window {
		for k, w := range l.bySource {
			if l.current(w, now) == 0 {
				delete(l.bySource, k)
			}
		}
		l.lastSweep = now
	}
	key := sourceKey(ip)
	w, ok := l.bySource[key]
	if !ok && len(l.bySource) >= limiterMaxKeys {
		return
	}
	l.bySource[key] = l.bump(w, now)
}

func (l *Limiter) current(w limiterWindow, now time.Time) int {
	if w.count == 0 || now.Sub(w.start) >= l.window || now.Before(w.start) {
		return 0
	}
	return w.count
}

func (l *Limiter) bump(w limiterWindow, now time.Time) limiterWindow {
	if l.current(w, now) == 0 {
		return limiterWindow{start: now, count: 1}
	}
	w.count++
	return w
}

func sourceKey(ip netip.Addr) netip.Prefix {
	ip = ip.Unmap().WithZone("")
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	p, err := ip.Prefix(bits)
	if err != nil {
		return netip.Prefix{} // invalid address: all share one bucket
	}
	return p
}
