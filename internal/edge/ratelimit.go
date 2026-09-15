package edge

import (
	"hash/maphash"
	"sync"
	"time"
)

// rateLimiter is a sharded per-key token bucket: rate requests per second with
// burst extra requests allowed at once. Idle keys are evicted, so memory stays
// bounded under scans from many addresses.
type rateLimiter struct {
	rate   float64
	burst  float64
	shards [32]rateShard
	seed   maphash.Seed
}

type rateShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec float64, burst int) *rateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = 1
	}
	if burst < 0 {
		burst = 0
	}
	l := &rateLimiter{rate: ratePerSec, burst: float64(burst), seed: maphash.MakeSeed()}
	for i := range l.shards {
		l.shards[i].buckets = map[string]*bucket{}
	}
	return l
}

// allow consumes a token for key and reports whether the request may proceed.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	s := &l.shards[maphash.String(l.seed, key)%uint64(len(l.shards))]
	capacity := l.burst + 1
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.swept) > time.Minute {
		l.sweep(s, now)
	}
	b := s.buckets[key]
	if b == nil {
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
