package benchwork

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/testutil"
)

// effectCounts is what would reveal a broken completion transaction, so it is tested
// against rows constructed to have exactly the defects it must find; in a correct
// system there are none, and a run alone could never show the check works.
func TestEffectCounts_FindsDuplicatesAndMissingEffects(t *testing.T) {
	ctx := context.Background()
	pool, owner := testutil.WorkerPool(t), testutil.OwnerPool(t)
	if err := Setup(ctx, owner); err != nil {
		t.Fatal(err)
	}
	run := "effectcounts-" + uuid.NewString()
	t.Cleanup(func() { _ = cleanup(ctx, owner, run) })

	job := func(state string) uuid.UUID {
		var id uuid.UUID
		err := pool.QueryRow(ctx, `
			INSERT INTO jobs (kind, payload, state, attempts, max_attempts, run_at, finished_at)
			VALUES ($1, jsonb_build_object('run', $2::text), $3, 1, CASE WHEN $3 = 'dead' THEN 1 ELSE 5 END, now(), now())
			RETURNING id`, Kind, run, state).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	effects := func(id uuid.UUID, n int) {
		for range n {
			if _, err := pool.Exec(ctx, `INSERT INTO bench.effects (job_id, attempt, worker) VALUES ($1, 1, 'w')`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	effects(job("succeeded"), 3) // two beyond the first
	effects(job("succeeded"), 1) // correct
	job("succeeded")             // succeeded with no effect
	job("dead")                  // dead with no effect is correct

	dup, missing, err := effectCounts(ctx, pool, run)
	if err != nil {
		t.Fatal(err)
	}
	if dup != 2 || missing != 1 {
		t.Fatalf("duplicates %d, missing %d; want 2 and 1", dup, missing)
	}
}
