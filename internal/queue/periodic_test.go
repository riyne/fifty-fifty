package queue_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/testutil"
	"github.com/riyne/fifty-fifty/internal/queue"
)

func periodicRows(t *testing.T, kind string) (n int) {
	t.Helper()
	if err := testutil.WorkerPool(t).QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnqueuePeriodic_OneJobPerPeriod(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("periodic")
	p := queue.PeriodicJob{Kind: kind, Every: time.Hour, MaxAttempts: 2, Payload: map[string]any{"x": 1}}

	created, err := queue.EnqueuePeriodic(ctx, pool, p)
	if err != nil || !created {
		t.Fatalf("first call: created=%v, %v", created, err)
	}
	created, err = queue.EnqueuePeriodic(ctx, pool, p)
	if err != nil || created {
		t.Fatalf("second call in the same period: created=%v, %v; want no new job", created, err)
	}
	if n := periodicRows(t, kind); n != 1 {
		t.Errorf("%d jobs, want 1", n)
	}

	// The job is an ordinary one: due now, with the requested attempts and payload.
	got := claim(t, pool, kind, 5)
	if len(got) != 1 || got[0].MaxAttempts != 2 || string(got[0].Payload) != `{"x": 1}` {
		t.Errorf("claimed %+v", got)
	}
}

// Many workers starting at once must still produce one job.
func TestEnqueuePeriodic_ConcurrentCallersCreateOne(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("periodic-race")
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range 20 {
		wg.Go(func() {
			ok, err := queue.EnqueuePeriodic(ctx, pool, queue.PeriodicJob{Kind: kind, Every: time.Hour})
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				created++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if created != 1 || periodicRows(t, kind) != 1 {
		t.Errorf("%d callers reported creating the job, %d rows; want 1 and 1", created, periodicRows(t, kind))
	}
}

// A finished job for a period still blocks a second one for that period, and the
// next period gets a new job.
func TestEnqueuePeriodic_NextPeriodGetsANewJob(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("periodic-next")
	p := queue.PeriodicJob{Kind: kind, Every: time.Second}

	// Stay inside one period for the first two calls: wait for the start of a fresh one.
	var toNext time.Duration
	if err := pool.QueryRow(ctx, `SELECT ceil(extract(epoch FROM now())) - extract(epoch FROM now()) || ' seconds'`).Scan(&toNext); err == nil {
		time.Sleep(toNext + 20*time.Millisecond)
	} else {
		time.Sleep(1100 * time.Millisecond)
	}
	if ok, err := queue.EnqueuePeriodic(ctx, pool, p); err != nil || !ok {
		t.Fatalf("first: %v, %v", ok, err)
	}
	c := claim(t, pool, kind, 1)
	if len(c) != 1 {
		t.Fatal("setup")
	}
	if err := completeAlone(t, pool, c[0]); err != nil {
		t.Fatal(err)
	}
	// Same period, job already succeeded: still no second job (skipped if the second ticked over meanwhile).
	if ok, _ := queue.EnqueuePeriodic(ctx, pool, p); ok && periodicRows(t, kind) == 2 {
		t.Log("the period ticked over between calls; skipping the same-period assertion")
	} else if ok {
		t.Errorf("a finished job did not block a second one for its period")
	}

	time.Sleep(1100 * time.Millisecond)
	if ok, err := queue.EnqueuePeriodic(ctx, pool, p); err != nil || !ok {
		t.Fatalf("next period: created=%v, %v", ok, err)
	}
	if n := periodicRows(t, kind); n < 2 {
		t.Errorf("%d jobs after two periods, want at least 2", n)
	}
}

func TestEnqueuePeriodic_RejectsBadJobs(t *testing.T) {
	pool := testutil.WorkerPool(t)
	for name, p := range map[string]queue.PeriodicJob{
		"period under a second": {Kind: "k", Every: 999 * time.Millisecond},
		"no period":             {Kind: "k"},
		"no kind":               {Every: time.Hour},
		"array payload":         {Kind: "k", Every: time.Hour, Payload: []int{1}},
	} {
		if _, err := queue.EnqueuePeriodic(ctx, pool, p); !errors.Is(err, queue.ErrInvalidJob) {
			t.Errorf("%s: err = %v, want ErrInvalidJob", name, err)
		}
	}
}
