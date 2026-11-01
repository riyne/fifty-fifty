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

// KindReconcile is the job kind that asks the provider what became of an `unknown`
// settlement (step 4.5).
const KindReconcile = "settlement.reconcile"

// ReconcilePayload is the payload of a KindReconcile job. Check counts the reconcile
// attempts so far; the reconcile handler uses it to schedule its own next check and to
// know when its deadline has passed.
type ReconcilePayload struct {
	SettlementID uuid.UUID `json:"settlement_id"`
	Check        int       `json:"check"`
}

// FirstReconcileDelay is how long after an ambiguous answer the first reconcile runs.
// A placeholder, not tuned; results that depend on it must say so.
const FirstReconcileDelay = 5 * time.Second

// Provider is the part of the payment provider that execute needs.
type Provider interface {
	Create(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error)
}

// ExecuteHandler is the handler of KindExecute: it sends a pending settlement to the
// provider and records what came back.
//
// It is a two-phase handler (see worker.Handler). Phase one, the provider call, runs
// with no transaction open. It may run more than once for one settlement (a crash
// after the call, a lost lease), so the call is keyed by the settlement id: the provider
// returns the payment it already has and charges nothing. Phase two, the returned
// Commit, runs inside the transaction that also marks the job succeeded, so the
// settlement's new status and, on success, the ledger posting commit together with the
// job or not at all; a worker whose lease was taken over has that transaction rolled back.
//
// Outcomes of the call:
//
//	payment succeeded     -> Resolve(success): the ledger is posted
//	payment failed        -> Resolve(failure): nothing is posted
//	ambiguous (timeout, dropped connection, 5xx, unreadable answer)
//	                      -> Resolve(unknown) and a reconcile job, in the same transaction.
//	                         Execute never sends again: a 5xx can come after the provider
//	                         took the payment, and only a read can tell.
//	rejected (a 4xx)      -> an error: our request is wrong, so the job retries, then is
//	                         dead-lettered where an operator sees it. The settlement
//	                         stays pending.
//
// A settlement that is no longer pending (resolved by a webhook, or already unknown) is
// left alone and the job succeeds without calling the provider.
func ExecuteHandler(pool *pgxpool.Pool, p Provider) worker.Handler {
	q := db.New(pool)
	return func(ctx context.Context, job queue.Claimed) (worker.Commit, error) {
		var payload ExecutePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.SettlementID == uuid.Nil {
			return nil, fmt.Errorf("settlement.execute: bad payload %q", job.Payload)
		}
		id := payload.SettlementID

		s, err := q.GetSettlementForExecution(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("settlement.execute: settlement %s does not exist", id)
		}
		if err != nil {
			return nil, fmt.Errorf("read settlement: %w", err)
		}
		if Status(s.Status) != Pending {
			return nil, nil // someone already moved it; nothing to send, nothing to write
		}

		pay, err := p.Create(ctx, id.String(), payclient.Request{
			Reference: id.String(), From: s.FromMember.String(), To: s.ToMember.String(),
			AmountMinor: s.AmountMinor, Currency: s.Currency,
		})
		switch {
		case err == nil:
			return resolveCommit(id, outcomeOf(pay)), nil
		case errors.Is(err, payclient.ErrAmbiguous):
			return ambiguousCommit(id), nil
		default:
			// A rejected request, or our own context ending (lease lost, shutdown): not a
			// payment outcome. The worker decides what to do with the error.
			return nil, fmt.Errorf("settlement.execute %s: %w", id, err)
		}
	}
}

// outcomeOf maps a final provider payment to the outcome Resolve takes.
func outcomeOf(p payclient.Payment) Outcome {
	if p.Status == payclient.StatusSucceeded {
		return Success()
	}
	code := "declined" // the provider gave no reason
	if p.FailureCode != nil && *p.FailureCode != "" {
		code = *p.FailureCode
		if r := []rune(code); len(r) > 100 {
			code = string(r[:100])
		}
	}
	return Failure(code)
}

func resolveCommit(id uuid.UUID, o Outcome) worker.Commit {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := Resolve(ctx, tx, id, o)
		return err
	}
}

// ambiguousCommit records "unknown" and, only if this call is what moved the
// settlement there, schedules the first reconcile. If a webhook resolved the settlement
// while the call was in flight, Resolve ignores the outcome and nothing is scheduled.
func ambiguousCommit(id uuid.UUID) worker.Commit {
	return func(ctx context.Context, tx pgx.Tx) error {
		res, err := Resolve(ctx, tx, id, AmbiguousResult())
		if err != nil {
			return err
		}
		if res.Action != Apply {
			return nil
		}
		_, err = queue.Enqueue(ctx, tx, queue.NewJob{
			Kind:    KindReconcile,
			Payload: ReconcilePayload{SettlementID: id, Check: 1},
			RunAt:   time.Now().Add(FirstReconcileDelay),
		})
		if err != nil {
			return fmt.Errorf("enqueue reconcile: %w", err)
		}
		return nil
	}
}
