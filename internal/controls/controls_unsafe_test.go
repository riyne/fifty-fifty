//go:build unsafe_controls

package controls_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/controls"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

// These tests need the unsafe_controls tag (go test -tags unsafe_controls
// ./internal/controls). Each one shows, for one control, that with the control on the
// protection works and with it off the protection is gone, and that off removes ONLY
// that protection: the authorization in the same statement still applies. They toggle
// a process-wide switch, so none of them is parallel.

const body = `{"description":"dinner","items":[{"description":"pizza","price_minor":1500}],"tax_minor":0,"tip_minor":0}`

type fixture struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	payer    uuid.UUID
	member   uuid.UUID
	outsider uuid.UUID
	group    uuid.UUID
	expense  uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	t.Cleanup(controls.Reset)
	s := teststack.New(t)
	payerTok, memberTok, outTok := s.Token(t, "payer"), s.Token(t, "member"), s.Token(t, "outsider")
	group := s.CreateGroup(t, payerTok, "controls")
	_, code := s.CreateJoinCode(t, payerTok, group, 0, 0)
	if r := s.Join(t, memberTok, code); r.Status != 201 {
		t.Fatalf("join = %d: %s", r.Status, r.Body)
	}
	return fixture{
		pool: s.Pool, q: db.New(s.Pool),
		payer: s.UserID(t, payerTok), member: s.UserID(t, memberTok), outsider: s.UserID(t, outTok),
		group:   uuid.MustParse(group),
		expense: uuid.MustParse(s.CreateExpense(t, payerTok, group, body)),
	}
}

// hold opens a transaction that takes the given row lock on the statement's row and
// keeps it until the test ends. It stands in for the other request the lock is meant
// to wait for.
func (f fixture) hold(t *testing.T, stmt string, id uuid.UUID) {
	t.Helper()
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var got uuid.UUID
	if err := tx.QueryRow(context.Background(), stmt, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
}

// waits runs fn and reports whether it was still waiting for a lock after 600 ms
// (its context expired). Any other error fails the test.
func waits(t *testing.T, fn func(ctx context.Context) error) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := fn(ctx)
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return true
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return false
}

func (f fixture) tx(t *testing.T, ctx context.Context) (*db.Queries, func()) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return db.New(tx), func() { _ = tx.Rollback(context.Background()) }
}

func TestClaimLock(t *testing.T) {
	f := newFixture(t)
	f.hold(t, `SELECT id FROM expenses WHERE id = $1 FOR UPDATE`, f.expense) // what finalize holds
	claim := func(user uuid.UUID) func(context.Context) error {
		return func(ctx context.Context) error {
			q, done := f.tx(t, ctx)
			defer done()
			_, err := controls.LockExpenseForClaim(ctx, q, db.LockExpenseForClaimParams{UserID: user, ExpenseID: f.expense, GroupID: f.group})
			return err
		}
	}
	if !waits(t, claim(f.member)) {
		t.Fatal("with the control on, a claim must wait for a finalize holding FOR UPDATE")
	}
	controls.Disable(controls.ClaimLock)
	if waits(t, claim(f.member)) {
		t.Fatal("with the control off, the claim must not wait")
	}
	// Only the lock is gone: a non-member still gets no row.
	if err := claim(f.outsider)(context.Background()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("outsider = %v, want no rows", err)
	}
}

func TestFinalizeLock(t *testing.T) {
	f := newFixture(t)
	f.hold(t, `SELECT id FROM expenses WHERE id = $1 FOR SHARE`, f.expense) // what a claim holds
	fin := func(user uuid.UUID) func(context.Context) error {
		return func(ctx context.Context) error {
			q, done := f.tx(t, ctx)
			defer done()
			_, err := controls.LockExpenseForFinalize(ctx, q, db.LockExpenseForFinalizeParams{UserID: user, ExpenseID: f.expense, GroupID: f.group})
			return err
		}
	}
	if !waits(t, fin(f.payer)) {
		t.Fatal("with the control on, finalize must wait for an in-flight claim")
	}
	controls.Disable(controls.FinalizeLock)
	if waits(t, fin(f.payer)) {
		t.Fatal("with the control off, finalize must not wait")
	}
	if err := fin(f.member)(context.Background()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-payer = %v, want no rows", err)
	}
}

func TestVoidLock(t *testing.T) {
	f := newFixture(t)
	f.hold(t, `SELECT id FROM expenses WHERE id = $1 FOR SHARE`, f.expense)
	void := func(user uuid.UUID) func(context.Context) error {
		return func(ctx context.Context) error {
			q, done := f.tx(t, ctx)
			defer done()
			_, err := controls.LockExpenseForVoid(ctx, q, db.LockExpenseForVoidParams{UserID: user, ExpenseID: f.expense, GroupID: f.group})
			return err
		}
	}
	if !waits(t, void(f.payer)) {
		t.Fatal("with the control on, void must wait")
	}
	controls.Disable(controls.VoidLock)
	if waits(t, void(f.payer)) {
		t.Fatal("with the control off, void must not wait")
	}
	if err := void(f.member)(context.Background()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-payer, non-owner = %v, want no rows", err)
	}
}

func TestGroupLock(t *testing.T) {
	f := newFixture(t)
	f.hold(t, `SELECT id FROM groups WHERE id = $1 FOR NO KEY UPDATE`, f.group) // another settlement creation
	lock := func(ctx context.Context) error {
		q, done := f.tx(t, ctx)
		defer done()
		_, err := controls.LockGroupForSettlement(ctx, q, f.group)
		return err
	}
	if !waits(t, lock) {
		t.Fatal("with the control on, a second settlement creation must wait")
	}
	controls.Disable(controls.GroupLock)
	if waits(t, lock) {
		t.Fatal("with the control off, it must not wait")
	}
	q, done := f.tx(t, context.Background())
	defer done()
	if _, err := controls.LockGroupForSettlement(context.Background(), q, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown group = %v, want no rows", err)
	}
}

func TestEditLock(t *testing.T) {
	f := newFixture(t)
	f.hold(t, `SELECT id FROM expenses WHERE id = $1 FOR SHARE`, f.expense) // a claim in flight
	edit := func(user uuid.UUID) func(context.Context) error {
		return func(ctx context.Context) error {
			q, done := f.tx(t, ctx)
			defer done()
			got, err := controls.LockExpenseForEdit(ctx, q, db.LockExpenseForEditParams{UserID: user, ExpenseID: f.expense, GroupID: f.group})
			if err == nil && (got.Status != "claiming" || got.Version != 1) {
				t.Errorf("row = %+v", got)
			}
			return err
		}
	}
	if !waits(t, edit(f.payer)) {
		t.Fatal("with the control on, an edit must wait for an in-flight claim")
	}
	controls.Disable(controls.EditLock)
	if waits(t, edit(f.payer)) {
		t.Fatal("with the control off, the edit must not wait")
	}
	if err := edit(f.member)(context.Background()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-payer = %v, want no rows", err)
	}
}

func TestEditVersionCheck(t *testing.T) {
	if !controls.VersionMatches(2, 2) || controls.VersionMatches(2, 1) || controls.VersionMatches(1, 2) {
		t.Fatal("with the check on, only equal versions match")
	}
	controls.Disable(controls.EditVersionCheck)
	if !controls.VersionMatches(2, 1) || !controls.VersionMatches(2, 2) {
		t.Fatal("with the check off, any version matches")
	}
	controls.Enable(controls.EditVersionCheck)
	if controls.VersionMatches(2, 1) {
		t.Fatal("with the check back on, a stale version must not match")
	}
}

// Through the real handler: a second edit from the same (now stale) version is refused,
// and with the check off it is applied on top of the first, which the audit event shows
// (the version it was made from is not the one before the new version). Only the
// comparison is gone: the version still goes up by one and the stored version is what
// the final update is conditional on.
func TestEditVersionCheckThroughTheHandler(t *testing.T) {
	t.Cleanup(controls.Reset)
	s := teststack.New(t)
	tok := s.Token(t, "editor")
	group := s.CreateGroup(t, tok, "edits")
	exp := s.CreateExpense(t, tok, group, body)
	path := "/v1/groups/" + group + "/expenses/" + exp
	edit := func(price int) teststack.Response {
		return s.Do(t, tok, "PATCH", path, `{"version":1,"tip_minor":`+string(rune('0'+price))+`}`, "Idempotency-Key", uuid.NewString())
	}
	if r := edit(1); r.Status != 200 {
		t.Fatalf("first edit = %d: %s", r.Status, r.Body)
	}
	if r := edit(2); r.Status != 409 {
		t.Fatalf("stale edit with the check on = %d: %s, want 409", r.Status, r.Body)
	}
	controls.Disable(controls.EditVersionCheck)
	if r := edit(3); r.Status != 200 {
		t.Fatalf("stale edit with the check off = %d: %s, want 200", r.Status, r.Body)
	}
	if n := s.QueryInt(t, `SELECT version FROM expenses WHERE id = $1`, exp); n != 3 {
		t.Errorf("version = %d, want 3", n)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'expense.items_edited' AND data->>'expense_id' = $1 AND (data->>'version')::int - 1 <> (data->>'base_version')::int`, exp); n != 1 {
		t.Errorf("audit events made from a stale version = %d, want 1", n)
	}
}

// Confirm compares versions through the same seam as the edit.
func TestEditVersionCheckThroughConfirm(t *testing.T) {
	t.Cleanup(controls.Reset)
	s := teststack.New(t)
	tok := s.Token(t, "confirmer")
	group := s.CreateGroup(t, tok, "confirms")
	exp := s.CreateExpense(t, tok, group, body)
	s.Exec(t, `UPDATE expenses SET status = 'needs_review' WHERE id = $1`, exp)
	confirm := func(version string) teststack.Response {
		return s.Do(t, tok, "POST", "/v1/groups/"+group+"/expenses/"+exp+"/confirm", `{"version":`+version+`}`, "Idempotency-Key", uuid.NewString())
	}
	if r := confirm("7"); r.Status != 409 {
		t.Fatalf("confirm at a stale version with the check on = %d: %s, want 409", r.Status, r.Body)
	}
	controls.Disable(controls.EditVersionCheck)
	if r := confirm("7"); r.Status != 200 {
		t.Fatalf("confirm at a stale version with the check off = %d: %s, want 200", r.Status, r.Body)
	}
}

func (f fixture) settlement(t *testing.T) db.Settlement {
	t.Helper()
	s, err := f.q.CreateSettlement(context.Background(), db.CreateSettlementParams{GroupID: f.group, FromMember: f.member, ToMember: f.payer, AmountMinor: 100})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveLock(t *testing.T) {
	f := newFixture(t)
	s := f.settlement(t)
	f.hold(t, `SELECT id FROM settlements WHERE id = $1 FOR UPDATE`, s.ID) // another resolver
	lock := func(ctx context.Context) error {
		q, done := f.tx(t, ctx)
		defer done()
		got, err := controls.LockSettlement(ctx, q, s.ID)
		if err == nil && (got.ID != s.ID || got.Status != "pending" || got.AmountMinor != 100) {
			t.Errorf("row = %+v", got)
		}
		return err
	}
	if !waits(t, lock) {
		t.Fatal("with the control on, a second resolver must wait")
	}
	controls.Disable(controls.ResolveLock)
	if waits(t, lock) {
		t.Fatal("with the control off, it must not wait")
	}
}

func TestResolveGuards(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	fail := func(id uuid.UUID) int64 {
		n, err := controls.MarkSettlementFailed(ctx, f.q, db.MarkSettlementFailedParams{ID: id, FailureCode: pgtype.Text{String: "card_declined", Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	s := f.settlement(t)
	if n := fail(s.ID); n != 1 {
		t.Fatalf("first failure moved %d rows, want 1", n)
	}
	if n := fail(s.ID); n != 0 {
		t.Fatalf("with the guard on, failing an already failed settlement moved %d rows, want 0", n)
	}
	controls.Disable(controls.ResolveGuards)
	if n := fail(s.ID); n != 1 {
		t.Fatalf("with the guard off, the update must apply again: moved %d rows", n)
	}
	// The same for the other two statements, so none of the three keeps its condition.
	u := f.settlement(t)
	if n, err := controls.MarkSettlementUnknown(ctx, f.q, u.ID); err != nil || n != 1 {
		t.Fatalf("unknown = %d, %v", n, err)
	}
	if n, err := controls.MarkSettlementUnknown(ctx, f.q, u.ID); err != nil || n != 1 {
		t.Fatalf("with the guard off, unknown from unknown moved %d rows (%v), want 1", n, err)
	}
	controls.Enable(controls.ResolveGuards)
	if n, err := controls.MarkSettlementUnknown(ctx, f.q, u.ID); err != nil || n != 0 {
		t.Fatalf("with the guard back on, unknown from unknown moved %d rows (%v), want 0", n, err)
	}
	// MarkSettlementSucceeded needs a real posting to link to; a settlement that is not
	// pending or unknown is the case the guard exists for.
	g := f.settlement(t)
	fail(g.ID)
	if n, err := controls.MarkSettlementSucceeded(ctx, f.q, db.MarkSettlementSucceededParams{ID: g.ID}); err != nil || n != 0 {
		t.Fatalf("with the guard on, success after failure moved %d rows (%v), want 0", n, err)
	}
}

func TestIdempotencyCheck(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runs := 0
	do := func(key string) idempotency.Outcome {
		out, err := idempotency.Do(ctx, f.pool, f.payer, key, []byte("fp"), func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
			runs++
			return idempotency.Result{Status: 200, Body: []byte(`{}`)}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	key := uuid.NewString()
	do(key)
	if out := do(key); !out.Replayed || runs != 1 {
		t.Fatalf("with the check on, the retry must replay: replayed=%v runs=%d", out.Replayed, runs)
	}
	controls.Disable(controls.IdempotencyCheck)
	key = uuid.NewString()
	do(key)
	if out := do(key); out.Replayed || runs != 3 {
		t.Fatalf("with the check off, the retry must run again: replayed=%v runs=%d", out.Replayed, runs)
	}
}

func TestDisableEnableParse(t *testing.T) {
	t.Cleanup(controls.Reset)
	controls.Reset()
	if len(controls.Active()) != 0 {
		t.Fatalf("active after reset: %v", controls.Active())
	}
	controls.Disable(controls.GroupLock, controls.ClaimLock)
	if !controls.Disabled(controls.GroupLock) || !controls.Disabled(controls.ClaimLock) || controls.Disabled(controls.VoidLock) {
		t.Fatalf("active = %v", controls.Active())
	}
	controls.Enable(controls.ClaimLock)
	if controls.Disabled(controls.ClaimLock) {
		t.Fatal("enable did not switch it back on")
	}
	for _, c := range controls.All {
		got, err := controls.Parse(string(c))
		if err != nil || len(got) != 1 || got[0] != c {
			t.Errorf("Parse(%q) = %v, %v", c, got, err)
		}
	}
	if _, err := controls.Parse("claim_lock, cliam_lock"); err == nil {
		t.Error("a misspelled control name must be an error")
	}
	if got, err := controls.Parse(""); err != nil || len(got) != 0 {
		t.Errorf("Parse(empty) = %v, %v", got, err)
	}
}
