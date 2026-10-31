package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// MinRetention is the shortest retention PruneFinished accepts. It is a guard, not
// a tuning knob: a zero or tiny value from a mistyped config would delete every
// finished job, including ones whose results another process is about to read, and
// periodic jobs' dedupe keys need the previous period's row to still exist.
const MinRetention = time.Hour

// PruneFinished deletes up to batch succeeded jobs that finished more than
// retention ago (by the database's clock), oldest first, and returns how many. Call
// it until it returns 0 to remove a backlog in short statements. Dead jobs are kept.
func PruneFinished(ctx context.Context, pool *pgxpool.Pool, retention time.Duration, batch int) (int64, error) {
	if retention < MinRetention {
		return 0, fmt.Errorf("%w: retention must be at least %v", ErrInvalidJob, MinRetention)
	}
	if batch < 1 {
		return 0, fmt.Errorf("%w: prune batch must be at least 1", ErrInvalidJob)
	}
	n, err := db.New(pool).DeleteFinishedJobs(ctx, db.DeleteFinishedJobsParams{
		RetentionSeconds: retention.Seconds(), Batch: int32(batch),
	})
	if err != nil {
		return 0, fmt.Errorf("prune finished jobs: %w", err)
	}
	return n, nil
}
