package receiptparse

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// KindSweep is the periodic job that gives up on receipts whose parse job died.
const KindSweep = "receipt.sweep"

// SweepSchedule is how often the sweep runs.
var SweepSchedule = queue.PeriodicJob{Kind: KindSweep, Every: 5 * time.Minute, MaxAttempts: 3}

// SweepConfig tunes the sweep. Zero values take the defaults noted; both are placeholders.
type SweepConfig struct {
	// StuckAfter: a receipt is only looked at once it is older than this. A parse job
	// that is queued or running is never swept, however old; this only spares a receipt
	// whose job has just been written and not yet seen. Default 15 minutes.
	StuckAfter time.Duration
	// Batch is how many receipts one pass takes. Default 100.
	Batch int
	// Group, if set, restricts the sweep to one group (so tests do not sweep each
	// other's receipts).
	Group *uuid.UUID
}

func (c *SweepConfig) setDefaults() {
	if c.StuckAfter == 0 {
		c.StuckAfter = 15 * time.Minute
	}
	if c.Batch == 0 {
		c.Batch = 100
	}
}

// SweepHandler is the handler of KindSweep.
//
// Why it exists: the queue dead-letters a job that runs out of attempts, and knows
// nothing about the expense the job was for. A receipt whose parse job died would leave
// its expense in `parsing` for ever, and `parsing` has no way out for the payer (no
// edit, no confirm). The sweep finds receipts still waiting with no live parse job and
// moves their expense to needs_review with no items, reason parse_abandoned, so the
// payer can type the receipt in. It does not retry the parse: a job that died has
// already been tried several times.
//
// Each pass is one transaction that locks the expenses it takes (SKIP LOCKED); the
// status change is conditional on `parsing`, so a parse job committing at the same
// moment either wins (the sweep then changes nothing for that receipt) or loses (its
// commit is a no-op).
func SweepHandler(pool *pgxpool.Pool, cfg SweepConfig) worker.Handler {
	cfg.setDefaults()
	return func(ctx context.Context, _ queue.Claimed) (worker.Commit, error) {
		for range 10 { // a full batch may mean more waiting; the next period continues
			n, err := sweepOnce(ctx, pool, cfg)
			if err != nil {
				return nil, err
			}
			if n < cfg.Batch {
				break
			}
		}
		return nil, nil
	}
}

// sweepOnce abandons the stuck receipts it can lock and returns how many it found.
func sweepOnce(ctx context.Context, pool *pgxpool.Pool, cfg SweepConfig) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	q := db.New(tx)

	var group uuid.UUID // the zero id means "every group" in the query
	if cfg.Group != nil {
		group = *cfg.Group
	}
	rows, err := q.ListStuckReceipts(ctx, db.ListStuckReceiptsParams{
		StuckSecs: cfg.StuckAfter.Seconds(), GroupID: group, Batch: int32(cfg.Batch),
	})
	if err != nil {
		return 0, fmt.Errorf("list stuck receipts: %w", err)
	}
	for _, r := range rows {
		res := result{status: receipts.StatusNeedsReview, reasons: []string{ReasonAbandoned}, source: "sweep"}
		if _, err := apply(ctx, tx, r.ID, r.ExpenseID, r.GroupID, r.UploaderID, res); err != nil {
			return 0, fmt.Errorf("abandon receipt %s: %w", r.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(rows), nil
}
