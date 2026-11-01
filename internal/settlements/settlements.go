// Package settlements holds the settlement state machine. Everything that learns
// how a payment went (the execute job, the reconcile job, a provider webhook) calls
// Resolve, so there is exactly one place that moves a settlement forward and posts
// it to the ledger.
//
// Settlement is simulated: the "provider" is the mock payment server.
package settlements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/riyne/fifty-fifty/internal/controls"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/ledger"
)

// Status is a settlement's state. Values match the CHECK in migration 00011.
type Status string

const (
	Pending   Status = "pending"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Unknown   Status = "unknown" // the provider's answer was ambiguous; reconcile will find out
)

// Terminal reports whether nothing may leave this status.
func (s Status) Terminal() bool { return s == Succeeded || s == Failed }

// Kind is what a resolver learned about the payment.
type Kind string

const (
	KindSucceeded Kind = "succeeded"
	KindFailed    Kind = "failed"
	KindUnknown   Kind = "unknown"
)

// Outcome is one resolver's finding. Build it with the three constructors.
type Outcome struct {
	Kind Kind
	// FailureCode is the provider's decline reason; required for KindFailed.
	FailureCode string
}

func Success() Outcome            { return Outcome{Kind: KindSucceeded} }
func Failure(code string) Outcome { return Outcome{Kind: KindFailed, FailureCode: code} }
func AmbiguousResult() Outcome    { return Outcome{Kind: KindUnknown} }
func (o Outcome) status() Status  { return Status(o.Kind) }
func (o Outcome) validate() (err error) {
	switch o.Kind {
	case KindSucceeded, KindUnknown:
		if o.FailureCode != "" {
			err = errors.New("failure code on a non-failure outcome")
		}
	case KindFailed:
		if n := len([]rune(o.FailureCode)); n < 1 || n > 100 {
			err = errors.New("failure outcome needs a failure code of 1-100 characters")
		}
	default:
		err = fmt.Errorf("unknown outcome kind %q", o.Kind)
	}
	return err
}

// Action is what Resolve does with an outcome, given the settlement's status.
type Action int

const (
	// Apply moves the settlement to the outcome's status.
	Apply Action = iota
	// Ignore changes nothing: a repeat of where the settlement already is, or an
	// "unknown" arriving after the settlement was resolved.
	Ignore
	// Conflict means the outcome contradicts a terminal status (success after
	// failure, failure after success). Nothing is changed; Resolve records an audit
	// event so a person can look.
	Conflict
)

func (a Action) String() string {
	return [...]string{"apply", "ignore", "conflict"}[a]
}

// Transition is the whole state machine:
//
//	pending -> succeeded | failed | unknown
//	unknown -> succeeded | failed
//	succeeded, failed: terminal
//
// A repeat of the current status is Ignore, so a duplicated webhook or a retried
// job is harmless. An "unknown" after the settlement has left pending is Ignore
// too (it carries no information the settlement does not already have). Only a
// terminal status contradicted by the opposite terminal outcome is a Conflict.
func Transition(current Status, k Kind) Action {
	next := Status(k)
	switch {
	case current == next:
		return Ignore
	case current.Terminal() && next.Terminal():
		return Conflict
	case current.Terminal():
		// Only "unknown" is left: it adds nothing to a resolved settlement.
		return Ignore
	default:
		// pending -> anything, unknown -> succeeded | failed.
		return Apply
	}
}

// ErrNotFound means no settlement has this id.
var ErrNotFound = errors.New("settlements: not found")

// Result reports what Resolve did.
type Result struct {
	Action Action
	// Status is the settlement's status after the call.
	Status Status
	// LedgerTransactionID is set only when this call posted to the ledger.
	LedgerTransactionID uuid.UUID
}

// Resolve applies one outcome to settlement id inside tx and commits nothing: the
// caller's transaction (the job's completion transaction, or the webhook's) decides.
//
// The order matters. First the settlement row is locked FOR UPDATE, so concurrent
// resolvers queue up and each sees the previous one's result. Then Transition
// decides. Only an Apply writes anything, and a success writes the ledger posting
// and marks the settlement succeeded in the same transaction: either both exist or
// neither does. If the lock were missing, the unique index on the settlement's
// posting would still refuse a second posting.
func Resolve(ctx context.Context, tx pgx.Tx, id uuid.UUID, o Outcome) (Result, error) {
	if err := o.validate(); err != nil {
		return Result{}, fmt.Errorf("settlements: invalid outcome: %w", err)
	}
	q := db.New(tx)
	s, err := controls.LockSettlement(ctx, q, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("lock settlement: %w", err)
	}
	current := Status(s.Status)

	action := Transition(current, o.Kind)
	switch action {
	case Ignore:
		return Result{Action: Ignore, Status: current}, nil
	case Conflict:
		if err := audit(ctx, q, s, "settlement.conflict", map[string]any{
			"settlement_id": s.ID, "status": current, "attempted": o.Kind,
		}); err != nil {
			return Result{}, err
		}
		return Result{Action: Conflict, Status: current}, nil
	}

	res := Result{Action: Apply, Status: o.status()}
	data := map[string]any{"settlement_id": s.ID, "from_status": current}
	switch o.Kind {
	case KindSucceeded:
		entries, err := ledger.SettlementEntries(s.FromMember, s.ToMember, s.AmountMinor)
		if err != nil {
			return Result{}, fmt.Errorf("ledger entries: %w", err)
		}
		txID, err := q.CreateSettlementTransaction(ctx, db.CreateSettlementTransactionParams{
			GroupID: s.GroupID, SettlementID: s.ID, CreatedBy: s.FromMember,
		})
		if err != nil {
			return Result{}, fmt.Errorf("create ledger transaction: %w", err)
		}
		p := db.CreateLedgerEntriesParams{TransactionID: txID, GroupID: s.GroupID}
		for _, e := range entries {
			p.MemberIds = append(p.MemberIds, e.MemberID)
			p.Amounts = append(p.Amounts, e.Amount)
		}
		if err := q.CreateLedgerEntries(ctx, p); err != nil {
			return Result{}, fmt.Errorf("create ledger entries: %w", err)
		}
		if err := mustMoveOne(controls.MarkSettlementSucceeded(ctx, q, db.MarkSettlementSucceededParams{ID: s.ID, LedgerTransactionID: pgtype.UUID{Bytes: txID, Valid: true}})); err != nil {
			return Result{}, fmt.Errorf("mark succeeded: %w", err)
		}
		res.LedgerTransactionID = txID
		data["ledger_transaction_id"] = txID
		data["amount_minor"] = s.AmountMinor
	case KindFailed:
		if err := mustMoveOne(controls.MarkSettlementFailed(ctx, q, db.MarkSettlementFailedParams{ID: s.ID, FailureCode: pgtype.Text{String: o.FailureCode, Valid: true}})); err != nil {
			return Result{}, fmt.Errorf("mark failed: %w", err)
		}
		data["failure_code"] = o.FailureCode
	case KindUnknown:
		if err := mustMoveOne(controls.MarkSettlementUnknown(ctx, q, s.ID)); err != nil {
			return Result{}, fmt.Errorf("mark unknown: %w", err)
		}
	}
	if err := audit(ctx, q, s, "settlement."+string(o.Kind), data); err != nil {
		return Result{}, err
	}
	return res, nil
}

// mustMoveOne turns "the guarded UPDATE touched no row" into an error. Under the
// row lock that cannot happen; if it ever does, the transaction must not commit.
func mustMoveOne(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("settlement was not in a state that allows this change (rows=%d)", n)
	}
	return nil
}

// audit records the event in the settlement's group, attributed to the sender
// (the settlement's creator). It holds ids and amounts, never provider payloads.
func audit(ctx context.Context, q *db.Queries, s db.LockSettlementRow, kind string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("audit data: %w", err)
	}
	if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorUserID: s.FromMember, GroupID: s.GroupID, Kind: kind, Data: b,
	}); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}
