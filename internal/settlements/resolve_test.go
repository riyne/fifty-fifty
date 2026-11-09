package settlements_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/harness/testutil"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

var ctx = context.Background()

// world is one group with a sender (who owes), a payee and a bystander. Every test
// builds its own, so nothing depends on the shared database being empty. There is no
// create-settlement code yet (step 4.2), so settlements are inserted with SQL.
type world struct {
	// pool is the WORKER role (fifty_worker): what the code under test runs as (Resolve,
	// the handlers, the queue). owner seeds fixtures and inspects state; it sees every
	// row whatever policies apply to the worker.
	pool                *pgxpool.Pool
	owner               *pgxpool.Pool
	group               uuid.UUID
	from, to, bystander uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{pool: testutil.WorkerPool(t), owner: testutil.SharedOwnerPool(t)}
	for _, id := range []*uuid.UUID{&w.from, &w.to, &w.bystander} {
		must(t, w.owner.QueryRow(ctx,
			`INSERT INTO users (issuer, subject) VALUES ('test', $1) RETURNING id`,
			testutil.UniqueSubject("settle")).Scan(id))
	}
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO groups (name, currency, created_by) VALUES ('g', 'CAD', $1) RETURNING id`, w.from).Scan(&w.group))
	for i, m := range []uuid.UUID{w.from, w.to, w.bystander} {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		_, err := w.owner.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, $3)`, w.group, m, role)
		must(t, err)
	}
	return w
}

func (w *world) settlement(t *testing.T, amount int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, $4) RETURNING id`,
		w.group, w.from, w.to, amount).Scan(&id))
	return id
}

// resolve runs Resolve in its own transaction and commits.
func (w *world) resolve(t *testing.T, id uuid.UUID, o settlements.Outcome) settlements.Result {
	t.Helper()
	res, err := w.tryResolve(id, o)
	must(t, err)
	return res
}

func (w *world) tryResolve(id uuid.UUID, o settlements.Outcome) (settlements.Result, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return settlements.Result{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	res, err := settlements.Resolve(ctx, tx, id, o)
	if err != nil {
		return settlements.Result{}, err
	}
	return res, tx.Commit(ctx)
}

type row struct {
	status      string
	failureCode *string
	ledgerTx    *uuid.UUID
	resolved    bool
}

func (w *world) row(t *testing.T, id uuid.UUID) row {
	t.Helper()
	var r row
	must(t, w.owner.QueryRow(ctx,
		`SELECT status, failure_code, ledger_transaction_id, resolved_at IS NOT NULL FROM settlements WHERE id = $1`, id).
		Scan(&r.status, &r.failureCode, &r.ledgerTx, &r.resolved))
	return r
}

func (w *world) postings(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	must(t, w.owner.QueryRow(ctx,
		`SELECT count(*) FROM ledger_transactions WHERE settlement_id = $1`, id).Scan(&n))
	return n
}

func (w *world) audits(t *testing.T, kind string) int {
	t.Helper()
	var n int
	must(t, w.owner.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE group_id = $1 AND kind = $2`, w.group, kind).Scan(&n))
	return n
}

// balances recomputes every member's balance from the ledger entries.
func (w *world) balances(t *testing.T) map[uuid.UUID]int64 {
	t.Helper()
	rows, err := w.owner.Query(ctx,
		`SELECT member_id, SUM(amount_minor)::bigint FROM ledger_entries WHERE group_id = $1 GROUP BY member_id`, w.group)
	must(t, err)
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var m uuid.UUID
		var n int64
		must(t, rows.Scan(&m, &n))
		out[m] = n
	}
	must(t, rows.Err())
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolve_SuccessPostsOnceAndRepeatsAreNoOps(t *testing.T) {
	w := newWorld(t)
	id := w.settlement(t, 700)

	res := w.resolve(t, id, settlements.Success())
	if res.Action != settlements.Apply || res.Status != settlements.Succeeded || res.LedgerTransactionID == uuid.Nil {
		t.Fatalf("first success: %+v", res)
	}
	r := w.row(t, id)
	if r.status != "succeeded" || r.ledgerTx == nil || *r.ledgerTx != res.LedgerTransactionID || !r.resolved {
		t.Errorf("row after success: %+v", r)
	}
	// The sender owed money, so a payment raises their balance and lowers the payee's.
	if got := w.balances(t); len(got) != 2 || got[w.from] != 700 || got[w.to] != -700 {
		t.Errorf("balances = %v, want from +700 and to -700", got)
	}
	if n := w.audits(t, "settlement.succeeded"); n != 1 {
		t.Errorf("succeeded audit events = %d, want 1", n)
	}

	// A retried job, a duplicate webhook, a reconcile that finds the same answer.
	for range 3 {
		again := w.resolve(t, id, settlements.Success())
		if again.Action != settlements.Ignore || again.Status != settlements.Succeeded || again.LedgerTransactionID != uuid.Nil {
			t.Errorf("repeat: %+v", again)
		}
	}
	if n := w.postings(t, id); n != 1 {
		t.Errorf("postings = %d, want 1", n)
	}
	if got := w.balances(t); got[w.from] != 700 || got[w.to] != -700 {
		t.Errorf("balances changed by repeats: %v", got)
	}
	if n := w.audits(t, "settlement.succeeded"); n != 1 {
		t.Errorf("succeeded audit events after repeats = %d, want 1", n)
	}
}

func TestResolve_FailureIsFinalAndContradictionsAreRecordedNotApplied(t *testing.T) {
	w := newWorld(t)

	// failed, then a late success
	id := w.settlement(t, 500)
	if res := w.resolve(t, id, settlements.Failure("card_declined")); res.Action != settlements.Apply || res.Status != settlements.Failed {
		t.Fatalf("failure: %+v", res)
	}
	r := w.row(t, id)
	if r.status != "failed" || r.failureCode == nil || *r.failureCode != "card_declined" || r.ledgerTx != nil || !r.resolved {
		t.Errorf("row after failure: %+v", r)
	}
	res := w.resolve(t, id, settlements.Success())
	if res.Action != settlements.Conflict || res.Status != settlements.Failed {
		t.Errorf("success after failure: %+v", res)
	}
	if w.postings(t, id) != 0 || len(w.balances(t)) != 0 {
		t.Error("a success after a recorded failure posted to the ledger")
	}
	if w.row(t, id).status != "failed" {
		t.Error("status moved out of failed")
	}
	if n := w.audits(t, "settlement.conflict"); n != 1 {
		t.Errorf("conflict audit events = %d, want 1", n)
	}

	// succeeded, then a late failure
	id2 := w.settlement(t, 300)
	w.resolve(t, id2, settlements.Success())
	res = w.resolve(t, id2, settlements.Failure("late"))
	if res.Action != settlements.Conflict || res.Status != settlements.Succeeded {
		t.Errorf("failure after success: %+v", res)
	}
	if w.row(t, id2).status != "succeeded" || w.postings(t, id2) != 1 {
		t.Error("a failure after success undid or changed the settlement")
	}
	if n := w.audits(t, "settlement.conflict"); n != 2 {
		t.Errorf("conflict audit events = %d, want 2", n)
	}
}

func TestResolve_UnknownThenResolves(t *testing.T) {
	w := newWorld(t)

	// unknown -> succeeded
	id := w.settlement(t, 900)
	if res := w.resolve(t, id, settlements.AmbiguousResult()); res.Action != settlements.Apply || res.Status != settlements.Unknown {
		t.Fatalf("unknown: %+v", res)
	}
	if r := w.row(t, id); r.status != "unknown" || r.resolved || r.ledgerTx != nil {
		t.Errorf("row while unknown: %+v", r)
	}
	if w.postings(t, id) != 0 {
		t.Error("an unknown settlement posted to the ledger")
	}
	// A second ambiguous answer (a reconcile that still cannot tell) changes nothing.
	if res := w.resolve(t, id, settlements.AmbiguousResult()); res.Action != settlements.Ignore {
		t.Errorf("unknown again: %+v", res)
	}
	if n := w.audits(t, "settlement.unknown"); n != 1 {
		t.Errorf("unknown audit events = %d, want 1", n)
	}
	if res := w.resolve(t, id, settlements.Success()); res.Action != settlements.Apply || res.LedgerTransactionID == uuid.Nil {
		t.Fatalf("success after unknown: %+v", res)
	}
	if w.postings(t, id) != 1 || w.row(t, id).status != "succeeded" {
		t.Error("success after unknown did not post exactly once")
	}

	// unknown -> failed
	id2 := w.settlement(t, 100)
	w.resolve(t, id2, settlements.AmbiguousResult())
	if res := w.resolve(t, id2, settlements.Failure("not_found")); res.Action != settlements.Apply || res.Status != settlements.Failed {
		t.Errorf("failure after unknown: %+v", res)
	}
	if w.postings(t, id2) != 0 {
		t.Error("failure after unknown posted to the ledger")
	}
}

func TestResolve_UnknownAfterTerminalChangesNothing(t *testing.T) {
	w := newWorld(t)
	id := w.settlement(t, 250)
	w.resolve(t, id, settlements.Success())
	before := w.audits(t, "settlement.unknown") + w.audits(t, "settlement.conflict")
	if res := w.resolve(t, id, settlements.AmbiguousResult()); res.Action != settlements.Ignore || res.Status != settlements.Succeeded {
		t.Errorf("unknown after success: %+v", res)
	}
	if after := w.audits(t, "settlement.unknown") + w.audits(t, "settlement.conflict"); after != before {
		t.Error("an ignored outcome wrote an audit event")
	}
}

func TestResolve_Errors(t *testing.T) {
	w := newWorld(t)

	if _, err := w.tryResolve(uuid.New(), settlements.Success()); !errors.Is(err, settlements.ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}

	id := w.settlement(t, 100)
	for name, o := range map[string]settlements.Outcome{
		"failure without a code": {Kind: settlements.KindFailed},
		"success with a code":    {Kind: settlements.KindSucceeded, FailureCode: "x"},
		"unrecognized kind":      {Kind: "refunded"},
		"overlong failure code":  settlements.Failure(string(make([]byte, 101))),
		"empty failure code":     settlements.Failure(""),
	} {
		if _, err := w.tryResolve(id, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if w.row(t, id).status != "pending" {
		t.Error("an invalid outcome changed the settlement")
	}
}

// Resolve commits nothing itself: the posting and the status change belong to the
// caller's transaction (the job's completion transaction), so rolling back leaves
// the settlement exactly as it was.
func TestResolve_RollbackLeavesNothing(t *testing.T) {
	w := newWorld(t)
	id := w.settlement(t, 400)

	tx, err := w.pool.Begin(ctx)
	must(t, err)
	res, err := settlements.Resolve(ctx, tx, id, settlements.Success())
	must(t, err)
	if res.Action != settlements.Apply {
		t.Fatalf("res = %+v", res)
	}
	must(t, tx.Rollback(ctx))

	if r := w.row(t, id); r.status != "pending" || r.ledgerTx != nil {
		t.Errorf("row after rollback: %+v", r)
	}
	if w.postings(t, id) != 0 || len(w.balances(t)) != 0 || w.audits(t, "settlement.succeeded") != 0 {
		t.Error("rolled-back Resolve left ledger or audit rows")
	}
	// ... and it can still be resolved afterwards.
	if res := w.resolve(t, id, settlements.Success()); res.Action != settlements.Apply {
		t.Errorf("after rollback: %+v", res)
	}
}

// Ten resolvers, all reporting success at once (a webhook, its duplicates, a
// reconcile, a retried job): one applies, the rest are no-ops, one posting.
func TestResolve_ConcurrentSuccessPostsOnce(t *testing.T) {
	w := newWorld(t)
	id := w.settlement(t, 1234)

	const n = 10
	results := make([]settlements.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.tryResolve(id, settlements.Success())
		}()
	}
	close(start)
	wg.Wait()

	applied := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("resolver %d: %v", i, errs[i])
		}
		if results[i].Action == settlements.Apply {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("%d resolvers applied, want 1", applied)
	}
	if w.postings(t, id) != 1 {
		t.Errorf("postings = %d, want 1", w.postings(t, id))
	}
	if got := w.balances(t); got[w.from] != 1234 || got[w.to] != -1234 {
		t.Errorf("balances = %v", got)
	}
}

// Deterministic test of the lock. Another resolver is mid-transaction: it has already
// marked the settlement failed but not committed. Resolve(success) must wait BEFORE
// it reads the status, so that once the other commits it sees "failed" and reports a
// conflict. Without the lock it would read the old "pending", stall later on the
// UPDATE, and fail with an error instead.
func TestResolve_WaitsForTheRowLock(t *testing.T) {
	w := newWorld(t)
	id := w.settlement(t, 600)

	holder, err := w.owner.Begin(ctx) // stands in for another resolver, not the code under test
	must(t, err)
	defer holder.Rollback(ctx) //nolint:errcheck
	_, err = holder.Exec(ctx,
		`UPDATE settlements SET status = 'failed', failure_code = 'declined', resolved_at = now() WHERE id = $1`, id)
	must(t, err)

	type outcome struct {
		res settlements.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := w.tryResolve(id, settlements.Success())
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		t.Fatalf("Resolve finished while another transaction held the row (%+v, %v)", o.res, o.err)
	case <-time.After(300 * time.Millisecond):
	}
	must(t, holder.Commit(ctx))
	select {
	case o := <-done:
		must(t, o.err)
		if o.res.Action != settlements.Conflict || o.res.Status != settlements.Failed {
			t.Errorf("after the other resolver committed: %+v, want a conflict with failed", o.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve did not proceed after the lock was released")
	}
	if w.postings(t, id) != 0 {
		t.Error("a success after a committed failure posted to the ledger")
	}
}

// A success and a failure racing: exactly one wins, and the ledger agrees with the
// settlement's final status in every round, whichever one it was.
func TestResolve_SuccessVersusFailureRace(t *testing.T) {
	w := newWorld(t)
	rng := rand.New(rand.NewPCG(7, 11))
	wins := map[string]int{}
	for round := range 30 {
		id := w.settlement(t, int64(100+round))
		// One of the two starts a little late. Drawn here: rand.Rand is not safe to share.
		late, delay := rng.IntN(2), time.Duration(rng.IntN(3))*time.Millisecond
		var wg sync.WaitGroup
		var applied sync.Map
		for i, o := range []settlements.Outcome{settlements.Success(), settlements.Failure("declined")} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i == late {
					time.Sleep(delay)
				}
				res, err := w.tryResolve(id, o)
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				applied.Store(o.Kind, res.Action)
			}()
		}
		wg.Wait()

		r := w.row(t, id)
		wins[r.status]++
		switch r.status {
		case "succeeded":
			if w.postings(t, id) != 1 {
				t.Fatalf("round %d: succeeded with %d postings", round, w.postings(t, id))
			}
		case "failed":
			if w.postings(t, id) != 0 {
				t.Fatalf("round %d: failed but posted", round)
			}
		default:
			t.Fatalf("round %d: status %q", round, r.status)
		}
		// Exactly one applied; the other was told it conflicted.
		a1, _ := applied.Load(settlements.KindSucceeded)
		a2, _ := applied.Load(settlements.KindFailed)
		if !(a1 == settlements.Apply && a2 == settlements.Conflict) && !(a1 == settlements.Conflict && a2 == settlements.Apply) {
			t.Fatalf("round %d: actions %v / %v", round, a1, a2)
		}
	}
	t.Logf("winners over 30 rounds: %v", wins)
}
