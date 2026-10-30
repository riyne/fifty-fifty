package worker

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/queue"
)

// KindIdempotencyCleanup deletes expired Idempotency-Key rows.
const KindIdempotencyCleanup = "idempotency.cleanup"

// cleanupBatch bounds one DELETE, so a large backlog is removed in short
// statements rather than one long one that holds locks and bloats WAL.
const cleanupBatch = 1000

// IdempotencyCleanup is the handler for KindIdempotencyCleanup. Expired keys are
// already ignored by every reader; this reclaims their space. Deleting is
// idempotent, so the work happens in phase one on the pool and there is nothing
// to commit with the job: a retry after a crash just finds less to delete.
func IdempotencyCleanup(pool *pgxpool.Pool) Handler {
	return func(ctx context.Context, _ queue.Claimed) (Commit, error) {
		_, err := deleteAllExpired(ctx, pool, cleanupBatch)
		return nil, err
	}
}

// deleteAllExpired deletes expired keys batch by batch until a batch deletes
// nothing, and returns the total it deleted. It does not stop at the first short
// batch: another cleaner running at the same moment (two workers, or an
// overlapping run) can delete some of the rows this batch selected, making it
// short while work remains. One extra, index-only query at the end is the price.
// It also stops if ctx is canceled (for example, the lease was lost); the next run
// continues.
func deleteAllExpired(ctx context.Context, pool *pgxpool.Pool, batch int) (int64, error) {
	var total int64
	for {
		n, err := idempotency.DeleteExpired(ctx, pool, batch)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

// CleanupSchedule is how often the cleanup runs. Keys live 24 h, so hourly keeps
// the table within about 25 h of keys.
var CleanupSchedule = queue.PeriodicJob{Kind: KindIdempotencyCleanup, Every: time.Hour, MaxAttempts: 3}
