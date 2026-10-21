package ratelimit

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestAllow_BurstThenRefill(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newWithClock(rate.Every(10*time.Second), 3, c.now)
	k := uuid.New()

	for i := range 3 {
		if ok, _ := l.Allow(k); !ok {
			t.Fatalf("attempt %d within burst was denied", i+1)
		}
	}
	ok, retry := l.Allow(k)
	if ok || retry != 10*time.Second {
		t.Fatalf("4th attempt: ok=%v retry=%v, want denied with 10s", ok, retry)
	}

	// A denied attempt must not consume tokens: after 10s exactly one is back.
	c.advance(10 * time.Second)
	if ok, _ := l.Allow(k); !ok {
		t.Fatal("token should have refilled after 10s")
	}
	if ok, _ := l.Allow(k); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newWithClock(rate.Every(time.Minute), 1, c.now)
	a, b := uuid.New(), uuid.New()
	if ok, _ := l.Allow(a); !ok {
		t.Fatal("a first")
	}
	if ok, _ := l.Allow(a); ok {
		t.Fatal("a second should be denied")
	}
	if ok, _ := l.Allow(b); !ok {
		t.Fatal("b must not be affected by a")
	}
}

func TestSweep_DropsIdleKeys(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	l := newWithClock(rate.Every(time.Second), 1, c.now)
	for range 100 {
		l.Allow(uuid.New())
	}
	c.advance(idleAfter + 2*sweepInterval)
	l.Allow(uuid.New()) // triggers the sweep
	if n := len(l.entries); n != 1 {
		t.Fatalf("entries after sweep = %d, want 1", n)
	}
}
