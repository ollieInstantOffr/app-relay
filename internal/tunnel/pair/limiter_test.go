package pair

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestLimiterPerIP(t *testing.T) {
	l := NewLimiter(3, 100, time.Minute)
	now := time.Unix(1_000_000, 0)
	a, b := netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2")

	for i := range 3 {
		if !l.Allow(a, now) {
			t.Fatalf("blocked after %d failures", i)
		}
		l.Failed(a, now)
	}
	if l.Allow(a, now) {
		t.Fatal("allowed after 3 failures")
	}
	if l.Allow(netip.MustParseAddr("::ffff:203.0.113.1"), now) {
		t.Error("IPv4-mapped address escaped the limit")
	}
	if !l.Allow(b, now) {
		t.Error("other address blocked")
	}
	if l.Allow(a, now.Add(59*time.Second)) {
		t.Error("allowed before the window ended")
	}
	if !l.Allow(a, now.Add(time.Minute)) {
		t.Error("still blocked after the window")
	}
}

func TestLimiterIPv6Prefix(t *testing.T) {
	l := NewLimiter(2, 0, time.Minute)
	now := time.Unix(1_000_000, 0)
	l.Failed(netip.MustParseAddr("2001:db8:1:2::1"), now)
	l.Failed(netip.MustParseAddr("2001:db8:1:2::ffff"), now)
	if l.Allow(netip.MustParseAddr("2001:db8:1:2:abcd::9"), now) {
		t.Error("same /64 not limited together")
	}
	if !l.Allow(netip.MustParseAddr("2001:db8:1:3::1"), now) {
		t.Error("different /64 blocked")
	}
}

func TestLimiterGlobal(t *testing.T) {
	l := NewLimiter(10, 5, time.Minute)
	now := time.Unix(1_000_000, 0)
	for i := range 5 {
		l.Failed(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}), now)
	}
	fresh := netip.MustParseAddr("192.0.2.9")
	if l.Allow(fresh, now) {
		t.Error("global limit not applied")
	}
	if !l.Allow(fresh, now.Add(time.Minute)) {
		t.Error("global limit outlived the window")
	}
}

func TestLimiterDisabledAndSweep(t *testing.T) {
	l := NewLimiter(0, 0, time.Minute)
	now := time.Unix(1_000_000, 0)
	ip := netip.MustParseAddr("192.0.2.1")
	for range 100 {
		l.Failed(ip, now)
	}
	if !l.Allow(ip, now) {
		t.Error("limits <= 0 should disable limiting")
	}

	l = NewLimiter(1, 0, time.Minute)
	for i := range limiterMaxKeys + 10 {
		l.Failed(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), now)
	}
	if n := len(l.bySource); n != limiterMaxKeys {
		t.Errorf("tracked %d sources, want cap %d", n, limiterMaxKeys)
	}
	l.Failed(ip, now.Add(2*time.Minute))
	if n := len(l.bySource); n != 1 {
		t.Errorf("tracked %d sources after sweep, want 1", n)
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter(1000, 10000, time.Minute)
	now := time.Now()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ip := netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})
			for range 100 {
				l.Allow(ip, now)
				l.Failed(ip, now)
			}
		})
	}
	wg.Wait()
	if !l.Allow(netip.MustParseAddr("192.0.2.200"), now) {
		t.Error("global limit hit early")
	}
}
