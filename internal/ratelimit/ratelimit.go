// Package ratelimit is a per-key token-bucket limiter held in process memory.
//
// Limits are per api process and reset on restart. That is adequate for the
// single-instance deployment this project targets; running several api
// instances would multiply the effective limit, and a shared store (Postgres)
// would be needed. The README should say so.
package ratelimit

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

const (
	idleAfter     = 10 * time.Minute
	sweepInterval = time.Minute
)

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// Limiter hands out one bucket per user id.
type Limiter struct {
	rate  rate.Limit
	burst int
	now   func() time.Time

	mu        sync.Mutex
	entries   map[uuid.UUID]*entry
	lastSweep time.Time
}

// New returns a limiter allowing `burst` immediate events per key, refilling at r.
func New(r rate.Limit, burst int) *Limiter {
	return newWithClock(r, burst, time.Now)
}

func newWithClock(r rate.Limit, burst int, now func() time.Time) *Limiter {
	return &Limiter{rate: r, burst: burst, now: now, entries: map[uuid.UUID]*entry{}, lastSweep: now()}
}

// Allow consumes one token for key. If none is available it consumes nothing
// and returns how long until one will be.
func (l *Limiter) Allow(key uuid.UUID) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)
	e := l.entries[key]
	if e == nil {
		e = &entry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.entries[key] = e
	}
	e.seen = now

	r := e.lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second // burst is 0: can never succeed
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// sweep drops buckets idle long enough to have fully refilled, bounding memory
// by the number of distinct recent users. Caller holds l.mu.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for k, e := range l.entries {
		if now.Sub(e.seen) > idleAfter {
			delete(l.entries, k)
		}
	}
}
