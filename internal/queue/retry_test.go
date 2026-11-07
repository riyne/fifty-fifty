package queue_test

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// failUntilGone plays a handler that always fails: claim, fail with the backoff's
// delay, repeat. It returns the delay used after each attempt and the final state.
// To avoid sleeping through real delays, "time passes" by moving run_at to now
// after checking the job really was not claimable before its retry time.
func failUntilGone(t *testing.T, pool *pgxpool.Pool, kind string, id uuid.UUID, b queue.Backoff, rnd queue.Rand) (delays []time.Duration, finalState string) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		got := claim(t, pool, kind, 1)
		if len(got) != 1 || got[0].ID != id || got[0].Attempt != attempt {
			t.Fatalf("attempt %d: claim = %+v", attempt, got)
		}
		d := b.Delay(attempt, rnd)
		state, err := queue.Fail(ctx, pool, got[0], queue.FailParams{Error: "always fails", RetryIn: d})
		if err != nil {
			t.Fatal(err)
		}
		if state == queue.StateDead {
			return delays, state
		}
		delays = append(delays, d)

		// The stored retry time is exactly the delay the policy chose, on the
		// database clock (within the test's own latency).
		var in time.Duration
		if err := pool.QueryRow(ctx, `SELECT run_at - now() FROM jobs WHERE id = $1`, id).Scan(&in); err != nil {
			t.Fatal(err)
		}
		if in > d || in < d-5*time.Second {
			t.Fatalf("attempt %d: retry in %v, policy chose %v", attempt, in, d)
		}
		if d > time.Second { // far enough away that the claim below proves it is held back
			if early := claim(t, pool, kind, 1); len(early) != 0 {
				t.Fatalf("attempt %d: claimed %v before its retry time", attempt, d)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET run_at = now() WHERE id = $1 AND state = 'queued'`, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A job that always fails is retried with growing, jittered delays until it has
// used max_attempts, then dead-lettered with the last error. Never more attempts
// than allowed.
func TestRetry_AlwaysFailingJobEndsDeadAfterMaxAttempts(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("retry-dead")
	const maxAttempts = 5
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: maxAttempts})
	b := queue.Backoff{Base: 4 * time.Second, Max: 20 * time.Second}

	delays, state := failUntilGone(t, pool, kind, id, b, rand.New(rand.NewPCG(3, 3)))

	if state != queue.StateDead || len(delays) != maxAttempts-1 {
		t.Fatalf("state %q after %d retries, want dead after %d", state, len(delays), maxAttempts-1)
	}
	for i, d := range delays {
		if d > b.Ceiling(i+1) {
			t.Errorf("retry %d waited %v, over the ceiling %v", i+1, d, b.Ceiling(i+1))
		}
	}
	row := getJob(t, pool, id)
	if row.State != queue.StateDead || row.Attempts != maxAttempts || row.FinishedAt == nil ||
		row.LastError == nil || *row.LastError != "always fails" {
		t.Errorf("row = %+v", row)
	}
	if got := claim(t, pool, kind, 1); len(got) != 0 {
		t.Errorf("a dead job was claimed: %+v", got)
	}
}

// A transient failure: two failed attempts, then success. The job ends succeeded,
// on its third attempt, not dead.
func TestRetry_TransientFailureEventuallySucceeds(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("retry-ok")
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 5})
	rnd := rand.New(rand.NewPCG(4, 4))

	for attempt := 1; attempt <= 2; attempt++ {
		c := claim(t, pool, kind, 1)[0]
		if _, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: "429", RetryIn: queue.DefaultBackoff.Delay(attempt, rnd)}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET run_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	c := claim(t, pool, kind, 1)[0]
	if c.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", c.Attempt)
	}
	if err := completeAlone(t, pool, c); err != nil {
		t.Fatal(err)
	}
	if row := getJob(t, pool, id); row.State != queue.StateSucceeded || row.Attempts != 3 || row.LastError == nil {
		t.Errorf("row = %+v (last_error keeps the last failure's message)", row)
	}
}

// Crashes use leases, not backoff: each takeover is the next attempt, and when the
// last attempt's lease runs out the reaper dead-letters the job.
func TestRetry_RepeatedCrashesEndDeadViaTheReaper(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("retry-crash")
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})

	for attempt := 1; attempt <= 3; attempt++ {
		got := claim(t, pool, kind, 1)
		if len(got) != 1 || got[0].Attempt != attempt {
			t.Fatalf("attempt %d: claim = %+v", attempt, got)
		}
		expireLease(t, pool, id) // the worker dies without reporting
	}
	if got := claim(t, pool, kind, 1); len(got) != 0 {
		t.Fatalf("a job past its last attempt was claimed: %+v", got)
	}
	// Reaping is global; a worker in another test process may get there first. Either
	// way the job must end dead, with its attempts intact.
	if _, err := queue.ReapExhausted(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if row := getJob(t, pool, id); row.State != queue.StateDead || row.Attempts != 3 {
		t.Errorf("row = %+v", row)
	}
}

// ---- requeue ----

func killJob(t *testing.T, pool *pgxpool.Pool, kind string) uuid.UUID {
	t.Helper()
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})
	c := claim(t, pool, kind, 1)[0]
	if st, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: "it broke"}); err != nil || st != queue.StateDead {
		t.Fatalf("setup: %q, %v", st, err)
	}
	return id
}

func TestRequeue_GivesDeadJobsAFreshSetOfAttempts(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("requeue")
	dead := killJob(t, pool, kind)

	got, err := queue.Requeue(ctx, pool, queue.RequeueParams{IDs: []uuid.UUID{dead}})
	if err != nil || len(got) != 1 || got[0] != dead {
		t.Fatalf("requeued %v, %v", got, err)
	}
	row := getJob(t, pool, dead)
	if row.State != queue.StateQueued || row.Attempts != 0 || row.FinishedAt != nil || row.Token != nil ||
		row.LastError == nil || *row.LastError != "it broke" {
		t.Errorf("row = %+v, want queued, attempts 0, last_error kept", row)
	}
	c := claim(t, pool, kind, 1)
	if len(c) != 1 || c[0].ID != dead || c[0].Attempt != 1 {
		t.Errorf("claim after requeue = %+v, want attempt 1", c)
	}
}

func TestRequeue_ByKindAndIdAreScopedAndIdempotent(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind, other := newKind("rq-kind"), newKind("rq-other")
	a, b, o := killJob(t, pool, kind), killJob(t, pool, kind), killJob(t, pool, other)
	live := enqueue(t, pool, queue.NewJob{Kind: kind})

	// One id: only that job, not its siblings.
	if got, err := queue.Requeue(ctx, pool, queue.RequeueParams{IDs: []uuid.UUID{a}}); err != nil || len(got) != 1 {
		t.Fatalf("by id: %v, %v", got, err)
	}
	if row := getJob(t, pool, b); row.State != queue.StateDead {
		t.Errorf("sibling was requeued: %+v", row)
	}
	// Kind: the remaining dead job of that kind; not another kind, not the live job.
	got, err := queue.Requeue(ctx, pool, queue.RequeueParams{Kind: kind})
	if err != nil || len(got) != 1 || got[0] != b {
		t.Fatalf("by kind: %v, %v", got, err)
	}
	if row := getJob(t, pool, o); row.State != queue.StateDead {
		t.Errorf("a job of another kind was requeued: %+v", row)
	}
	if row := getJob(t, pool, live); row.State != queue.StateQueued || row.Attempts != 0 {
		t.Errorf("a live job was touched: %+v", row)
	}
	// Id and kind together must both match.
	if got, err := queue.Requeue(ctx, pool, queue.RequeueParams{IDs: []uuid.UUID{o}, Kind: kind}); err != nil || len(got) != 0 {
		t.Errorf("id of another kind matched: %v, %v", got, err)
	}
	// Repeating it, or aiming it at a job that has been requeued and run again, does nothing.
	run := claim(t, pool, kind, 5)
	if len(run) == 0 {
		t.Fatal("setup: nothing running")
	}
	if got, err := queue.Requeue(ctx, pool, queue.RequeueParams{Kind: kind}); err != nil || len(got) != 0 {
		t.Errorf("second requeue: %v, %v", got, err)
	}
	for _, c := range run {
		if row := getJob(t, pool, c.ID); row.State != queue.StateRunning {
			t.Errorf("a running job was disturbed: %+v", row)
		}
	}
}

func TestRequeue_RefusesToMatchEverything(t *testing.T) {
	pool := testutil.WorkerPool(t)
	if _, err := queue.Requeue(ctx, pool, queue.RequeueParams{}); !errors.Is(err, queue.ErrInvalidJob) {
		t.Errorf("err = %v, want ErrInvalidJob", err)
	}
}
