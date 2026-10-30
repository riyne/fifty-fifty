package queue

import (
	"math/rand/v2"
	"time"
)

// Backoff is the retry delay policy: exponential growth with full jitter.
//
// After attempt n fails, the job waits a random time between 0 and
// min(Max, Base * 2^(n-1)). The ceiling doubles each attempt so a struggling
// dependency gets progressively more room; the randomness means jobs that failed
// together (a provider outage, a 429 burst) do not all come back in the same
// instant. "Full" jitter, rather than adding a small random amount to a fixed
// delay, is the variant that spreads a synchronized crowd best. The cost is that a
// retry can come back almost immediately; if a dependency needs a guaranteed
// pause, raise Base or switch to "equal" jitter (half the ceiling fixed, half
// random).
//
// A job whose worker crashed is not retried through this: its lease simply
// expires and the next claim picks it up (ClaimJobs).
type Backoff struct {
	Base time.Duration // ceiling after the first failure
	Max  time.Duration // ceiling never grows past this
}

// DefaultBackoff: ceilings of 2 s, 4 s, 8 s, ... capped at 5 minutes.
var DefaultBackoff = Backoff{Base: 2 * time.Second, Max: 5 * time.Minute}

// Rand is the source of randomness; *rand.Rand from math/rand/v2 satisfies it,
// so tests can pass a seeded one.
type Rand interface {
	Int64N(n int64) int64
}

type systemRand struct{}

func (systemRand) Int64N(n int64) int64 { return rand.Int64N(n) }

// SystemRand uses the goroutine-safe top-level functions of math/rand/v2.
var SystemRand Rand = systemRand{}

// Ceiling is the largest delay Delay can return after the given failed attempt
// (1 for the first). Attempts below 1 count as 1.
func (b Backoff) Ceiling(attempt int) time.Duration {
	if b.Base <= 0 || b.Max <= 0 {
		return 0
	}
	d := min(b.Base, b.Max)
	// d doubles each pass, so the guard fires within 63 passes however large
	// attempt is; it is also what keeps d*2 from overflowing.
	for i := 1; i < attempt; i++ {
		if d > b.Max/2 { // doubling would pass Max
			return b.Max
		}
		d *= 2
	}
	return d
}

// Delay picks the wait before the next attempt, uniformly from [0, Ceiling(attempt)].
func (b Backoff) Delay(attempt int, rnd Rand) time.Duration {
	c := b.Ceiling(attempt)
	if c <= 0 {
		return 0
	}
	return time.Duration(rnd.Int64N(int64(c) + 1))
}
