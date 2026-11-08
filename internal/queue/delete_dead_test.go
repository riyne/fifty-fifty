package queue_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// makeDead enqueues a one-attempt job of kind, claims it and fails it: dead, died just now.
func makeDead(t *testing.T, pool *pgxpool.Pool, kind string) uuid.UUID {
	t.Helper()
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})
	got := claim(t, pool, kind, 1)
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("setup: claimed %v, want %s", got, id)
	}
	if st, err := queue.Fail(ctx, pool, got[0], queue.FailParams{Error: "boom"}); err != nil || st != queue.StateDead {
		t.Fatalf("setup: fail = %q, %v", st, err)
	}
	return id
}

// diedAgo moves a dead job's finished_at into the past.
func diedAgo(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, d time.Duration) {
	t.Helper()
	tag, err := pool.Exec(ctx, `UPDATE jobs SET finished_at = now() - make_interval(secs => $2::float8) WHERE id = $1 AND state = 'dead'`, id, d.Seconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("setup: age a dead job: %v, %d rows", err, tag.RowsAffected())
	}
}

func exists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func ascending(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

// There is no "everything" form: a call with no filter, or a negative age, deletes nothing.
func TestDeleteDead_NeedsAFilter(t *testing.T) {
	pool := testutil.WorkerPool(t)
	id := makeDead(t, pool, newKind("del-nofilter"))
	for name, p := range map[string]queue.DeleteDeadParams{
		"nothing at all":  {},
		"only a dry run":  {DryRun: true},
		"a negative age":  {OlderThan: -time.Hour},
		"an empty id set": {IDs: []uuid.UUID{}},
	} {
		if got, err := queue.DeleteDead(ctx, pool, p); !errors.Is(err, queue.ErrInvalidJob) || got != nil {
			t.Errorf("%s: %v, %v, want ErrInvalidJob and no ids", name, got, err)
		}
	}
	if !exists(t, pool, id) {
		t.Fatal("a refused call deleted a job")
	}
}

// Only dead jobs: a queued, a running (live lease), a delayed and a succeeded job of the same
// kind survive a delete of that kind.
func TestDeleteDead_OnlyDeadJobsAreTouched(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("del-only-dead")
	dead := makeDead(t, pool, kind)

	succeeded := enqueue(t, pool, queue.NewJob{Kind: kind})
	c := claim(t, pool, kind, 1)
	if len(c) != 1 || c[0].ID != succeeded {
		t.Fatalf("setup: claimed %v", c)
	}
	if err := completeAlone(t, pool, c[0]); err != nil {
		t.Fatalf("setup: complete: %v", err)
	}
	running := enqueue(t, pool, queue.NewJob{Kind: kind})
	if c := claim(t, pool, kind, 1); len(c) != 1 || c[0].ID != running {
		t.Fatalf("setup: running: %v", c)
	}
	queued := enqueue(t, pool, queue.NewJob{Kind: kind})
	delayed := enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: time.Now().Add(time.Hour)})

	got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{Kind: kind})
	if err != nil || !slices.Equal(got, []uuid.UUID{dead}) {
		t.Fatalf("DeleteDead = %v, %v, want only %s", got, err, dead)
	}
	if exists(t, pool, dead) {
		t.Error("the dead job is still there")
	}
	for name, id := range map[string]uuid.UUID{"succeeded": succeeded, "running": running, "queued": queued, "delayed": delayed} {
		if !exists(t, pool, id) {
			t.Errorf("the %s job was deleted", name)
		}
	}
	// Naming a job that is not dead, by id, does not delete it either.
	if got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{IDs: []uuid.UUID{queued, running, succeeded}}); err != nil || len(got) != 0 {
		t.Errorf("by id, jobs that are not dead: %v, %v, want none", got, err)
	}
	if !exists(t, pool, queued) || !exists(t, pool, running) || !exists(t, pool, succeeded) {
		t.Error("a job that is not dead was deleted by id")
	}
}

// A job must match every filter given.
func TestDeleteDead_FiltersCombineAsAnd(t *testing.T) {
	pool := testutil.WorkerPool(t)
	a, b := newKind("del-and-a"), newKind("del-and-b")
	aOld, aFresh, bOld := makeDead(t, pool, a), makeDead(t, pool, a), makeDead(t, pool, b)
	diedAgo(t, pool, aOld, 48*time.Hour)
	diedAgo(t, pool, bOld, 48*time.Hour)

	// kind and age: only the old one of kind a.
	if got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{Kind: a, OlderThan: 24 * time.Hour, DryRun: true}); err != nil || !slices.Equal(got, []uuid.UUID{aOld}) {
		t.Errorf("kind a, 24h: %v, %v, want %s", got, err, aOld)
	}
	// ids and kind: an id of another kind does not match.
	if got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{IDs: []uuid.UUID{aOld, bOld}, Kind: a, DryRun: true}); err != nil || !slices.Equal(got, []uuid.UUID{aOld}) {
		t.Errorf("ids of both kinds, kind a: %v, %v, want %s", got, err, aOld)
	}
	// ids and age: the fresh one named by id is too young.
	if got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{IDs: []uuid.UUID{aOld, aFresh}, OlderThan: 24 * time.Hour, DryRun: true}); err != nil || !slices.Equal(got, []uuid.UUID{aOld}) {
		t.Errorf("ids, 24h: %v, %v, want %s", got, err, aOld)
	}
	// An age longer than any of them: nothing.
	if got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{Kind: a, OlderThan: 72 * time.Hour}); err != nil || len(got) != 0 {
		t.Errorf("kind a, 72h: %v, %v, want none", got, err)
	}
	if !exists(t, pool, aOld) || !exists(t, pool, aFresh) || !exists(t, pool, bOld) {
		t.Fatal("setup or the 72h run deleted something")
	}
	// The real run with kind and age deletes exactly the old job of kind a.
	got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{Kind: a, OlderThan: 24 * time.Hour})
	if err != nil || !slices.Equal(got, []uuid.UUID{aOld}) {
		t.Fatalf("kind a, 24h: %v, %v", got, err)
	}
	if exists(t, pool, aOld) || !exists(t, pool, aFresh) || !exists(t, pool, bOld) {
		t.Errorf("after: aOld exists %v, aFresh exists %v, bOld exists %v, want false true true",
			exists(t, pool, aOld), exists(t, pool, aFresh), exists(t, pool, bOld))
	}
}

// With only an age, the jobs that died long enough ago are deleted wherever they are, and a job
// that died recently is not. (A very old age, so that other tests' dead jobs are not caught.)
func TestDeleteDead_ByAgeAlone(t *testing.T) {
	pool := testutil.WorkerPool(t)
	old, fresh := makeDead(t, pool, newKind("del-age-old")), makeDead(t, pool, newKind("del-age-fresh"))
	diedAgo(t, pool, old, 400*24*time.Hour)

	got, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{OlderThan: 399 * 24 * time.Hour})
	if err != nil || !slices.Contains(got, old) || slices.Contains(got, fresh) {
		t.Fatalf("by age alone: %v, %v, want the 400-day-old job and not the fresh one", got, err)
	}
	if exists(t, pool, old) || !exists(t, pool, fresh) {
		t.Errorf("old exists %v, fresh exists %v, want false true", exists(t, pool, old), exists(t, pool, fresh))
	}
}

// The dry run reports exactly what the real run then deletes, and deletes nothing itself.
func TestDeleteDead_DryRunMatchesTheRealRunAndDeletesNothing(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("del-dry")
	var all []uuid.UUID
	for range 5 {
		all = append(all, makeDead(t, pool, kind))
	}
	diedAgo(t, pool, all[0], 72*time.Hour)
	diedAgo(t, pool, all[1], 30*time.Hour)

	// In this order, not a map's: "by ids" runs for real and deletes two of the jobs, so the
	// cases after it would find them gone.
	for _, c := range []struct {
		name string
		p    queue.DeleteDeadParams
	}{
		{"by kind", queue.DeleteDeadParams{Kind: kind}},
		{"kind and age", queue.DeleteDeadParams{Kind: kind, OlderThan: 24 * time.Hour}},
		{"by ids", queue.DeleteDeadParams{IDs: []uuid.UUID{all[0], all[3]}}},
	} {
		name, p := c.name, c.p
		dry := p
		dry.DryRun = true
		before, err := queue.DeleteDead(ctx, pool, dry)
		if err != nil {
			t.Fatalf("%s: dry run: %v", name, err)
		}
		for _, id := range all {
			if !exists(t, pool, id) {
				t.Fatalf("%s: the dry run deleted %s", name, id)
			}
		}
		if len(before) == 0 {
			t.Fatalf("%s: the dry run found nothing; the case is empty", name)
		}
		if !slices.Equal(before, ascending(before...)) {
			t.Errorf("%s: the dry run's ids are not in ascending order: %v", name, before)
		}
		// Run it for real only for "by ids", so the other cases still have their jobs; the
		// remaining ones are compared with a dry run below.
		if name == "by ids" {
			real, err := queue.DeleteDead(ctx, pool, p)
			if err != nil || !slices.Equal(real, before) {
				t.Errorf("%s: real run %v, %v; dry run said %v", name, real, err, before)
			}
		}
	}
	// What is left of the kind: the dry run and the real run agree.
	dry, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{Kind: kind, DryRun: true})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(err)
	real, err := queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{Kind: kind})
	must(err)
	if !slices.Equal(dry, real) || len(real) != 3 {
		t.Errorf("dry run %v, real run %v, want the same three", dry, real)
	}
}

// A job that a requeue is moving back to the queue at that moment is not deleted: the delete
// waits for the requeue's lock, then sees the job is no longer dead. A raw transaction plays
// the requeue (the same UPDATE, held open).
func TestDeleteDead_ADeadJobBeingRequeuedIsNotDeleted(t *testing.T) {
	pool := testutil.WorkerPool(t)
	id := makeDead(t, pool, newKind("del-race"))

	requeuer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer requeuer.Rollback(ctx) //nolint:errcheck // cleanup
	if _, err := requeuer.Exec(ctx, `UPDATE jobs SET state = 'queued', attempts = 0, finished_at = NULL, run_at = now() WHERE id = $1 AND state = 'dead'`, id); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var got []uuid.UUID
	var derr error
	finished := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		got, derr = queue.DeleteDead(ctx, pool, queue.DeleteDeadParams{IDs: []uuid.UUID{id}})
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatalf("the delete finished (%v, %v) while the requeue held the job: it must wait for the lock", got, derr)
	case <-time.After(300 * time.Millisecond):
	}
	if err := requeuer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if derr != nil || len(got) != 0 {
		t.Fatalf("DeleteDead = %v, %v, want nothing deleted (the job was requeued)", got, derr)
	}
	if r := getJob(t, pool, id); r.State != queue.StateQueued {
		t.Fatalf("the job is %q after the race, want queued", r.State)
	}
}
