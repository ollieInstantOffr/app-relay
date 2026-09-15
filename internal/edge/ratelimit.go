package edge

import (
	"hash/maphash"
	"net/netip"
	"sync"
	"time"
)

// rateLimiter is a sharded per-client token bucket equivalent to nginx
// `limit_req rate=<r>r/s burst=<b> nodelay`: burst+1 requests may arrive at
// once, then tokens refill at rate per second. Idle keys are evicted and each
// shard is capped, so memory stays bounded under scans from many addresses.
type rateLimiter struct {
	rate   float64
	burst  float64
	shards [32]rateShard
	seed   maphash.Seed
}

type rateShard struct {
	mu      sync.Mutex
	buckets map[netip.Addr]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxKeysPerShard bounds a shard at ~260k tracked clients overall.
const maxKeysPerShard = 8192

func newRateLimiter(ratePerSec float64, burst int) *rateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = 1
	}
	if burst < 0 {
		burst = 0
	}
	l := &rateLimiter{rate: ratePerSec, burst: float64(burst), seed: maphash.MakeSeed()}
	for i := range l.shards {
		l.shards[i].buckets = map[netip.Addr]*bucket{}
	}
	return l
}

// allow consumes a token for key and reports whether the request may proceed.
func (l *rateLimiter) allow(key netip.Addr, now time.Time) bool {
	s := &l.shards[maphash.Comparable(l.seed, key)%uint64(len(l.shards))]
	capacity := l.burst + 1
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.swept) > time.Minute {
		l.sweep(s, now)
	}
	b := s.buckets[key]
	if b == nil {
		if len(s.buckets) >= maxKeysPerShard {
			l.sweep(s, now)
			l.shrink(s)
		}
		b = &bucket{tokens: capacity, last: now}
		s.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have refilled completely; s.mu must be held.
func (l *rateLimiter) sweep(s *rateShard, now time.Time) {
	full := time.Duration((l.burst+1)/l.rate*float64(time.Second)) + time.Second
	for k, b := range s.buckets {
		if now.Sub(b.last) > full {
			delete(s.buckets, k)
		}
	}
	s.swept = now
}

// shrink evicts an arbitrary eighth of a shard that is still full after a
// sweep (a flood of distinct addresses); s.mu must be held.
func (l *rateLimiter) shrink(s *rateShard) {
	if len(s.buckets) < maxKeysPerShard {
		return
	}
	n := len(s.buckets) / 8
	for k := range s.buckets {
		if n == 0 {
			break
		}
		delete(s.buckets, k)
		n--
	}
}

// size returns the number of tracked keys (for tests and metrics).
func (l *rateLimiter) size() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].buckets)
		l.shards[i].mu.Unlock()
	}
	return n
}

// hostLimiter adds the exempt rules of a host's rate limit.
type hostLimiter struct {
	l             *rateLimiter
	exempt        *prefixMap[bool]
	exemptDefault bool
}

func (h *hostLimiter) allow(addr netip.Addr, now time.Time) bool {
	exempt, ok := h.exempt.lookup(addr)
	if !ok {
		exempt = h.exemptDefault
	}
	return exempt || h.l.allow(addr, now)
}
