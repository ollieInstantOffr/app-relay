package logs

import (
	"math"
	"testing"
)

func TestLatencyBucket(t *testing.T) {
	cases := map[float64]int{0: 0, 5: 0, 5.01: 1, 10: 1, 38: 3, 212: 5, 1000: 7, 9999: 10, 10000: 10, 10001: 11, 60000: 11}
	for ms, want := range cases {
		if got := latencyBucket(ms); got != want {
			t.Errorf("latencyBucket(%v) = %d, want %d", ms, got, want)
		}
	}
	if HistLen != 12 {
		t.Fatalf("HistLen = %d", HistLen)
	}
}

func TestPercentile(t *testing.T) {
	if _, ok := Percentile(nil, 0.5); ok {
		t.Fatal("empty histogram should not report a percentile")
	}
	if _, ok := Percentile(make([]int64, HistLen), 0.95); ok {
		t.Fatal("all-zero histogram should not report a percentile")
	}

	// 10 requests in (5,10] ms: the median interpolates to the bucket middle.
	h := make([]int64, HistLen)
	h[1] = 10
	if p, _ := Percentile(h, 0.5); math.Abs(p-7.5) > 1e-9 {
		t.Fatalf("p50 = %v", p)
	}

	// 90 fast (≤5 ms) + 10 slow in (100,250]: p50 in the first bucket, p95 in the slow one.
	h = make([]int64, HistLen)
	h[0], h[5] = 90, 10
	if p, _ := Percentile(h, 0.5); p <= 0 || p > 5 {
		t.Fatalf("p50 = %v", p)
	}
	p95, _ := Percentile(h, 0.95)
	if math.Abs(p95-175) > 1e-9 { // halfway through (100,250]
		t.Fatalf("p95 = %v", p95)
	}
	if p, _ := Percentile(h, 1); p != 250 {
		t.Fatalf("p100 = %v", p)
	}

	// Open-ended bucket is capped at twice the last bound.
	h = make([]int64, HistLen)
	h[HistLen-1] = 4
	if p, _ := Percentile(h, 1); p != 20000 {
		t.Fatalf("overflow p100 = %v", p)
	}
}
