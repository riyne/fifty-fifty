package main

import (
	"math"
	"testing"

	"github.com/riyne/fifty-fifty/internal/benchwork"
)

func TestDistribution_NearestRank(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3, 10, 9, 8, 7, 6} // unsorted on purpose
	d := distribution(xs)
	if d.N != 10 || d.Min != 1 || d.Max != 10 || d.Median != 5 || d.P90 != 9 {
		t.Fatalf("%+v", d)
	}
	if xs[0] != 5 {
		t.Fatal("distribution reordered its input")
	}
	if one := distribution([]float64{3}); one.Median != 3 || one.P90 != 3 || one.Min != 3 || one.Max != 3 {
		t.Fatalf("%+v", one)
	}
	if empty := distribution(nil); empty.N != 0 || math.IsNaN(empty.Median) {
		t.Fatalf("%+v", empty)
	}
}

func TestSummarize(t *testing.T) {
	s := summarize([]benchwork.RecoveryResult{
		{Jobs: 10, Succeeded: 10, Orphans: 2, RecoverySecs: []float64{4, 6}, ExtraAttempts: 2},
		{Jobs: 10, Succeeded: 9, LostJobs: 1, Orphans: 2, RecoverySecs: []float64{5, -1}, DuplicateFx: 1, ExtraAttempts: 1},
		{Jobs: 10, Succeeded: 10},
	})
	if s.Trials != 3 || s.Jobs != 30 || s.Succeeded != 29 || s.LostJobs != 1 || s.DuplicateFx != 1 || s.Orphans != 4 ||
		s.TrialsNoOrphan != 1 || s.ExtraAttempts != 3 {
		t.Fatalf("%+v", s)
	}
	// The unfinished orphan (-1) is kept out of the timings, and its trial is kept out
	// of the per-trial numbers: a trial that did not finish has no recovery time.
	if s.PerOrphanSecs.N != 3 || s.PerOrphanSecs.Max != 6 || s.PerTrialSecs.N != 1 || s.PerTrialSecs.Max != 6 {
		t.Fatalf("orphan %+v trial %+v", s.PerOrphanSecs, s.PerTrialSecs)
	}
}
