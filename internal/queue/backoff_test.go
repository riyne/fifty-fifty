package queue_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/queue"
)

func TestBackoff_CeilingDoublesThenSaturates(t *testing.T) {
	b := queue.Backoff{Base: 2 * time.Second, Max: 5 * time.Minute}
	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300} // seconds
	for i, w := range want {
		if got := b.Ceiling(i + 1); got != w*time.Second {
			t.Errorf("Ceiling(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
	// Attempts below 1 count as the first.
	if b.Ceiling(0) != 2*time.Second || b.Ceiling(-5) != 2*time.Second {
		t.Errorf("Ceiling(0) = %v, Ceiling(-5) = %v", b.Ceiling(0), b.Ceiling(-5))
	}
}

// The doubling must neither overflow nor loop for absurd attempt numbers.
func TestBackoff_CeilingIsSafeForHugeInputs(t *testing.T) {
	cases := []queue.Backoff{
		{Base: time.Second, Max: time.Duration(math.MaxInt64)},
		{Base: time.Duration(math.MaxInt64 / 2), Max: time.Duration(math.MaxInt64)},
		{Base: time.Duration(math.MaxInt64), Max: time.Duration(math.MaxInt64)},
		queue.DefaultBackoff,
	}
	for _, b := range cases {
		for _, attempt := range []int{1, 2, 62, 63, 64, 1000, math.MaxInt32} {
			c := b.Ceiling(attempt)
			if c <= 0 || c > b.Max {
				t.Errorf("%+v attempt %d: ceiling %v outside (0, Max]", b, attempt, c)
			}
		}
	}
}

func TestBackoff_DegenerateConfigsDoNotWait(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	for _, b := range []queue.Backoff{{}, {Base: time.Second}, {Max: time.Second}, {Base: -time.Second, Max: time.Second}} {
		if d := b.Delay(3, rnd); d != 0 {
			t.Errorf("%+v: delay %v, want 0", b, d)
		}
	}
	// Max below Base: Max wins.
	if c := (queue.Backoff{Base: time.Minute, Max: time.Second}).Ceiling(1); c != time.Second {
		t.Errorf("ceiling = %v, want 1s", c)
	}
}

// Property: every delay is within [0, ceiling], for many attempts and seeds.
func TestBackoff_DelayStaysWithinTheCeiling(t *testing.T) {
	b := queue.Backoff{Base: 100 * time.Millisecond, Max: 30 * time.Second}
	for seed := range uint64(50) {
		rnd := rand.New(rand.NewPCG(seed, 99))
		for attempt := 1; attempt <= 40; attempt++ {
			for range 50 {
				d := b.Delay(attempt, rnd)
				if d < 0 || d > b.Ceiling(attempt) {
					t.Fatalf("seed %d attempt %d: delay %v outside [0, %v]", seed, attempt, d, b.Ceiling(attempt))
				}
			}
		}
	}
}

// The jitter must actually spread: jobs that failed together must not retry
// together. Fixed seed, so this is deterministic, not statistical luck.
func TestBackoff_JitterSpreadsAcrossTheWholeRange(t *testing.T) {
	b := queue.Backoff{Base: time.Second, Max: time.Minute}
	rnd := rand.New(rand.NewPCG(7, 7))
	const n, attempt = 10000, 4 // ceiling 8 s
	ceil := b.Ceiling(attempt)
	buckets := make([]int, 8)
	var sum time.Duration
	distinct := map[time.Duration]bool{}
	for range n {
		d := b.Delay(attempt, rnd)
		sum += d
		distinct[d] = true
		buckets[min(int(d*8/ceil), 7)]++
	}
	for i, c := range buckets {
		// Uniform would put n/8 = 1250 in each; allow generous slack.
		if c < 1000 || c > 1500 {
			t.Errorf("bucket %d holds %d of %d delays, want about %d", i, c, n, n/8)
		}
	}
	if mean := sum / n; mean < ceil*45/100 || mean > ceil*55/100 {
		t.Errorf("mean delay %v, want about half of %v", mean, ceil)
	}
	if len(distinct) < n*9/10 {
		t.Errorf("only %d distinct delays in %d draws", len(distinct), n)
	}
}

func TestBackoff_SameSeedSameDelays(t *testing.T) {
	b := queue.DefaultBackoff
	a, c := rand.New(rand.NewPCG(5, 5)), rand.New(rand.NewPCG(5, 5))
	for attempt := 1; attempt <= 20; attempt++ {
		if x, y := b.Delay(attempt, a), b.Delay(attempt, c); x != y {
			t.Fatalf("attempt %d: %v != %v", attempt, x, y)
		}
	}
}

func TestSystemRandWorks(t *testing.T) {
	for range 100 {
		if d := queue.DefaultBackoff.Delay(3, queue.SystemRand); d < 0 || d > 8*time.Second {
			t.Fatalf("delay %v outside [0, 8s]", d)
		}
	}
}

// fakeRand returns a fixed fraction of the range, so the ends of it can be pinned.
type fakeRand struct {
	pick func(n int64) int64
	got  int64 // the n it was asked for
}

func (f *fakeRand) Int64N(n int64) int64 { f.got = n; return f.pick(n) }

// Delay is uniform over the closed range [0, ceiling]: the random source is asked
// for ceiling+1 values, so both ends are reachable.
func TestBackoff_DelayCoversBothEndsOfTheRange(t *testing.T) {
	b := queue.Backoff{Base: time.Second, Max: time.Minute}
	lo := &fakeRand{pick: func(int64) int64 { return 0 }}
	hi := &fakeRand{pick: func(n int64) int64 { return n - 1 }}
	if d := b.Delay(3, lo); d != 0 {
		t.Errorf("lowest draw gave %v, want 0", d)
	}
	if d := b.Delay(3, hi); d != b.Ceiling(3) {
		t.Errorf("highest draw gave %v, want the ceiling %v", d, b.Ceiling(3))
	}
	if hi.got != int64(b.Ceiling(3))+1 {
		t.Errorf("asked the random source for %d values, want ceiling+1 = %d", hi.got, int64(b.Ceiling(3))+1)
	}
}
