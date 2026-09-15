package logs

// Latency histogram stored per minute in metrics_minute.latency_hist.
//
// Bucket i counts requests whose total request time (nginx $request_time) is
// <= LatencyBucketsMs[i] and > LatencyBucketsMs[i-1]; the final bucket
// (index len(LatencyBucketsMs)) counts everything slower than 10 s:
//
//	≤5 ms · ≤10 · ≤25 · ≤50 · ≤100 · ≤250 · ≤500 · ≤1 s · ≤2.5 s · ≤5 s · ≤10 s · >10 s
var LatencyBucketsMs = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// HistLen is the number of counts in a latency histogram.
var HistLen = len(LatencyBucketsMs) + 1

// latencyBucket returns the histogram index for a latency in milliseconds.
func latencyBucket(ms float64) int {
	for i, b := range LatencyBucketsMs {
		if ms <= b {
			return i
		}
	}
	return len(LatencyBucketsMs)
}

// Percentile estimates the p-quantile (0 < p ≤ 1) in milliseconds from a
// histogram, interpolating linearly inside the bucket that contains it. The
// open-ended last bucket is treated as (10 s, 20 s]. ok is false when the
// histogram is empty.
func Percentile(hist []int64, p float64) (ms float64, ok bool) {
	var total int64
	for _, c := range hist {
		total += c
	}
	if total == 0 {
		return 0, false
	}
	if p <= 0 {
		p = 0.0001
	}
	if p > 1 {
		p = 1
	}
	target := p * float64(total)
	var cum int64
	last := LatencyBucketsMs[len(LatencyBucketsMs)-1]
	for i, c := range hist {
		if c == 0 {
			continue
		}
		if float64(cum+c) >= target {
			lower := 0.0
			if i > 0 {
				lower = LatencyBucketsMs[min(i, len(LatencyBucketsMs))-1]
			}
			upper := last * 2
			if i < len(LatencyBucketsMs) {
				upper = LatencyBucketsMs[i]
			}
			frac := (target - float64(cum)) / float64(c)
			return lower + frac*(upper-lower), true
		}
		cum += c
	}
	return last * 2, true
}
