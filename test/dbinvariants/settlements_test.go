package dbinvariants_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Settlements and settlement postings (migration 00011): raw SQL that breaks a rule
// must be refused by the database, whatever the Go code does.

func (w *world) settlement(group, from, to uuid.UUID, amount int64) (uuid.UUID, error) {
	var id uuid.UUID
	err := w.pool.QueryRow(ctx,
		`INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, $4) RETURNING id`,
		group, from, to, amount).Scan(&id)
	return id, err
}

func (w *world) mustSettlement(t *testing.T, from, to uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	id, err := w.settlement(w.g, from, to, amount)
	must(t, err)
	return id
}

// postSettlement writes a settlement posting (header and entries) in one database
// transaction, the way settlements.Resolve does.
func (w *world) postSettlement(settlement, group, createdBy uuid.UUID, entries ...entry) (uuid.UUID, error) {
	var id uuid.UUID
	err := inTx(w.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO ledger_transactions (group_id, kind, settlement_id, created_by) VALUES ($1, 'settlement', $2, $3) RETURNING id`,
			group, settlement, createdBy).Scan(&id); err != nil {
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

func TestSettlementRowsAreValidated(t *testing.T) {
	w := newWorld(t)

	tests := []struct {
		name string
		sql  string
		args []any
		want string
	}{
		{"valid", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 100)`,
			[]any{w.g, w.b, w.payer}, noErrExpected},
		{"zero amount", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 0)`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"negative amount", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, -5)`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"paying yourself", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $2, 100)`,
			[]any{w.g, w.b}, codeCheck},
		{"sender not in the group", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 100)`,
			[]any{w.g, w.outsider, w.payer}, codeFK},
		{"payee not in the group", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 100)`,
			[]any{w.g, w.payer, w.outsider}, codeFK},
		{"unknown status", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status) VALUES ($1, $2, $3, 100, 'refunded')`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"failed without a failure code", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status, resolved_at) VALUES ($1, $2, $3, 100, 'failed', now())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"failure code on a pending settlement", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, failure_code) VALUES ($1, $2, $3, 100, 'declined')`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"empty failure code", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status, failure_code, resolved_at) VALUES ($1, $2, $3, 100, 'failed', '', now())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"failed with a ledger posting", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status, failure_code, resolved_at, ledger_transaction_id) VALUES ($1, $2, $3, 100, 'failed', 'x', now(), gen_random_uuid())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"succeeded without a ledger posting", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status, resolved_at) VALUES ($1, $2, $3, 100, 'succeeded', now())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"pending with a ledger posting", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, ledger_transaction_id) VALUES ($1, $2, $3, 100, gen_random_uuid())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"resolved_at on a pending settlement", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, resolved_at) VALUES ($1, $2, $3, 100, now())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"failed without resolved_at", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status, failure_code) VALUES ($1, $2, $3, 100, 'failed', 'x')`,
			[]any{w.g, w.b, w.payer}, codeCheck},
		{"unknown with resolved_at", `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, status, resolved_at) VALUES ($1, $2, $3, 100, 'unknown', now())`,
			[]any{w.g, w.b, w.payer}, codeCheck},
	}
	for _, tc := range tests {
		_, err := w.pool.Exec(ctx, tc.sql, tc.args...)
		wantCode(t, err, tc.want, tc.name)
	}
}

func TestSettlementPostings(t *testing.T) {
	w := newWorld(t)
	s := w.mustSettlement(t, w.b, w.payer, 700)
	good := []entry{{w.b, 700}, {w.payer, -700}}

	// Unbalanced, and a posting in a group that is not the settlement's.
	_, err := w.postSettlement(s, w.g, w.b, entry{w.b, 700}, entry{w.payer, -600})
	wantCode(t, err, codeCheck, "unbalanced settlement posting")
	_, err = w.postSettlement(s, w.h, w.outsider)
	wantCode(t, err, codeFK, "settlement posting in another group")

	first, err := w.postSettlement(s, w.g, w.b, good...)
	wantCode(t, err, noErrExpected, "first posting")
	must(t, err)

	// The unique index: a second posting for the same settlement, balanced or not.
	_, err = w.postSettlement(s, w.g, w.b, good...)
	wantCode(t, err, codeUnique, "second posting for one settlement")
	_, err = w.postSettlement(s, w.g, w.b)
	wantCode(t, err, codeUnique, "second, empty posting for one settlement")

	// Entries cannot be appended once the posting committed (same rule as expenses).
	err = inTx(w.pool, func(tx pgx.Tx) error {
		for _, e := range []entry{{w.c, 5}, {w.b, -5}} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ($1, $2, $3, $4)`,
				first, w.g, e.member, e.amount); err != nil {
				return err
			}
		}
		return nil
	})
	wantCode(t, err, codeRestrict, "balanced pair appended to a committed settlement posting")

	// Header shape: a settlement posting names a settlement and nothing else, and
	// the other kinds do not carry a settlement id.
	exp := w.finalizedExpense(t)
	shape := []struct {
		name string
		sql  string
		args []any
	}{
		{"settlement kind without a settlement id", `INSERT INTO ledger_transactions (group_id, kind, created_by) VALUES ($1, 'settlement', $2)`, []any{w.g, w.b}},
		{"settlement kind that also names an expense", `INSERT INTO ledger_transactions (group_id, kind, settlement_id, expense_id, created_by) VALUES ($1, 'settlement', $3, $4, $2)`,
			[]any{w.g, w.b, w.mustSettlement(t, w.b, w.c, 1), exp}},
		{"expense kind that names a settlement", `INSERT INTO ledger_transactions (group_id, kind, expense_id, settlement_id, created_by) VALUES ($1, 'expense', $3, $4, $2)`,
			[]any{w.g, w.b, w.finalizedExpense(t), w.mustSettlement(t, w.b, w.c, 1)}},
		{"unknown kind", `INSERT INTO ledger_transactions (group_id, kind, settlement_id, created_by) VALUES ($1, 'refund', $3, $2)`,
			[]any{w.g, w.b, w.mustSettlement(t, w.b, w.c, 1)}},
	}
	for _, tc := range shape {
		_, err := w.pool.Exec(ctx, tc.sql, tc.args...)
		wantCode(t, err, codeCheck, tc.name)
	}
}

// A settlement can only be marked succeeded with its own posting: not with another
// settlement's, and not with an expense posting.
func TestSettlementPointsAtItsOwnPosting(t *testing.T) {
	w := newWorld(t)
	s1 := w.mustSettlement(t, w.b, w.payer, 700)
	s2 := w.mustSettlement(t, w.c, w.payer, 300)
	p1, err := w.postSettlement(s1, w.g, w.b, entry{w.b, 700}, entry{w.payer, -700})
	must(t, err)
	expensePosting, err := w.post("expense", w.g, w.finalizedExpense(t), nil, w.payer, entry{w.payer, 10}, entry{w.c, -10})
	must(t, err)

	mark := `UPDATE settlements SET status = 'succeeded', ledger_transaction_id = $2, resolved_at = now() WHERE id = $1`
	for name, posting := range map[string]uuid.UUID{
		"another settlement's posting":  p1,
		"an expense posting":            expensePosting,
		"a posting that does not exist": uuid.New(),
	} {
		err := inTxAlwaysRollback(w.pool, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, mark, s2, posting); return err })
		wantCode(t, err, codeFK, "settlement marked succeeded with "+name)
	}

	// Its own posting is accepted.
	_, err = w.pool.Exec(ctx, mark, s1, p1)
	wantCode(t, err, noErrExpected, "settlement marked succeeded with its own posting")
}
