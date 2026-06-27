package session

import "math"

// LatencyBuckets defines the histogram's fixed boundaries (exclusive upper, in
// ms). The last is the "and above" bucket. Exported so the sqlite sub-package
// can reference the bucket definitions.
var LatencyBuckets = []LatencyBucket{
	{Label: "<1s", UpperMs: 1000},
	{Label: "1-2s", UpperMs: 2000},
	{Label: "2-5s", UpperMs: 5000},
	{Label: "5-10s", UpperMs: 10000},
	{Label: "10-20s", UpperMs: 20000},
	{Label: "20-30s", UpperMs: 30000},
	{Label: "30s+", UpperMs: math.MaxInt64},
}

// Percentile returns the nearest-rank pth percentile of an ascending slice.
func Percentile(sortedAsc []int64, p float64) int64 {
	n := len(sortedAsc)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sortedAsc[rank-1]
}

// Histogram buckets latencies into LatencyBuckets (counts per bar).
func Histogram(latencies []int64) []LatencyBucket {
	out := make([]LatencyBucket, len(LatencyBuckets))
	copy(out, LatencyBuckets)
	for _, l := range latencies {
		for i := range out {
			if l < out[i].UpperMs {
				out[i].Count++
				break
			}
		}
	}
	return out
}
