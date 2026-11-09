// Package dbinvariants tests the guarantees the database itself gives about
// expenses, claims and the ledger (migration 00005), independent of any Go
// handler: raw SQL that violates an invariant must be refused.
//
// These run as the unprivileged app role (the same privileges as production)
// except where a test says it needs the owner to prove a protection holds even
// against the owner.
package dbinvariants_test

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/harness/testutil"
)

// SQLSTATEs the schema is expected to raise.
const (
	codeCheck      = "23514" // check_violation: CHECK constraints and the zero-sum trigger
	codeFK         = "23503" // foreign_key_violation
	codeUnique     = "23505" // unique_violation
	codeRestrict   = "23001" // restrict_violation: append-only and same-transaction triggers
	codePrivilege  = "42501" // insufficient_privilege: REVOKEd statements for the app role
	noErrExpected  = ""
	ledgerTxInsert = `INSERT INTO ledger_transactions (group_id, kind, expense_id, reverses_id, created_by)
	                  VALUES ($1, $2, $3, $4, $5) RETURNING id`
)

var ctx = context.Background()

// ---- fixtures ----

type world struct {
	// pool is what the tests aim refused statements at: the application role, fifty_app,
	// with its connections carrying the identity of actor (a member of every group in the
	// world) so that row-level security lets each statement through and it is the
	// constraint, trigger or revoked privilege under test that refuses it. Without the
	// identity RLS would refuse everything and the tests would pass for the wrong reason.
	// owner seeds fixtures (users, groups, memberships, expenses, items).
	pool  *pgxpool.Pool
	owner *pgxpool.Pool
	actor uuid.UUID

	g, h              uuid.UUID // two groups
	payer, b, c       uuid.UUID // members of g
	outsider          uuid.UUID // member of h only
	otherGroupMember  uuid.UUID // alias for outsider, for readability in group-h tests
	members           []uuid.UUID
	expense, expense2 uuid.UUID // claiming expenses in g; expense has item1 and item2
	item1, item2      uuid.UUID
	expenseH, itemH   uuid.UUID // an expense in group h
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{owner: testutil.SharedOwnerPool(t)}
	w.actor = w.user(t)
	w.pool = appPoolAs(t, w.actor)
	w.payer, w.b, w.c, w.outsider = w.user(t), w.user(t), w.user(t), w.user(t)
	w.otherGroupMember = w.outsider
	w.members = []uuid.UUID{w.payer, w.b, w.c}
	w.g = w.group(t, w.payer, w.b, w.c)
	w.h = w.group(t, w.outsider)
	w.expense, w.item1, w.item2 = w.expenseWithTwoItems(t, w.g, w.payer)
	w.expense2, _, _ = w.expenseWithTwoItems(t, w.g, w.payer)
	w.expenseH, w.itemH, _ = w.expenseWithTwoItems(t, w.h, w.outsider)
	return w
}

func (w *world) user(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := w.owner.QueryRow(ctx,
		`INSERT INTO users (issuer, subject) VALUES ('test', $1) RETURNING id`,
		testutil.UniqueSubject("dbinv")).Scan(&id)
	must(t, err)
	return id
}

// group creates a CAD group; the first member is the owner.
func (w *world) group(t *testing.T, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO groups (name, currency, created_by) VALUES ('g', 'CAD', $1) RETURNING id`, members[0]).Scan(&id))
	for i, m := range members {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		_, err := w.owner.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, $3)`, id, m, role)
		must(t, err)
	}
	// The actor sees every group, so row-level security never is what refuses a statement.
	_, err := w.owner.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, id, w.actor)
	must(t, err)
	return id
}

func (w *world) expenseWithTwoItems(t *testing.T, group, payer uuid.UUID) (exp, i1, i2 uuid.UUID) {
	t.Helper()
	exp = w.newExpense(t, group, payer, "claiming")
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, 0, 'pizza', 1500) RETURNING id`, exp).Scan(&i1))
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, 1, 'beer', 800) RETURNING id`, exp).Scan(&i2))
	return exp, i1, i2
}

func (w *world) newExpense(t *testing.T, group, payer uuid.UUID, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	q := `INSERT INTO expenses (group_id, payer_id, description, status) VALUES ($1, $2, 'dinner', $3) RETURNING id`
	if status == "finalized" {
		q = `INSERT INTO expenses (group_id, payer_id, description, status, finalized_at) VALUES ($1, $2, 'dinner', $3, now()) RETURNING id`
	}
	must(t, w.owner.QueryRow(ctx, q, group, payer, status).Scan(&id))
	return id
}

// finalizedExpense returns a fresh finalized expense in group g, ready to post.
func (w *world) finalizedExpense(t *testing.T) uuid.UUID {
	t.Helper()
	return w.newExpense(t, w.g, w.payer, "finalized")
}

// ---- helpers ----

// appPoolAs returns a pool of the application role whose every connection starts with
// app.user_id set to id (a session-level setting, unlike production's transaction-local
// one; these tests are about the schema, and the isolation tests are in test/rls).
func appPoolAs(t *testing.T, id uuid.UUID) *pgxpool.Pool {
	t.Helper()
	testutil.Pool(t) // skips, or fails under REQUIRE_DATABASE=1, when there is no database
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.RuntimeParams["options"] = "-c app.user_id=" + id.String()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// inTx runs fn in a transaction and commits. An error from fn or from COMMIT is
// returned (deferred constraint triggers fire at COMMIT). On any error the
// transaction is rolled back.
func inTx(pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// inTxAlwaysRollback runs fn and always rolls back, even on success. Used for
// destructive statements so that if a protection were broken, the test would
// not actually destroy rows in the shared database.
func inTxAlwaysRollback(pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return fn(tx)
}

// runRefused runs one destructive statement in an always-rolled-back transaction and
// returns the error it raised. TRUNCATE ... CASCADE takes ACCESS EXCLUSIVE locks on
// every table that references the ledger (since migration 00011 that includes
// settlements), in an order that can deadlock with a settlement or finalize
// transaction running in another test package. A short lock_timeout makes this
// statement give up quickly instead of holding locks and letting the deadlock
// detector pick a victim; contention (55P03, 40P01) is retried, anything else is
// returned as the real answer.
func runRefused(pool *pgxpool.Pool, sql string) (err error) {
	const lockNotAvailable, deadlock = "55P03", "40P01"
	for range 100 {
		err = inTxAlwaysRollback(pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, sql)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != lockNotAvailable && pg.Code != deadlock) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

func wantCode(t *testing.T, err error, want string, what string) {
	t.Helper()
	var pg *pgconn.PgError
	switch {
	case want == noErrExpected && err == nil:
		return
	case want == noErrExpected:
		t.Errorf("%s: unexpected error: %v", what, err)
	case err == nil:
		t.Errorf("%s: expected SQLSTATE %s, got no error", what, want)
	case !errors.As(err, &pg):
		t.Errorf("%s: expected SQLSTATE %s, got non-database error: %v", what, want, err)
	case pg.Code != want:
		t.Errorf("%s: SQLSTATE %s (%s), want %s", what, pg.Code, pg.Message, want)
	}
}

type entry struct {
	member uuid.UUID
	amount int64
}

// post writes one ledger transaction header and its entries in a single database
// transaction, like the real finalize/void will, and returns the header id.
func (w *world) post(kind string, group, expense uuid.UUID, reverses *uuid.UUID, createdBy uuid.UUID, entries ...entry) (uuid.UUID, error) {
	var id uuid.UUID
	err := inTx(w.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, ledgerTxInsert, group, kind, expense, reverses, createdBy).Scan(&id); err != nil {
			return err
		}
		for _, e := range entries {
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ($1, $2, $3, $4)`,
				id, group, e.member, e.amount); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// ---- the app role is what we think it is ----

// If TEST_DATABASE_URL accidentally pointed at the owner, every "the app cannot
// ..." test below would be meaningless. Fail loudly instead.
func TestAppRoleIsUnprivileged(t *testing.T) {
	pool := testutil.Pool(t)
	var user string
	var super bool
	must(t, pool.QueryRow(ctx,
		`SELECT current_user, (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)`).Scan(&user, &super))
	if user != "fifty_app" || super {
		t.Fatalf("tests must run as the unprivileged fifty_app role, got user=%q superuser=%v (check TEST_DATABASE_URL)", user, super)
	}
	var owns int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' AND pg_get_userbyid(c.relowner) = current_user`).Scan(&owns))
	if owns != 0 {
		t.Fatalf("the app role owns %d tables; owners can bypass REVOKE", owns)
	}
}

// ---- zero-sum ----

func TestLedgerTransactionsMustSumToZero(t *testing.T) {
	w := newWorld(t)
	tests := []struct {
		name    string
		entries []entry
		want    string
	}{
		{"balanced three-way", []entry{{w.payer, 2300}, {w.b, -1150}, {w.c, -1150}}, noErrExpected},
		{"balanced two-way", []entry{{w.payer, 100}, {w.b, -100}}, noErrExpected},
		{"credit without matching debit", []entry{{w.payer, 100}, {w.b, -60}}, codeCheck},
		{"single entry", []entry{{w.payer, 1}}, codeCheck},
		{"off by one cent", []entry{{w.payer, 2301}, {w.b, -1150}, {w.c, -1150}}, codeCheck},
		{"sums to 2^64, which would wrap to zero in int64", []entry{{w.payer, math.MaxInt64}, {w.b, math.MaxInt64}, {w.c, 2}}, codeCheck},
		{"no entries at all (trivially zero)", nil, noErrExpected},
	}
	for _, tc := range tests {
		_, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, tc.entries...)
		wantCode(t, err, tc.want, tc.name)
	}
}

// ---- append-only ----

// Entries cannot be appended to a posting after the transaction that created it
// has committed, even if the late entries balance. Otherwise history could be
// rewritten with a balanced pair.
func TestLedgerEntriesOnlyInCreatingTransaction(t *testing.T) {
	w := newWorld(t)
	txID, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer,
		entry{w.payer, 100}, entry{w.b, -100})
	must(t, err)

	err = inTx(w.pool, func(tx pgx.Tx) error {
		for _, e := range []entry{{w.c, 5}, {w.b, -5}} { // balanced pair
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ($1, $2, $3, $4)`,
				txID, w.g, e.member, e.amount); err != nil {
				return err
			}
		}
		return nil
	})
	wantCode(t, err, codeRestrict, "balanced pair appended to a committed posting")
}

var mutations = []struct{ name, sql string }{
	{"UPDATE entries", `UPDATE ledger_entries SET amount_minor = amount_minor + 1`},
	{"DELETE entries", `DELETE FROM ledger_entries`},
	{"TRUNCATE entries", `TRUNCATE ledger_entries`},
	{"UPDATE transactions", `UPDATE ledger_transactions SET kind = kind`},
	{"DELETE transactions", `DELETE FROM ledger_transactions`},
	{"TRUNCATE transactions", `TRUNCATE ledger_transactions CASCADE`},
}

// postSomething makes sure the ledger tables are non-empty so row-level triggers
// have a row to fire on.
func (w *world) postSomething(t *testing.T) {
	t.Helper()
	_, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, entry{w.payer, 10}, entry{w.b, -10})
	must(t, err)
}

func TestLedgerAppendOnly_AppRole(t *testing.T) {
	w := newWorld(t)
	w.postSomething(t)
	for _, m := range mutations {
		// TRUNCATE is not granted to the app role at all, so it fails the same way.
		err := runRefused(w.pool, m.sql)
		wantCode(t, err, codePrivilege, "app role: "+m.name)
	}
	// audit_events is written once and never changed.
	for _, q := range []string{`UPDATE audit_events SET kind = kind`, `DELETE FROM audit_events`} {
		err := inTxAlwaysRollback(w.pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		wantCode(t, err, codePrivilege, "app role: "+q)
	}
}

// The owner (a superuser in the dev stack) can run the statements, so only the
// triggers stand in the way. Always rolled back so a broken trigger cannot wipe
// the shared database.
func TestLedgerAppendOnly_EvenOwnerIsRefused(t *testing.T) {
	w := newWorld(t)
	w.postSomething(t)
	owner := testutil.OwnerPool(t)
	for _, m := range mutations {
		err := runRefused(owner, m.sql)
		wantCode(t, err, codeRestrict, "owner: "+m.name)
		if err != nil && !strings.Contains(err.Error(), "append-only") {
			t.Errorf("owner: %s: refused for the wrong reason: %v", m.name, err)
		}
	}
}

// ---- referential integrity: cross-group rows are impossible ----

func TestCrossGroupRowsAreRefused(t *testing.T) {
	w := newWorld(t)

	// An expense whose payer is not a member of its group.
	_, err := w.pool.Exec(ctx,
		`INSERT INTO expenses (group_id, payer_id, description, status) VALUES ($1, $2, 'x', 'claiming')`, w.g, w.outsider)
	wantCode(t, err, codeFK, "expense with non-member payer")

	claim := `INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`
	_, err = w.pool.Exec(ctx, claim, w.item1, w.expense, w.g, w.outsider)
	wantCode(t, err, codeFK, "claim by a user who is not in the group")
	// Each of the next two violates exactly one foreign key, so removing either
	// constraint on its own is detected.
	_, err = w.pool.Exec(ctx, claim, w.item1, w.expense2, w.g, w.b)
	wantCode(t, err, codeFK, "claim naming a real expense in the group that does not own the item")
	_, err = w.pool.Exec(ctx, claim, w.item1, w.expense, w.h, w.outsider)
	wantCode(t, err, codeFK, "claim whose group_id is not the expense's group (claimant is a member of that other group)")
	_, err = w.pool.Exec(ctx, claim, w.item1, w.expenseH, w.g, w.b)
	wantCode(t, err, codeFK, "claim mixing group g's item with group h's expense")
	_, err = w.pool.Exec(ctx, claim, w.itemH, w.expenseH, w.h, w.b)
	wantCode(t, err, codeFK, "group-g member claiming an item in group h")
	_, err = w.pool.Exec(ctx, claim, w.item1, w.expense, w.g, w.b)
	wantCode(t, err, noErrExpected, "control: a valid claim")

	// A ledger posting for an expense in another group.
	exp := w.finalizedExpense(t)
	_, err = w.post("expense", w.h, exp, nil, w.outsider, entry{w.outsider, 1}, entry{w.outsider, -1})
	wantCode(t, err, codeFK, "posting in group h for an expense of group g")

	// Entries for someone who is not in the group, or in the wrong group.
	_, err = w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, entry{w.payer, 5}, entry{w.outsider, -5})
	wantCode(t, err, codeFK, "entry for a non-member")

	err = inTx(w.pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, ledgerTxInsert, w.g, "expense", w.finalizedExpense(t), nil, w.payer).Scan(&id); err != nil {
			return err
		}
		// Entry claims group h but its header is group g.
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ($1, $2, $3, 5)`,
			id, w.h, w.outsider)
		return err
	})
	wantCode(t, err, codeFK, "entry whose group differs from its transaction's")

	// A void must reverse a posting of the same expense.
	other, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, entry{w.payer, 5}, entry{w.b, -5})
	must(t, err)
	_, err = w.post("void", w.g, w.finalizedExpense(t), &other, w.payer, entry{w.payer, -5}, entry{w.b, 5})
	wantCode(t, err, codeFK, "void reversing another expense's posting")
}

// ---- uniqueness: nothing posts twice ----

func TestLedgerUniqueness(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedExpense(t)

	first, err := w.post("expense", w.g, exp, nil, w.payer, entry{w.payer, 100}, entry{w.b, -100})
	wantCode(t, err, noErrExpected, "first posting")
	_, err = w.post("expense", w.g, exp, nil, w.payer, entry{w.payer, 100}, entry{w.b, -100})
	wantCode(t, err, codeUnique, "second finalize posting for the same expense")

	// Reversal: exactly one, and it can only reverse the real posting.
	void, err := w.post("void", w.g, exp, &first, w.payer, entry{w.payer, -100}, entry{w.b, 100})
	wantCode(t, err, noErrExpected, "void")
	_, err = w.post("void", w.g, exp, &first, w.payer, entry{w.payer, -100}, entry{w.b, 100})
	wantCode(t, err, codeUnique, "second void for the same expense")
	_, err = w.post("void", w.g, exp, &void, w.payer, entry{w.payer, 100}, entry{w.b, -100})
	wantCode(t, err, codeUnique, "voiding the void")

	// One net entry per member per transaction.
	_, err = w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer,
		entry{w.payer, 50}, entry{w.b, -25}, entry{w.b, -25})
	wantCode(t, err, codeUnique, "two entries for the same member in one transaction")

	// Claims: one per item and member; items: one per position.
	claim := `INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`
	_, err = w.pool.Exec(ctx, claim, w.item2, w.expense, w.g, w.c)
	must(t, err)
	_, err = w.pool.Exec(ctx, claim, w.item2, w.expense, w.g, w.c)
	wantCode(t, err, codeUnique, "duplicate claim")
	_, err = w.pool.Exec(ctx,
		`INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, 0, 'dup', 1)`, w.expense)
	wantCode(t, err, codeUnique, "two items at the same position")
}

// ---- CHECK constraints ----

func TestChecks(t *testing.T) {
	w := newWorld(t)
	exec := func(what, q string, args ...any) {
		t.Helper()
		_, err := w.pool.Exec(ctx, q, args...)
		wantCode(t, err, codeCheck, what)
	}
	ins := `INSERT INTO expenses (group_id, payer_id, description, status, tax_minor, tip_minor, finalized_at, voided_at)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	exec("empty description", ins, w.g, w.payer, "", "claiming", 0, 0, nil, nil)
	exec("negative tax", ins, w.g, w.payer, "x", "claiming", -1, 0, nil, nil)
	exec("negative tip", ins, w.g, w.payer, "x", "claiming", 0, -1, nil, nil)
	exec("unknown status", ins, w.g, w.payer, "x", "paid", 0, 0, nil, nil)
	exec("finalized without finalized_at", ins, w.g, w.payer, "x", "finalized", 0, 0, nil, nil)
	exec("claiming but already finalized_at", ins, w.g, w.payer, "x", "claiming", 0, 0, "2026-01-01", nil)
	exec("void without voided_at", ins, w.g, w.payer, "x", "void", 0, 0, "2026-01-01", nil)
	exec("finalized with voided_at", ins, w.g, w.payer, "x", "finalized", 0, 0, "2026-01-01", "2026-01-02")

	exec("negative item price",
		`INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, 9, 'x', -1)`, w.expense)
	exec("negative item position",
		`INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, -1, 'x', 1)`, w.expense)

	// Ledger shape: zero amounts are never stored; kinds carry the right links.
	_, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, entry{w.payer, 0}, entry{w.b, 0})
	wantCode(t, err, codeCheck, "zero-amount entry")
	_, err = w.post("settlement", w.g, w.finalizedExpense(t), nil, w.payer)
	wantCode(t, err, codeCheck, "settlement kind with an expense id and no settlement id")
	_, err = w.post("void", w.g, w.finalizedExpense(t), nil, w.payer)
	wantCode(t, err, codeCheck, "void with no reverses_id")
	first, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, entry{w.payer, 1}, entry{w.b, -1})
	must(t, err)
	_, err = w.post("expense", w.g, w.finalizedExpense(t), &first, w.payer)
	wantCode(t, err, codeCheck, "expense posting with reverses_id")
}
