package settlements

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// The window the re-check closes, forced: this sweep has listed (and locked) a stuck
// settlement; a concurrent sweep then commits an execute job for it, which this sweep's
// listing could not see; this sweep must notice and not enqueue a second one. Without
// the re-check it does, and the settlement gets two jobs.
func TestSweep_ARivalsJobCommittedAfterTheListingIsNotDuplicated(t *testing.T) {
	// pool (the owner) seeds and inspects; the sweep and the rival's commit run as the worker.
	pool, worker := testutil.SharedOwnerPool(t), testutil.WorkerPool(t)
	ctx := context.Background()

	var from, to, group, id uuid.UUID
	for _, u := range []*uuid.UUID{&from, &to} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (issuer, subject) VALUES ('test', $1) RETURNING id`, testutil.UniqueSubject("wd")).Scan(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ('g', 'CAD', $1) RETURNING id`, from).Scan(&group); err != nil {
		t.Fatal(err)
	}
	for _, m := range []uuid.UUID{from, to} {
		if _, err := pool.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, group, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, created_at)
	    VALUES ($1, $2, $3, 100, now() - interval '1 hour') RETURNING id`, group, from, to).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE payload->>'settlement_id' = $1`, id.String()) })

	rival := func() {
		tx, err := worker.Begin(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: KindExecute, Payload: ExecutePayload{SettlementID: id}}); err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
	afterList = rival
	t.Cleanup(func() { afterList = nil })

	n, err := sweep(ctx, worker, WatchdogConfig{StuckAfter: time.Minute, MaxExecuteJobs: 3, Batch: 5, Group: &group})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the sweep listed %d settlements, want 1", n)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = 'settlement.execute' AND payload->>'settlement_id' = $1`, id.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Errorf("the settlement has %d execute jobs, want 1 (the rival's)", jobs)
	}
}
