package benchwork_test

import (
	"math"
	"testing"

	"github.com/riyne/fifty-fifty/internal/benchwork"
)

func TestDistribution_NearestRank(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3, 10, 9, 8, 7, 6} // unsorted on purpose
	d := benchwork.Distribution(xs)
	if d.N != 10 || d.Min != 1 || d.Max != 10 || d.Median != 5 || d.P90 != 9 || d.P99 != 10 {
		t.Fatalf("%+v", d)
	}
	if xs[0] != 5 {
		t.Fatal("Distribution reordered its input")
	}
	hundred := make([]float64, 100)
	for i := range hundred {
		hundred[i] = float64(i + 1)
	}
	if d := benchwork.Distribution(hundred); d.Median != 50 || d.P90 != 90 || d.P99 != 99 {
		t.Fatalf("nearest rank over 1..100: %+v", d)
	}
	if one := benchwork.Distribution([]float64{3}); one.Median != 3 || one.P90 != 3 || one.P99 != 3 || one.Min != 3 || one.Max != 3 {
		t.Fatalf("%+v", one)
	}
	if empty := benchwork.Distribution(nil); empty.N != 0 || math.IsNaN(empty.Median) {
		t.Fatalf("%+v", empty)
	}
}
