package queue

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// RequeueParams selects which dead jobs to give another chance. At least one
// field must be set; there is deliberately no "everything" form, so a mistyped
// command cannot resurrect the whole dead-letter queue.
type RequeueParams struct {
	IDs  []uuid.UUID // these jobs
	Kind string      // every dead job of this kind
	// With both set, a job must match both.
}

// Requeue moves matching dead jobs back to queued with a fresh set of attempts,
// due now, and returns their ids. Only dead jobs are touched, so repeating it is
// harmless. attempts restarts at 0, so the history of earlier attempts is lost;
// last_error stays as the record of why the job died.
func Requeue(ctx context.Context, pool *pgxpool.Pool, p RequeueParams) ([]uuid.UUID, error) {
	if len(p.IDs) == 0 && p.Kind == "" {
		return nil, fmt.Errorf("%w: requeue needs job ids or a kind", ErrInvalidJob)
	}
	ids := p.IDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	out, err := db.New(pool).RequeueDeadJobs(ctx, db.RequeueDeadJobsParams{Ids: ids, Kind: p.Kind})
	if err != nil {
		return nil, fmt.Errorf("requeue dead jobs: %w", err)
	}
	return out, nil
}

// DeleteDeadParams selects which dead jobs to delete for good. At least one field must be
// set: there is deliberately no "everything" form, so a mistyped command cannot empty the
// dead-letter queue, which is the record of what failed and why.
type DeleteDeadParams struct {
	IDs       []uuid.UUID   // these jobs
	Kind      string        // every dead job of this kind
	OlderThan time.Duration // dead for at least this long (measured on the database clock)
	// A job must match every field that is set.

	// DryRun finds the jobs and deletes nothing.
	DryRun bool
}

// DeleteDead permanently deletes the dead jobs that match p and returns their ids in
// ascending order. Only dead jobs are touched, and the state is checked again on the locked row, so a job
// that a concurrent Requeue has just moved back to the queue is left alone. Deleting a dead job
// does not change what it was about: an expense still waiting to be read, a settlement still
// pending (the receipt sweep and the settlement watchdog deal with those); requeue instead if
// the work should still happen. With DryRun it only reports what a real run would delete.
func DeleteDead(ctx context.Context, pool *pgxpool.Pool, p DeleteDeadParams) ([]uuid.UUID, error) {
	if len(p.IDs) == 0 && p.Kind == "" && p.OlderThan <= 0 {
		return nil, fmt.Errorf("%w: deleting dead jobs needs job ids, a kind or an age", ErrInvalidJob)
	}
	if p.OlderThan < 0 {
		return nil, fmt.Errorf("%w: the age must not be negative", ErrInvalidJob)
	}
	ids := p.IDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	q := db.New(pool)
	var out []uuid.UUID
	var err error
	if p.DryRun {
		out, err = q.ListDeadJobsToDelete(ctx, db.ListDeadJobsToDeleteParams{Ids: ids, Kind: p.Kind, OlderThanSeconds: p.OlderThan.Seconds()})
	} else {
		out, err = q.DeleteDeadJobs(ctx, db.DeleteDeadJobsParams{Ids: ids, Kind: p.Kind, OlderThanSeconds: p.OlderThan.Seconds()})
		// DELETE ... RETURNING has no order; sorting gives the same order as the dry run.
		slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	}
	if err != nil {
		return nil, fmt.Errorf("delete dead jobs: %w", err)
	}
	return out, nil
}
