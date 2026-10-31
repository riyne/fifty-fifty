package benchwork

import (
	"math"
	"sort"
)

// Dist summarizes a sample. Percentiles use the nearest-rank method (the smallest
// value with at least p percent of the sample at or below it), so every reported
// number is a value that was observed.
type Dist struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
}

// Distribution computes the Dist of xs without reordering it. An empty sample gives
// the zero Dist (N is 0).
func Distribution(xs []float64) Dist {
	if len(xs) == 0 {
		return Dist{}
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	rank := func(p float64) float64 {
		i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
		return sorted[max(i, 0)]
	}
	return Dist{N: len(sorted), Min: sorted[0], Median: rank(50), P90: rank(90), P99: rank(99), Max: sorted[len(sorted)-1]}
}
