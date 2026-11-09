package queue_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/harness/testutil"
	"github.com/riyne/fifty-fifty/internal/queue"
)

// finishedJob inserts a job already in a final state, finished `age` ago, which is
// faster and more exact than running one through the queue and backdating it.
func finishedJob(t *testing.T, pool *pgxpool.Pool, kind, state string, age string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, state, attempts, max_attempts, run_at, finished_at)
		VALUES ($1, '{}', $2, 1, CASE WHEN $2 = 'dead' THEN 1 ELSE 5 END, now(), now() - $3::interval)
		RETURNING id`, kind, state, age).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func jobExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// The pruner touches succeeded jobs past retention and nothing else: not recent
// ones, not dead-letters (however old), not queued or running jobs.
func TestPruneFinished_DeletesOnlyOldSucceededJobs(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("prune")
	// A retention of a day and ages of days: other tests' jobs, which are minutes
	// old, are never in range, so this cannot disturb them.
	old := []uuid.UUID{
		finishedJob(t, pool, kind, "succeeded", "3 days"),
		finishedJob(t, pool, kind, "succeeded", "2 days"),
		finishedJob(t, pool, kind, "succeeded", "25 hours"),
		finishedJob(t, pool, kind, "succeeded", "10 days"),
		finishedJob(t, pool, kind, "succeeded", "30 days"),
	}
	recent := finishedJob(t, pool, kind, "succeeded", "23 hours")
	dead := finishedJob(t, pool, kind, "dead", "30 days")
	queued := enqueue(t, pool, queue.NewJob{Kind: kind})
	running := enqueue(t, pool, queue.NewJob{Kind: kind})
	if got := claim(t, pool, kind, 1); len(got) != 1 {
		t.Fatalf("claimed %d", len(got))
	} else if got[0].ID == queued { // either job may be claimed first
		queued, running = running, queued
	}

	n, err := queue.PruneFinished(ctx, pool, 24*time.Hour, 2)
	if err != nil || n < 0 || n > 2 {
		t.Fatalf("first batch: deleted %d, %v; a call deletes at most its batch of 2", n, err)
	}
	for { // until nothing is left in range, as the worker does
		n, err := queue.PruneFinished(ctx, pool, 24*time.Hour, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	for _, id := range old {
		if jobExists(t, pool, id) {
			t.Errorf("succeeded job %s past retention survived", id)
		}
	}
	for name, id := range map[string]uuid.UUID{"recent succeeded": recent, "dead": dead, "queued": queued, "running": running} {
		if !jobExists(t, pool, id) {
			t.Errorf("%s job was pruned", name)
		}
	}
}

// Oldest first, so a backlog is worked off from the end that is furthest past
// retention and a cut-off batch never leaves an older job behind a newer one.
func TestPruneFinished_DeletesOldestFirst(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("prune-order")
	// Older than anything another test or earlier run leaves behind.
	a := finishedJob(t, pool, kind, "succeeded", "42 years")
	b := finishedJob(t, pool, kind, "succeeded", "41 years")
	c := finishedJob(t, pool, kind, "succeeded", "40 years")

	n, err := queue.PruneFinished(ctx, pool, 24*time.Hour, 1)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v; want exactly 1", n, err)
	}
	if jobExists(t, pool, a) || !jobExists(t, pool, b) || !jobExists(t, pool, c) {
		t.Fatalf("the batch of one must delete the oldest job (42 years) and no other: a=%v b=%v c=%v",
			jobExists(t, pool, a), jobExists(t, pool, b), jobExists(t, pool, c))
	}
	for { // leave nothing 40 years old behind for the next run
		n, err := queue.PruneFinished(ctx, pool, 24*time.Hour, 100)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
}

func TestPruneFinished_RefusesDangerousInput(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("prune-guard")
	fresh := finishedJob(t, pool, kind, "succeeded", "1 second")

	for name, c := range map[string]struct {
		retention time.Duration
		batch     int
	}{
		"zero retention":     {0, 10},
		"negative retention": {-time.Hour, 10},
		"under the minimum":  {queue.MinRetention - time.Second, 10},
		"zero batch":         {24 * time.Hour, 0},
		"negative batch":     {24 * time.Hour, -1},
	} {
		if _, err := queue.PruneFinished(ctx, pool, c.retention, c.batch); !errors.Is(err, queue.ErrInvalidJob) {
			t.Errorf("%s: err = %v, want ErrInvalidJob", name, err)
		}
	}
	if !jobExists(t, pool, fresh) {
		t.Fatal("a refused prune deleted a job")
	}
	// The boundary itself is allowed.
	if _, err := queue.PruneFinished(ctx, pool, queue.MinRetention, 1); err != nil {
		t.Errorf("retention of exactly %v refused: %v", queue.MinRetention, err)
	}
}
