package settlements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/payclient"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// The reconcile schedule. All placeholders, not tuned: any result that depends on how
// long an `unknown` settlement takes to resolve is measuring these constants, and must
// say so.
//
// The worker's own retry backoff is global and ends in `dead` after a few attempts,
// which does not fit waiting minutes for a provider, so reconcile keeps its own
// schedule: when a check finds no answer it enqueues the next check itself.
const (
	// MaxReconcileChecks is how many checks are made before giving up. With the
	// delays below the last one is about 35 minutes after the first.
	MaxReconcileChecks = 12
	// MaxReconcileDelay caps the wait between two checks.
	MaxReconcileDelay = 5 * time.Minute
)

// NextReconcileDelay is the wait after `done` checks that found no answer: the first
// delay doubled each time (FirstReconcileDelay * 2^done), capped at MaxReconcileDelay.
func NextReconcileDelay(done int) time.Duration {
	d := FirstReconcileDelay
	for i := 0; i < done; i++ {
		d *= 2
		if d >= MaxReconcileDelay {
			return MaxReconcileDelay
		}
	}
	return d
}

// Reader is the part of the payment provider that reconcile needs.
type Reader interface {
	Get(ctx context.Context, key string) (payclient.Payment, error)
}

// errNoAnswerYet is returned, wrapped, when the final check still gets no answer.
var errNoAnswerYet = errors.New("no answer from the provider")

// ReconcileHandler is the handler of KindReconcile: it asks the provider what became of
// an `unknown` settlement and records the answer.
//
// Phase one only reads (a GET by the settlement id), so a re-run is harmless. Phase two
// is in the job's completion transaction:
//
//	payment succeeded or failed -> Resolve: the ledger is posted on success
//	no answer (the provider has no payment for the key, or the read failed or was
//	unreadable)                  -> the next check is enqueued with its own delay, in the
//	                               same transaction, if the settlement is still unknown
//
// A missing payment is not treated as a failure: the original request may still be in
// flight at the provider. After MaxReconcileChecks checks without an answer the job
// returns an error, so it retries a few times (each retry reads again), then
// dead-letters with the reason: the settlement stays `unknown` and the dead job is the
// operator's signal (the dead-jobs gauge). `worker requeue` reads once more; if the
// payment has appeared by then, the settlement resolves.
//
// A settlement that is no longer `unknown` (a webhook got there first) is left alone.
// A rejected read (a 4xx) is our bug and fails the attempt.
func ReconcileHandler(pool *pgxpool.Pool, r Reader) worker.Handler {
	q := db.New(pool)
	return func(ctx context.Context, job queue.Claimed) (worker.Commit, error) {
		var p ReconcilePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil || p.SettlementID == uuid.Nil || p.Check < 1 {
			return nil, fmt.Errorf("settlement.reconcile: bad payload %q", job.Payload)
		}
		id := p.SettlementID

		s, err := q.GetSettlementForExecution(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("settlement.reconcile: settlement %s does not exist", id)
		}
		if err != nil {
			return nil, fmt.Errorf("read settlement: %w", err)
		}
		if Status(s.Status) != Unknown {
			return nil, nil // resolved meanwhile, or not ambiguous: nothing to find out
		}

		pay, err := r.Get(ctx, id.String())
		switch {
		case err == nil:
			return resolveCommit(id, outcomeOf(pay)), nil
		case errors.Is(err, payclient.ErrNotFound), errors.Is(err, payclient.ErrAmbiguous):
			if p.Check >= MaxReconcileChecks {
				return nil, fmt.Errorf("settlement.reconcile %s: gave up after %d checks, the settlement stays unknown: %w", id, p.Check, errNoAnswerYet)
			}
			return nextCheckCommit(id, p.Check), nil
		default:
			return nil, fmt.Errorf("settlement.reconcile %s: %w", id, err)
		}
	}
}

// nextCheckCommit enqueues the check after `done` — unless the settlement has left
// `unknown` since the handler read it, which the row lock settles.
func nextCheckCommit(id uuid.UUID, done int) worker.Commit {
	return func(ctx context.Context, tx pgx.Tx) error {
		status, err := db.New(tx).LockSettlementStatus(ctx, id)
		if err != nil {
			return fmt.Errorf("lock settlement: %w", err)
		}
		if Status(status) != Unknown {
			return nil
		}
		_, err = queue.Enqueue(ctx, tx, queue.NewJob{
			Kind:    KindReconcile,
			Payload: ReconcilePayload{SettlementID: id, Check: done + 1},
			RunAt:   time.Now().Add(NextReconcileDelay(done)),
		})
		if err != nil {
			return fmt.Errorf("enqueue next check: %w", err)
		}
		return nil
	}
}
