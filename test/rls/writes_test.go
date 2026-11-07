package rls_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Each case is one statement shape, built for a group. It is run as a1 (a member of A only)
// against A's rows, where row-level security must NOT be what refuses it, and against B's
// rows, where it must be. Running the same statement against both is what keeps a typo in
// the statement from "passing" as a refusal.
type writeCase struct {
	name string
	sql  func(g grp, other uuid.UUID) string
	args func(g grp, other uuid.UUID) []any
}

func args(v ...any) func(grp, uuid.UUID) []any { return func(grp, uuid.UUID) []any { return v } }

var inserts = []writeCase{
	{"expenses", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO expenses (group_id, payer_id, description, status) VALUES ($1, $2, 'x', 'claiming')`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.id, g.m1} }},
	{"expense_items", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, 9, 'x', 1)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.exp2} }},
	{"claims", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.item2b, g.exp2, g.id, g.m2} }},
	{"ledger_transactions", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO ledger_transactions (group_id, kind, expense_id, created_by) VALUES ($1, 'expense', $2, $3)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.id, g.exp4, g.m1} }},
	// The ledger's own rule (entries only in the transaction that created the posting)
	// refuses this for every group before the policy is evaluated, and a header for another
	// group cannot be created to get past it (that insert IS refused by the policy, above).
	// So for this table the policy's protection is the one tested by the visibility tests.
	{"ledger_entries", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ($1, $2, $3, 1)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.tx, g.id, g.m2} }},
	{"settlements", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 5)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.id, g.m2, g.m1} }},
	{"receipts", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO receipts (expense_id, group_id, uploader_id, object_key, content_type, size_bytes, width_px, height_px)
		        VALUES ($1, $2, $3, repeat('b', 64), 'image/png', 1, 1, 1)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.exp2, g.id, g.m1} }},
	{"audit_events", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO audit_events (actor_user_id, group_id, kind) VALUES ($1, $2, 'x')`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.m1, g.id} }},
	{"join_codes", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO join_codes (group_id, code_hash, created_by, max_uses, expires_at)
		        VALUES ($1, decode(md5(random()::text), 'hex'), $2, 1, now() + interval '1 hour')`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.id, g.m1} }},
	{"groups (created_by someone else)", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO groups (name, currency, created_by) VALUES ('x', 'CAD', $1)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.m1} }},
	{"idempotency_keys (another user's)", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
		        VALUES ($1, 'k' || random()::text, '\x01', 200, '\x7b7d', now() + interval '1 hour')`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.m1} }},
	{"receipt_quota (another user's)", func(g grp, _ uuid.UUID) string {
		return `INSERT INTO receipt_quota (user_id, day, used) VALUES ($1, current_date + 5, 1)`
	}, func(g grp, _ uuid.UUID) []any { return []any{g.m1} }},
}

// shadowed names the tables whose insert is refused by another rule before the policy is
// evaluated (a BEFORE trigger runs ahead of the row-level security check), with that rule's
// SQLSTATE. For them the foreign statement must be refused with that code, and RLS is not
// what an insert exercises there.
var shadowed = map[string]string{"ledger_entries": "23001"}

func TestInsertsIntoAnotherGroupAreRefusedByThePolicy(t *testing.T) {
	w := shared(t)
	for _, c := range inserts {
		t.Run(c.name, func(t *testing.T) {
			// Own group: row-level security does not refuse it (it may succeed or be
			// refused by a constraint or trigger, which is not what this test is about).
			tx := w.as(t, w.A.m1)
			if _, err := tx.Exec(ctx, c.sql(w.A, w.x), c.args(w.A, w.x)...); rlsRefused(err) {
				t.Fatalf("own group: refused by row-level security: %v", err)
			}
			// Another group: refused by it.
			tx = w.as(t, w.A.m1)
			_, err := tx.Exec(ctx, c.sql(w.B, w.x), c.args(w.B, w.x)...)
			if code, ok := shadowed[c.name]; ok {
				var pg *pgconn.PgError
				if !errors.As(err, &pg) || pg.Code != code {
					t.Fatalf("another group's: want SQLSTATE %s from the rule that runs before the policy, got %v", code, err)
				}
				t.Logf("%s: refused by a rule that runs before the policy (SQLSTATE %s); the policy's protection of this table is tested by visibility", c.name, code)
				return
			}
			if !rlsRefused(err) {
				t.Fatalf("another group's: want a row-level security refusal, got %v", err)
			}
		})
	}
}

// Nobody can make anybody a member except through a join code or by creating the group.
func TestMembershipsCannotBeAddedByRawSQL(t *testing.T) {
	w := shared(t)
	refused := []struct {
		name string
		sql  string
		args []any
	}{
		{"yourself into someone else's group, as a member", `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, []any{w.B.id, w.A.m1}},
		{"yourself into someone else's group, as owner", `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'owner')`, []any{w.B.id, w.A.m1}},
		{"someone else into your own group", `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, []any{w.A.id, w.x}},
		{"yourself as owner of a group you did not create", `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'owner')`, []any{w.A.id, w.A.m2}},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			tx := w.as(t, w.A.m1)
			if _, err := tx.Exec(ctx, c.sql, c.args...); !rlsRefused(err) {
				t.Fatalf("want a row-level security refusal, got %v", err)
			}
		})
	}
	// m2 (a plain member of A) cannot promote or re-add themselves either.
	tx := w.as(t, w.A.m2)
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'owner')`, w.A.id, w.A.m2); !rlsRefused(err) {
		t.Fatalf("a member adding an owner row for themselves: got %v", err)
	}

	// The control: creating a group and its owner row, in one transaction, works, and the
	// creator then sees the group.
	tx = w.as(t, w.x)
	var gid uuid.UUID
	must(t, tx.QueryRow(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ('mine', 'CAD', $1) RETURNING id`, w.x).Scan(&gid))
	_, err := tx.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'owner')`, gid, w.x)
	must(t, err)
	var n int
	must(t, tx.QueryRow(ctx, `SELECT count(*) FROM groups WHERE id = $1`, gid).Scan(&n))
	if n != 1 {
		t.Fatalf("the creator sees %d rows of their new group", n)
	}

	// The one row a creator may insert is their own OWNER row; a plain member row for
	// themselves (not what the app does, and nothing the policy needs to allow) is refused.
	tx = w.as(t, w.x)
	must(t, tx.QueryRow(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ('mine2', 'CAD', $1) RETURNING id`, w.x).Scan(&gid))
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, gid, w.x); !rlsRefused(err) {
		t.Fatalf("a creator adding themselves as a plain member: want a row-level security refusal, got %v", err)
	}
}

// Users are written by fifty_system only; fifty_app cannot insert one, or change one, not
// even its own.
func TestTheAppRoleCannotWriteUsers(t *testing.T) {
	w := shared(t)
	tx := w.as(t, w.A.m1)
	if _, err := tx.Exec(ctx, `INSERT INTO users (issuer, subject) VALUES ('rls', 'new')`); !rlsRefused(err) {
		t.Fatalf("insert: want a row-level security refusal, got %v", err)
	}
	tag, err := w.as(t, w.A.m1).Exec(ctx, `UPDATE users SET display_name = 'z' WHERE id = $1`, w.A.m1)
	must(t, err)
	if tag.RowsAffected() != 0 {
		t.Fatalf("update of own user row affected %d rows, want 0", tag.RowsAffected())
	}
}

type changeCase struct {
	name string
	sql  func(g grp) string
	args func(g grp) []any
}

var changes = []changeCase{
	{"UPDATE expenses", func(g grp) string { return `UPDATE expenses SET description = 'y' WHERE id = $1` }, func(g grp) []any { return []any{g.exp2} }},
	{"UPDATE expense_items", func(g grp) string { return `UPDATE expense_items SET price_minor = 2 WHERE id = $1` }, func(g grp) []any { return []any{g.item2b} }},
	{"DELETE claims", func(g grp) string { return `DELETE FROM claims WHERE item_id = $1 AND member_id = $2` }, func(g grp) []any { return []any{g.item2a, g.m1} }},
	{"UPDATE settlements", func(g grp) string { return `UPDATE settlements SET amount_minor = 101 WHERE id = $1` }, func(g grp) []any { return []any{g.settlement} }},
	{"UPDATE join_codes (revoke)", func(g grp) string { return `UPDATE join_codes SET revoked_at = now() WHERE id = $1` }, func(g grp) []any { return []any{g.codeID} }},
	{"UPDATE receipts", func(g grp) string { return `UPDATE receipts SET size_bytes = 2 WHERE id = $1` }, func(g grp) []any { return []any{g.receipt} }},
	{"UPDATE groups", func(g grp) string { return `UPDATE groups SET name = 'n' WHERE id = $1` }, func(g grp) []any { return []any{g.id} }},
	{"UPDATE idempotency_keys", func(g grp) string { return `UPDATE idempotency_keys SET response_status = 201 WHERE user_id = $1` }, func(g grp) []any { return []any{g.m1} }},
	{"DELETE idempotency_keys", func(g grp) string { return `DELETE FROM idempotency_keys WHERE user_id = $1` }, func(g grp) []any { return []any{g.m1} }},
	{"DELETE receipt_quota", func(g grp) string { return `DELETE FROM receipt_quota WHERE user_id = $1` }, func(g grp) []any { return []any{g.m1} }},
}

// Updates and deletes of another group's rows touch nothing (the rows are invisible to the
// statement), while the same statement on the user's own group touches one.
func TestChangesToAnotherGroupsRowsAffectNothing(t *testing.T) {
	w := shared(t)
	for _, c := range changes {
		t.Run(c.name, func(t *testing.T) {
			own, err := w.as(t, w.A.m1).Exec(ctx, c.sql(w.A), c.args(w.A)...)
			must(t, err)
			if own.RowsAffected() == 0 {
				t.Fatalf("on the user's own group the statement affected 0 rows: the control is broken")
			}
			foreign, err := w.as(t, w.A.m1).Exec(ctx, c.sql(w.B), c.args(w.B)...)
			must(t, err)
			if foreign.RowsAffected() != 0 {
				t.Fatalf("on another group it affected %d rows", foreign.RowsAffected())
			}
		})
	}
	// Rows that are rolled back above: confirm B's data is as seeded, as the owner sees it.
	var desc string
	must(t, w.owner.QueryRow(ctx, `SELECT description FROM expenses WHERE id = $1`, w.B.exp2).Scan(&desc))
	if desc != "dinner" {
		t.Fatalf("group B's expense was changed: %q", desc)
	}
}

// A row cannot be moved into a group the caller is not in, and a membership row cannot be
// changed or removed by the app at all.
func TestRowsCannotBeMovedAndMembershipsCannotBeChanged(t *testing.T) {
	w := shared(t)
	_, err := w.as(t, w.A.m1).Exec(ctx, `UPDATE expenses SET group_id = $1 WHERE id = $2`, w.B.id, w.A.exp2)
	if !rlsRefused(err) {
		t.Fatalf("moving an expense into another group: want a row-level security refusal, got %v", err)
	}
	for _, sql := range []string{
		`DELETE FROM memberships WHERE group_id = $1 AND user_id = $2`,
		`UPDATE memberships SET role = 'owner' WHERE group_id = $1 AND user_id = $2`,
	} {
		tag, err := w.as(t, w.A.m1).Exec(ctx, sql, w.A.id, w.A.m2)
		must(t, err)
		if tag.RowsAffected() != 0 {
			t.Fatalf("%s affected %d rows of the user's own group, want 0 (the app never changes memberships)", sql, tag.RowsAffected())
		}
	}
}
