package balancer

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestLogPrefixParsesLikeHAProxy(t *testing.T) {
	buf := &syncBuffer{}
	l := newLogger(buf)
	l.sync(lvlAlert, "a")
	l.sync(lvlWarning, "b")
	l.sync(lvlNotice, "c\nd")
	l.close()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{"[ALERT]    (", "[WARNING]  (", "[NOTICE]   ("}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %q lacks prefix %q", lines[i], w)
		}
	}
	if !strings.HasSuffix(lines[2], ") : c d") {
		t.Errorf("newlines not flattened: %q", lines[2])
	}
}

func TestStickTable(t *testing.T) {
	now := time.Now()
	tb := newStickTable(2, time.Minute)
	a, b, c := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")
	tb.put(a, "s1", now)
	tb.put(b, "s2", now)
	tb.get(a, now) // no LRU refresh on get: stick on src refreshes on store
	tb.put(a, "s1", now)
	tb.put(c, "s3", now) // evicts b (least recently stored)
	if _, ok := tb.get(b, now); ok || tb.len() != 2 {
		t.Fatalf("LRU eviction: len %d", tb.len())
	}
	if s, ok := tb.get(a, now.Add(59*time.Second)); !ok || s != "s1" {
		t.Fatal("entry expired early")
	}
	if _, ok := tb.get(a, now.Add(61*time.Second)); ok {
		t.Fatal("entry did not expire")
	}
}

func TestFreqCtr(t *testing.T) {
	var f freqCtr
	base := time.Unix(1000, 0)
	for range 10 {
		f.add(base)
	}
	if r := f.read(base.Add(900 * time.Millisecond)); r != 10 {
		t.Fatalf("same second: %d", r)
	}
	if r := f.read(base.Add(1500 * time.Millisecond)); r != 5 {
		t.Fatalf("half of previous second: %d", r)
	}
	if r := f.read(base.Add(3 * time.Second)); r != 0 {
		t.Fatalf("stale: %d", r)
	}
	var s swrate
	s.add(100)
	if s.avg() != 100 {
		t.Fatal(s.avg())
	}
	s.add(0)
	if s.avg() != 50 {
		t.Fatal(s.avg())
	}
}
