package rls_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
)

// For every protected table: what the application role sees when it runs the laziest query
// there is (a count with no WHERE clause at all, as a handler with no membership check
// would) against what it should see, computed independently by the owner from the
// memberships table.
type tableSpec struct {
	name string
	// expected is run by the owner with $1 = the user's group ids and $2 = the user.
	expected string
}

var protected = []tableSpec{
	{"groups", `SELECT count(*) FROM groups WHERE id = ANY($1) OR created_by = $2`},
	{"memberships", `SELECT count(*) FROM memberships WHERE group_id = ANY($1)`},
	{"users", `SELECT count(*) FROM users WHERE id = $2 OR id IN (SELECT user_id FROM memberships WHERE group_id = ANY($1))`},
	{"join_codes", `SELECT count(*) FROM join_codes WHERE group_id = ANY($1)`},
	{"idempotency_keys", `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`},
	{"audit_events", `SELECT count(*) FROM audit_events WHERE group_id = ANY($1)`},
	{"expenses", `SELECT count(*) FROM expenses WHERE group_id = ANY($1)`},
	{"expense_items", `SELECT count(*) FROM expense_items WHERE expense_id IN (SELECT id FROM expenses WHERE group_id = ANY($1))`},
	{"claims", `SELECT count(*) FROM claims WHERE group_id = ANY($1)`},
	{"ledger_transactions", `SELECT count(*) FROM ledger_transactions WHERE group_id = ANY($1)`},
	{"ledger_entries", `SELECT count(*) FROM ledger_entries WHERE group_id = ANY($1)`},
	{"settlements", `SELECT count(*) FROM settlements WHERE group_id = ANY($1)`},
	{"receipts", `SELECT count(*) FROM receipts WHERE group_id = ANY($1)`},
	{"receipt_quota", `SELECT count(*) FROM receipt_quota WHERE user_id = $1`},
}

// argsFor passes only the parameters the query mentions ($1 = groups, $2 = user); the
// driver refuses a statement with more arguments than parameters.
func argsFor(sql string, groups []uuid.UUID, user uuid.UUID) []any {
	switch {
	case strings.Contains(sql, "$1") && strings.Contains(sql, "$2"):
		return []any{groups, user}
	case strings.Contains(sql, "$1") && (strings.Contains(sql, "user_id = $1") && !strings.Contains(sql, "ANY($1)")):
		return []any{user}
	case strings.Contains(sql, "$1"):
		return []any{groups}
	default:
		return nil
	}
}

func (w *world) groupsOf(t testing.TB, user uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := w.owner.Query(ctx, `SELECT group_id FROM memberships WHERE user_id = $1`, user)
	must(t, err)
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var g uuid.UUID
		must(t, rows.Scan(&g))
		out = append(out, g)
	}
	must(t, rows.Err())
	return out
}

// counts returns, for each protected table, how many rows user sees through a plain
// count, and how many they should see.
func (w *world) counts(t testing.TB, pool *pgxpool.Pool, user uuid.UUID) (visible, expected, total map[string]int) {
	t.Helper()
	visible, expected, total = map[string]int{}, map[string]int{}, map[string]int{}
	groups := w.groupsOf(t, user)
	tx, err := dbscope.New(pool).Begin(ctx, user)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, s := range protected {
		var v, e, all int
		must(t, tx.QueryRow(ctx, `SELECT count(*) FROM `+s.name).Scan(&v))
		must(t, w.owner.QueryRow(ctx, s.expected, argsFor(s.expected, groups, user)...).Scan(&e))
		must(t, w.owner.QueryRow(ctx, `SELECT count(*) FROM `+s.name).Scan(&all))
		visible[s.name], expected[s.name], total[s.name] = v, e, all
	}
	return
}

// mismatches lists the tables in which user sees something other than they should.
func (w *world) mismatches(t testing.TB, pool *pgxpool.Pool, user uuid.UUID) []string {
	v, e, _ := w.counts(t, pool, user)
	var out []string
	for _, s := range protected {
		if v[s.name] != e[s.name] {
			out = append(out, fmt.Sprintf("%s: sees %d, should see %d", s.name, v[s.name], e[s.name]))
		}
	}
	return out
}

// Every user sees exactly the rows of their own groups in every table, with a query that
// does not mention groups at all. The positive control (the user sees a non-zero number
// of their own rows, and fewer than the table holds) is what makes "sees nothing of the
// other group" mean something: a policy that hid everything would fail it.
func TestEveryTableShowsOnlyTheCallersGroups(t *testing.T) {
	w := shared(t)
	for name, user := range map[string]uuid.UUID{"a1 (owner of A)": w.A.m1, "a2 (member of A)": w.A.m2, "b1 (owner of B)": w.B.m1} {
		t.Run(name, func(t *testing.T) {
			v, e, all := w.counts(t, w.app, user)
			for _, s := range protected {
				if v[s.name] != e[s.name] {
					t.Errorf("%s: sees %d rows, should see %d", s.name, v[s.name], e[s.name])
				}
				if e[s.name] == 0 {
					t.Errorf("%s: the user should see some rows (the fixture is empty or the expectation is wrong)", s.name)
				}
				if all[s.name] <= e[s.name] {
					// both groups have rows, so the table must hold more than the user's share
					t.Errorf("%s: the table holds %d rows and the user may see %d: the other group is not there to be hidden", s.name, all[s.name], e[s.name])
				}
			}
		})
	}
}

// A user who belongs to no group sees only themselves, and nothing else.
func TestAUserInNoGroupSeesOnlyThemselves(t *testing.T) {
	w := shared(t)
	v, e, _ := w.counts(t, w.app, w.x)
	for _, s := range protected {
		want := 0
		if s.name == "users" {
			want = 1
		}
		if v[s.name] != want || e[s.name] != want {
			t.Errorf("%s: sees %d (expected by the owner: %d), want %d", s.name, v[s.name], e[s.name], want)
		}
	}
}

// Without an identity, and with an identity nobody has, the application role sees no row
// in any table. The unscoped pool is the case of a query that escaped dbscope.
func TestNoIdentityAndUnknownIdentitySeeNothing(t *testing.T) {
	w := shared(t)
	for _, s := range protected {
		var n int
		must(t, w.app.QueryRow(ctx, `SELECT count(*) FROM `+s.name).Scan(&n))
		if n != 0 {
			t.Errorf("%s: %d rows visible with no identity", s.name, n)
		}
	}
	v, _, _ := w.counts(t, w.app, uuid.New())
	for _, s := range protected {
		if v[s.name] != 0 {
			t.Errorf("%s: %d rows visible to a user who does not exist", s.name, v[s.name])
		}
	}
}

// The real queries the handlers use that have no authorization of their own (their comments
// say "call it only after GetGroupForMember"): aimed at the other group's ids by a user who
// skipped that check, they find nothing; aimed at the user's own group, they find the data.
func TestHandlerQueriesWithoutTheirMembershipCheckFindNothingElsewhere(t *testing.T) {
	w := shared(t)
	q := dbscope.New(w.app).Reader(w.A.m1)

	for _, c := range []struct {
		name         string
		own, foreign func() (int, error)
	}{
		{"ListMembers", func() (int, error) { r, err := q.ListMembers(ctx, w.A.id); return len(r), err },
			func() (int, error) { r, err := q.ListMembers(ctx, w.B.id); return len(r), err }},
		{"ListJoinCodes", func() (int, error) { r, err := q.ListJoinCodes(ctx, w.A.id); return len(r), err },
			func() (int, error) { r, err := q.ListJoinCodes(ctx, w.B.id); return len(r), err }},
		{"ListExpenseItems", func() (int, error) { r, err := q.ListExpenseItems(ctx, w.A.exp1); return len(r), err },
			func() (int, error) { r, err := q.ListExpenseItems(ctx, w.B.exp1); return len(r), err }},
	} {
		own, err := c.own()
		must(t, err)
		foreign, err := c.foreign()
		must(t, err)
		if own == 0 {
			t.Errorf("%s: finds nothing in the user's own group (the control is broken)", c.name)
		}
		if foreign != 0 {
			t.Errorf("%s: finds %d rows in a group the user is not in", c.name, foreign)
		}
	}

	// And plain statements with no membership check at all.
	tx := w.as(t, w.A.m1)
	for _, c := range []struct{ name, sql string }{
		{"one expense by id", `SELECT count(*) FROM expenses WHERE id = $1`},
		{"a settlement by id", `SELECT count(*) FROM settlements WHERE id = $1`},
		{"a join code by id", `SELECT count(*) FROM join_codes WHERE id = $1`},
	} {
		ids := map[string][2]uuid.UUID{
			"one expense by id": {w.A.exp1, w.B.exp1}, "a settlement by id": {w.A.settlement, w.B.settlement}, "a join code by id": {w.A.codeID, w.B.codeID},
		}[c.name]
		var own, foreign int
		must(t, tx.QueryRow(ctx, c.sql, ids[0]).Scan(&own))
		must(t, tx.QueryRow(ctx, c.sql, ids[1]).Scan(&foreign))
		if own != 1 || foreign != 0 {
			t.Errorf("%s: own %d (want 1), foreign %d (want 0)", c.name, own, foreign)
		}
	}
	// Summing the whole ledger sees only the user's group (a balances query with no group filter).
	var sum, rows int
	must(t, tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(abs(amount_minor)), 0) FROM ledger_entries`).Scan(&rows, &sum))
	if rows != 2 || sum != 1000 {
		t.Errorf("the whole ledger as seen by a1: %d entries summing to %d in absolute value, want 2 and 1000 (group A's posting only)", rows, sum)
	}
}

// A pooled connection that served one user's transaction does not carry that user to the
// next query. One connection, so the next statement is certain to reuse it.
func TestAConnectionDoesNotCarryTheLastUserOver(t *testing.T) {
	w := shared(t)
	cfg := w.app.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	must(t, err)
	defer one.Close()

	tx, err := dbscope.New(one).Begin(ctx, w.A.m1)
	must(t, err)
	var n int
	must(t, tx.QueryRow(ctx, `SELECT count(*) FROM expenses`).Scan(&n))
	if n == 0 {
		t.Fatal("a1 sees no expenses inside the transaction (the control is broken)")
	}
	must(t, tx.Commit(ctx))
	must(t, one.QueryRow(ctx, `SELECT count(*) FROM expenses`).Scan(&n))
	if n != 0 {
		t.Fatalf("the next statement on the same connection sees %d expenses", n)
	}
	// The Reader (autocommit reads) the same way.
	if rows, err := db.New(one).ListMembers(ctx, w.A.id); err != nil || len(rows) != 0 {
		t.Fatalf("unscoped ListMembers on the reused connection: %d rows, err %v", len(rows), err)
	}
}
