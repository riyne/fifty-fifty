package queue

import (
	"context"
	"fmt"

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
