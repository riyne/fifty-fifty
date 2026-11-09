package queue_test

import (
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/testutil"
	"github.com/riyne/fifty-fifty/internal/queue"
)

func statsFor(t *testing.T, kind string, always ...string) (queue.KindStats, []queue.KindStats) {
	t.Helper()
	all, err := queue.Stats(ctx, testutil.WorkerPool(t), append([]string{}, always...))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Kind == kind {
			return s, all
		}
	}
	return queue.KindStats{}, all
}

// One job in every state a job can be in, counted once each, in the right bucket.
func TestStats_CountsEachStateOnce(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("stats")

	// Running with a live lease, running with an expired one, dead. Each is claimed
	// on its own while it is the only claimable job.
	enqueue(t, pool, queue.NewJob{Kind: kind})
	if got := claim(t, pool, kind, 1); len(got) != 1 {
		t.Fatal("setup: running")
	}
	expired := enqueue(t, pool, queue.NewJob{Kind: kind})
	if got := claim(t, pool, kind, 1); len(got) != 1 || got[0].ID != expired {
		t.Fatal("setup: expired")
	}
	expireLease(t, pool, expired)
	// The expired job is claimable again by design, so make the next claim take a
	// job of its own: give it a far-future retry is not possible; instead create dead now.
	dead := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1, RunAt: past(10 * time.Minute)})
	var deadClaim []queue.Claimed
	for _, c := range claim(t, pool, kind, 5) {
		if c.ID == dead {
			deadClaim = append(deadClaim, c)
		}
	}
	// That claim also re-leased the expired job (attempt 2): it is running now.
	if len(deadClaim) != 1 {
		t.Fatal("setup: dead")
	}
	if st, err := queue.Fail(ctx, pool, deadClaim[0], queue.FailParams{Error: "x"}); err != nil || st != queue.StateDead {
		t.Fatalf("setup: %q, %v", st, err)
	}
	expireLease(t, pool, expired) // expired again, now that the claim above is done

	// Queued: two due (the older one 120 s), one delayed.
	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(120 * time.Second)})
	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(30 * time.Second)})
	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: time.Now().Add(time.Hour)})

	s, _ := statsFor(t, kind)
	if s.Ready != 2 || s.Delayed != 1 || s.Running != 1 || s.Expired != 1 || s.Dead != 1 {
		t.Errorf("stats = %+v; want ready 2, delayed 1, running 1, expired 1, dead 1", s)
	}
	// The job's run_at comes from this machine's clock and the age from the database's, which
	// can differ by milliseconds (the Docker VM): a bound of exactly 120 s failed at 119.99 s.
	// What matters is that the older job (120 s) is reported, not the 30 s one.
	if s.OldestReadyAge < 100*time.Second || s.OldestReadyAge > 140*time.Second {
		t.Errorf("oldest ready age = %v, want about 120s (the older of the two due jobs, not the 30 s one)", s.OldestReadyAge)
	}
}

func TestStats_ZeroForQuietKindsAndNoFinishedJobs(t *testing.T) {
	pool := testutil.WorkerPool(t)
	finished, quiet := newKind("stats-done"), newKind("stats-quiet")
	enqueue(t, pool, queue.NewJob{Kind: finished})
	c := claim(t, pool, finished, 1)
	if len(c) != 1 {
		t.Fatal("setup")
	}
	if err := completeAlone(t, pool, c[0]); err != nil {
		t.Fatal(err)
	}

	_, all := statsFor(t, "", quiet)
	var sawQuiet bool
	for i, s := range all {
		if s.Kind == finished {
			t.Errorf("a kind with only finished jobs was reported: %+v", s)
		}
		if s.Kind == quiet {
			sawQuiet = true
			if s != (queue.KindStats{Kind: quiet}) {
				t.Errorf("quiet kind = %+v, want all zeros", s)
			}
		}
		if i > 0 && all[i-1].Kind > s.Kind {
			t.Errorf("not sorted: %q before %q", all[i-1].Kind, s.Kind)
		}
	}
	if !sawQuiet {
		t.Error("a requested kind with no jobs was left out")
	}
}

// Nothing due, only a retry backing off: nothing is waiting, so the age is zero, not
// the (negative) time until the retry.
func TestStats_OldestReadyAgeIgnoresDelayedJobs(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("stats-delayed")
	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: time.Now().Add(time.Hour)})

	s, _ := statsFor(t, kind)
	if s.Delayed != 1 || s.Ready != 0 || s.OldestReadyAge != 0 {
		t.Errorf("stats = %+v; want 1 delayed, none ready, age exactly 0", s)
	}
}
