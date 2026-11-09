package settlerig_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/settlerig"
)

func TestArrivals(t *testing.T) {
	a := settlerig.Arrivals(7, 5000, 8)
	if !slices.Equal(a, settlerig.Arrivals(7, 5000, 8)) {
		t.Error("the same seed gave different arrivals")
	}
	if slices.Equal(a, settlerig.Arrivals(8, 5000, 8)) {
		t.Error("different seeds gave the same arrivals")
	}
	if !slices.IsSorted(a) || a[0] <= 0 {
		t.Error("arrivals must be increasing and start after zero")
	}
	// 5000 arrivals at 8/s take about 625 s; the standard deviation of the span is
	// sqrt(5000)/8 = 8.8 s, so 5 sigma is 44 s.
	if span := a[len(a)-1].Seconds(); math.Abs(span-625) > 44 {
		t.Errorf("5000 arrivals at 8/s spanned %.0f s, want about 625", span)
	}
	// The gaps of a Poisson process are exponential: their standard deviation is their mean.
	var sum, sq float64
	prev := time.Duration(0)
	for _, x := range a {
		g := (x - prev).Seconds()
		sum, sq, prev = sum+g, sq+g*g, x
	}
	mean := sum / 5000
	sd := math.Sqrt(sq/5000 - mean*mean)
	if math.Abs(sd/mean-1) > 0.1 {
		t.Errorf("gap mean %.4f, sd %.4f: not exponential", mean, sd)
	}
}
