package settlements

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// KindWatchdog is the periodic job that finds settlements whose execute job was lost.
const KindWatchdog = "settlement.watchdog"

// WatchdogSchedule is how often the watchdog runs; the worker makes sure one such job
// exists per period (queue.EnqueuePeriodic).
var WatchdogSchedule = queue.PeriodicJob{Kind: KindWatchdog, Every: time.Minute, MaxAttempts: 3}

// WatchdogConfig tunes the sweep. Zero values take the defaults noted; all are
// placeholders, not tuned.
type WatchdogConfig struct {
	// StuckAfter: a pending settlement is only looked at if it is older than this and
	// no execute job for it was created within it. Default 10 minutes.
	StuckAfter time.Duration
	// MaxExecuteJobs: stop replacing execute jobs once this many exist for a
	// settlement. Default 3: a settlement whose jobs keep dying has a bug that
	// more attempts will not fix, and is left for an operator.
	MaxExecuteJobs int
	// Batch is how many settlements one sweep statement takes. Default 100.
	Batch int
	// Group, if set, restricts the sweep to one group (for an operator who wants to
	// look at one group, and so tests do not sweep each other's settlements).
	Group *uuid.UUID
}

func (c *WatchdogConfig) setDefaults() {
	if c.StuckAfter == 0 {
		c.StuckAfter = 10 * time.Minute
	}
	if c.MaxExecuteJobs == 0 {
		c.MaxExecuteJobs = 3
	}
	if c.Batch == 0 {
		c.Batch = 100
	}
}

// WatchdogHandler is the handler of KindWatchdog.
//
// Why it exists: a job that exhausts its attempts is moved to `dead` by the queue,
// which knows nothing about the row the job was for. If that row is a settlement,
// it stays `pending` and keeps its reservation, with nothing that will ever look at it
// again. The watchdog finds pending settlements with no live execute job and enqueues a
// fresh one. That is safe because the provider call is keyed by the settlement id: if
// the earlier job did reach the provider, the new one gets the same payment back.
//
// It is bounded twice over (a job that just died is not replaced for StuckAfter, and at
// most MaxExecuteJobs are ever created per settlement), so a settlement that can never
// execute does not loop. It does not touch `unknown` settlements: reconcile owns them.
//
// Each sweep runs in one transaction that locks the settlements it takes (SKIP LOCKED),
// so a sweep that overlaps another takes different rows.
func WatchdogHandler(pool *pgxpool.Pool, cfg WatchdogConfig) worker.Handler {
	cfg.setDefaults()
	return func(ctx context.Context, _ queue.Claimed) (worker.Commit, error) {
		// A full batch may mean more waiting; a handful of rounds per run is plenty and
		// bounds the run. The next period continues.
		for range 10 {
			n, err := sweep(ctx, pool, cfg)
			if err != nil {
				return nil, err
			}
			if n < cfg.Batch {
				break
			}
		}
		return nil, nil // all effects are the sweeps' own transactions
	}
}

func groupBytes(g *uuid.UUID) [16]byte {
	if g == nil {
		return [16]byte{}
	}
	return *g
}

// afterList, when set (by an internal test), runs between a sweep's listing and its
// re-check. It exists so a test can commit another sweep's job in exactly the window
// the re-check is for, instead of hoping concurrent sweeps hit it.
var afterList func()

// sweep enqueues a fresh execute job for each stuck settlement it can lock, and returns
// how many settlements it found.
func sweep(ctx context.Context, pool *pgxpool.Pool, cfg WatchdogConfig) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	q := db.New(tx)

	rows, err := q.ListStuckSettlements(ctx, db.ListStuckSettlementsParams{
		StuckSecs: cfg.StuckAfter.Seconds(), MaxJobs: int32(cfg.MaxExecuteJobs), Batch: int32(cfg.Batch),
		GroupID: pgtype.UUID{Bytes: groupBytes(cfg.Group), Valid: cfg.Group != nil},
	})
	if err != nil {
		return 0, fmt.Errorf("list stuck settlements: %w", err)
	}
	if afterList != nil {
		afterList()
	}
	for _, s := range rows {
		// Re-ask under the lock, in a new statement (see ListStuckSettlements): the
		// listing's snapshot may predate another sweep's committed job.
		stuck, err := q.IsSettlementStillStuck(ctx, db.IsSettlementStillStuckParams{
			ID: s.ID, StuckSecs: cfg.StuckAfter.Seconds(), MaxJobs: int32(cfg.MaxExecuteJobs),
		})
		if err != nil {
			return 0, fmt.Errorf("re-check %s: %w", s.ID, err)
		}
		if !stuck {
			continue
		}
		if _, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: KindExecute, Payload: ExecutePayload{SettlementID: s.ID}}); err != nil {
			return 0, fmt.Errorf("enqueue execute for %s: %w", s.ID, err)
		}
		data, _ := json.Marshal(map[string]any{"settlement_id": s.ID, "earlier_execute_jobs": s.ExecuteJobs})
		if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
			ActorUserID: s.FromMember, GroupID: s.GroupID, Kind: "settlement.execute_requeued", Data: data,
		}); err != nil {
			return 0, fmt.Errorf("audit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return len(rows), nil
}
