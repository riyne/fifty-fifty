// Package stressrig is the rig for the M5 ledger stress test: the invariant checker
// (this file), the api started outside a test (api.go), and, from step 5.3, the workload.
//
// The checker recomputes every invariant from the database tables and from the payment
// provider's own record. It never looks at an API response, because the point of a
// negative control is to catch a response that said "ok" about state that is wrong.
package stressrig

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/allocator"
	"github.com/riyne/fifty-fifty/internal/ledger"
	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/settlerig"
)

// Invariant names one thing that must always hold. Counts are of violating objects
// (a ledger transaction, an expense, a settlement, a logical operation), not of rows.
type Invariant string

const (
	// ZeroSumTransaction: every ledger transaction's entries sum to zero. The database
	// enforces this (a deferred constraint trigger), so it should hold under every
	// negative control too; the report says so.
	ZeroSumTransaction Invariant = "ledger_transaction_not_zero"
	// ZeroSumGroup: every group's entries sum to zero.
	ZeroSumGroup Invariant = "group_not_zero"
	// ZeroSumLedger: all checked entries together sum to zero.
	ZeroSumLedger Invariant = "ledger_not_zero"
	// SharesMatchClaims: a finalized expense's ledger entries are exactly the entries
	// recomputed from its items, its claims, tax and tip (and a void is the exact
	// negation). A claim accepted after finalize, or a finalize that read a claim set
	// that then changed, shows up here.
	SharesMatchClaims Invariant = "ledger_differs_from_claims"
	// ExpenseState: an expense's status and its ledger postings disagree (finalized
	// with no posting, a posting on an expense that is still claiming, two postings...).
	ExpenseState Invariant = "expense_state_inconsistent"
	// SettlementPosting: a succeeded settlement does not have exactly one posting with
	// exactly the entries it implies, or another status has a posting.
	SettlementPosting Invariant = "settlement_posting_wrong"
	// DuplicateKey: one logical operation (one idempotency key, however often it was
	// sent) produced more than one expense or settlement.
	DuplicateKey Invariant = "duplicate_per_idempotency_key"
	// KeyOutcome: a logical operation's rows do not match what the client was told
	// (an acknowledged create with no row, a refused create with a row).
	KeyOutcome Invariant = "key_outcome_mismatch"
	// OverCapacity: in a group whose balances were frozen before settling, the
	// settlements that are not failed add up to more than a member owed or was owed.
	OverCapacity Invariant = "settlement_over_capacity"
	// EditStaleVersion: an item edit or confirm was applied to an expense that had moved
	// on since the version the client sent. Every such audit event records the version
	// it was made from (base_version) and the version it produced; with the optimistic
	// lock working, the second is always the first plus one.
	EditStaleVersion Invariant = "edit_applied_to_stale_version"
	// ProviderBooks: the ledger disagrees with the payment provider's record
	// (settlerig.Verify): succeeded != postings != charges, a double create or charge,
	// or a charge recorded as failed.
	ProviderBooks Invariant = "provider_books_differ"
)

// Invariants lists every invariant in report order.
var Invariants = []Invariant{
	ZeroSumTransaction, ZeroSumGroup, ZeroSumLedger, SharesMatchClaims, ExpenseState, EditStaleVersion,
	SettlementPosting, DuplicateKey, KeyOutcome, OverCapacity, ProviderBooks,
}

// Rows says how many rows a logical operation must have left behind.
type Rows int

const (
	// One: the client was told the operation succeeded (some attempt got a 2xx).
	One Rows = iota
	// Zero: every response was a refusal, so nothing may exist.
	Zero
	// ZeroOrOne: the client never saw an answer (a cancelled request), so either is fine
	// as long as it is not two.
	ZeroOrOne
)

// Create is one logical create (expense or settlement): every attempt that used the
// same idempotency key. The caller guarantees the identifying fields are unique per
// logical operation (an expense description that carries the operation's number; a
// settlement amount that no other operation in the group uses between the same pair).
type Create struct {
	Group uuid.UUID
	Rows  Rows

	// An expense is identified by its description.
	Description string
	// A settlement is identified by sender, payee and amount.
	Settlement bool
	From, To   uuid.UUID
	Amount     int64
}

// Frozen is a group whose balances did not change while settlements were created in
// it: Balance is each member's balance before the first settlement. Every non-failed
// settlement out of a member must fit in what the member owed (-Balance), and every
// one into a member in what they were owed (+Balance).
type Frozen struct {
	Group   uuid.UUID
	Balance map[uuid.UUID]int64
}

// Expect is what the harness knows from outside the database.
type Expect struct {
	// Groups limits the check to these groups. Nil checks every group in the database,
	// which is right for a scratch database and wrong for the shared development one.
	Groups  []uuid.UUID
	Creates []Create
	Frozen  []Frozen
	// Provider is the payment provider's record (settlerig.FetchProvider). Nil skips the
	// provider comparison.
	Provider *[]mockpay.Ledger
}

// Querier is a connection pool or a transaction. The tests run the checker inside a
// transaction they roll back, so deliberately corrupted state never reaches the shared
// test database.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Checked says how much the checker looked at, so a clean report is not mistaken for
// an empty one.
type Checked struct {
	Groups, Expenses, Finalized, Voided, Edits, LedgerTransactions, Settlements, SettlementPostings, Creates int
}

// Anomaly is one example of a violation.
type Anomaly struct {
	Invariant Invariant
	Detail    string
}

// MaxExamples caps the examples kept per invariant; the counts are always complete.
const MaxExamples = 5

// Report is the result of Check.
type Report struct {
	Checked  Checked
	Counts   map[Invariant]int
	Examples []Anomaly
}

func (r *Report) add(inv Invariant, format string, args ...any) {
	if r.Counts == nil {
		r.Counts = map[Invariant]int{}
	}
	r.Counts[inv]++
	if r.Counts[inv] <= MaxExamples {
		r.Examples = append(r.Examples, Anomaly{inv, fmt.Sprintf(format, args...)})
	}
}

// Total is the number of violations over all invariants.
func (r Report) Total() int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
}

// OK reports whether every invariant held.
func (r Report) OK() bool { return r.Total() == 0 }

// String lists the counts that are not zero, then the examples.
func (r Report) String() string {
	var b strings.Builder
	if r.OK() {
		return fmt.Sprintf("ok (%+v)", r.Checked)
	}
	for _, inv := range Invariants {
		if r.Counts[inv] > 0 {
			fmt.Fprintf(&b, "%s: %d\n", inv, r.Counts[inv])
		}
	}
	for _, a := range r.Examples {
		fmt.Fprintf(&b, "  %s: %s\n", a.Invariant, a.Detail)
	}
	return b.String()
}

// Balances returns every member's ledger balance in a group, for Frozen.
func Balances(ctx context.Context, q Querier, group uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := q.Query(ctx, `SELECT member_id, SUM(amount_minor)::bigint FROM ledger_entries WHERE group_id = $1 GROUP BY member_id`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var m uuid.UUID
		var sum int64
		if err := rows.Scan(&m, &sum); err != nil {
			return nil, err
		}
		out[m] = sum
	}
	return out, rows.Err()
}

// Check reads the database and reports every violated invariant. A non-nil error means
// the checker could not do its job, never that an invariant failed.
func Check(ctx context.Context, q Querier, exp Expect) (Report, error) {
	var rep Report
	groups := exp.Groups
	if groups == nil {
		var err error
		if groups, err = allGroups(ctx, q); err != nil {
			return rep, err
		}
	}
	rep.Checked.Groups = len(groups)
	for _, step := range []func(context.Context, Querier, []uuid.UUID, *Report) error{
		checkZeroSums,
		checkExpenses,
		checkEdits,
		checkSettlements,
	} {
		if err := step(ctx, q, groups, &rep); err != nil {
			return rep, err
		}
	}
	if err := checkCreates(ctx, q, groups, exp.Creates, &rep); err != nil {
		return rep, err
	}
	if err := checkCapacity(ctx, q, exp.Frozen, &rep); err != nil {
		return rep, err
	}
	if exp.Provider != nil {
		if err := checkProvider(ctx, q, groups, *exp.Provider, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// checkEdits reads the audit events of item edits and confirms (written in the same
// transaction as the change) and requires each to have been made from the version just
// before the one it produced. A lost update shows up here: two edits made from one
// version are both applied, the second one on top of a version it never saw.
func checkEdits(ctx context.Context, q Querier, groups []uuid.UUID, rep *Report) error {
	rows, err := q.Query(ctx, `
		SELECT data->>'expense_id', data->>'base_version', data->>'version', kind
		FROM audit_events
		WHERE kind IN ('expense.items_edited', 'expense.confirmed') AND group_id = ANY($1)`, groups)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var expense, kind string
		var base, version *string
		if err := rows.Scan(&expense, &base, &version, &kind); err != nil {
			return err
		}
		rep.Checked.Edits++
		want := ""
		if version != nil {
			var v int
			if _, err := fmt.Sscanf(*version, "%d", &v); err == nil {
				want = fmt.Sprint(v - 1)
			}
		}
		if base == nil || version == nil || *base != want {
			rep.add(EditStaleVersion, "%s of expense %s made from version %s produced version %s", kind, expense, deref(base), deref(version))
		}
	}
	return rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

func allGroups(ctx context.Context, q Querier) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `SELECT id FROM groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func checkZeroSums(ctx context.Context, q Querier, groups []uuid.UUID, rep *Report) error {
	rows, err := q.Query(ctx, `
		SELECT t.id, SUM(e.amount_minor)::text
		FROM ledger_transactions t JOIN ledger_entries e ON e.transaction_id = t.id
		WHERE t.group_id = ANY($1)
		GROUP BY t.id HAVING SUM(e.amount_minor) <> 0`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var sum string
		if err := rows.Scan(&id, &sum); err != nil {
			rows.Close()
			return err
		}
		rep.add(ZeroSumTransaction, "ledger transaction %s sums to %s", id, sum)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.Query(ctx, `SELECT group_id, SUM(amount_minor)::text FROM ledger_entries WHERE group_id = ANY($1) GROUP BY group_id HAVING SUM(amount_minor) <> 0`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var sum string
		if err := rows.Scan(&id, &sum); err != nil {
			rows.Close()
			return err
		}
		rep.add(ZeroSumGroup, "group %s sums to %s", id, sum)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var total string
	if err := q.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor), 0)::text FROM ledger_entries WHERE group_id = ANY($1)`, groups).Scan(&total); err != nil {
		return err
	}
	if total != "0" {
		rep.add(ZeroSumLedger, "all checked entries sum to %s", total)
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE group_id = ANY($1)`, groups).Scan(&n); err != nil {
		return err
	}
	rep.Checked.LedgerTransactions = n
	return nil
}

type expenseRow struct {
	id, payer uuid.UUID
	status    string
	tax, tip  int64
}

// postings loads every ledger entry of the groups, by transaction.
func postings(ctx context.Context, q Querier, groups []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]int64, error) {
	rows, err := q.Query(ctx, `SELECT transaction_id, member_id, amount_minor FROM ledger_entries WHERE group_id = ANY($1)`, groups)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]map[uuid.UUID]int64{}
	for rows.Next() {
		var tx, m uuid.UUID
		var amt int64
		if err := rows.Scan(&tx, &m, &amt); err != nil {
			return nil, err
		}
		if out[tx] == nil {
			out[tx] = map[uuid.UUID]int64{}
		}
		out[tx][m] += amt
	}
	return out, rows.Err()
}

func sameEntries(got map[uuid.UUID]int64, want []ledger.Entry) bool {
	if len(got) != len(want) {
		return false
	}
	for _, e := range want {
		if got[e.MemberID] != e.Amount {
			return false
		}
	}
	return true
}

func negated(m map[uuid.UUID]int64) []ledger.Entry {
	var out []ledger.Entry
	for id, a := range m {
		out = append(out, ledger.Entry{MemberID: id, Amount: -a})
	}
	return out
}

func checkExpenses(ctx context.Context, q Querier, groups []uuid.UUID, rep *Report) error {
	exps := map[uuid.UUID]expenseRow{}
	rows, err := q.Query(ctx, `SELECT id, payer_id, status, tax_minor, tip_minor FROM expenses WHERE group_id = ANY($1)`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var e expenseRow
		if err := rows.Scan(&e.id, &e.payer, &e.status, &e.tax, &e.tip); err != nil {
			rows.Close()
			return err
		}
		exps[e.id] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	itemsOf := map[uuid.UUID][]allocator.Item{} // by expense
	itemIdx := map[uuid.UUID]struct {
		exp uuid.UUID
		idx int
	}{}
	rows, err = q.Query(ctx, `SELECT i.id, i.expense_id, i.price_minor FROM expense_items i JOIN expenses e ON e.id = i.expense_id WHERE e.group_id = ANY($1) ORDER BY i.expense_id, i.position`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, exp uuid.UUID
		var price int64
		if err := rows.Scan(&id, &exp, &price); err != nil {
			rows.Close()
			return err
		}
		itemIdx[id] = struct {
			exp uuid.UUID
			idx int
		}{exp, len(itemsOf[exp])}
		itemsOf[exp] = append(itemsOf[exp], allocator.Item{ID: id, Price: price})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = q.Query(ctx, `SELECT item_id, member_id FROM claims WHERE group_id = ANY($1)`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item, m uuid.UUID
		if err := rows.Scan(&item, &m); err != nil {
			rows.Close()
			return err
		}
		at, ok := itemIdx[item]
		if !ok {
			rows.Close()
			return fmt.Errorf("claim on unknown item %s", item)
		}
		it := &itemsOf[at.exp][at.idx]
		it.Claimants = append(it.Claimants, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := postings(ctx, q, groups)
	if err != nil {
		return err
	}
	posting := map[uuid.UUID][]uuid.UUID{} // expense -> expense-kind transactions
	voids := map[uuid.UUID][]uuid.UUID{}
	rows, err = q.Query(ctx, `SELECT id, kind, expense_id FROM ledger_transactions WHERE group_id = ANY($1) AND kind IN ('expense', 'void')`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, exp uuid.UUID
		var kind string
		if err := rows.Scan(&id, &kind, &exp); err != nil {
			rows.Close()
			return err
		}
		if kind == "expense" {
			posting[exp] = append(posting[exp], id)
		} else {
			voids[exp] = append(voids[exp], id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rep.Checked.Expenses = len(exps)
	for id, e := range exps {
		np, nv := len(posting[id]), len(voids[id])
		wantP, wantV := 0, 0
		switch e.status {
		case "finalized":
			wantP = 1
		case "void":
			wantP, wantV = 1, 1
		}
		if np != wantP || nv != wantV {
			rep.add(ExpenseState, "expense %s is %s with %d postings and %d voids (want %d and %d)", id, e.status, np, nv, wantP, wantV)
			continue
		}
		if wantP == 0 {
			continue
		}
		rep.Checked.Finalized++
		members, err := allocator.Allocate(itemsOf[id], e.tax, e.tip)
		if err != nil {
			rep.add(SharesMatchClaims, "expense %s is %s but its claims cannot be allocated: %v", id, e.status, err)
			continue
		}
		want, err := ledger.ExpenseEntries(e.payer, members)
		if err != nil {
			return fmt.Errorf("expense %s: %w", id, err)
		}
		got := entries[posting[id][0]]
		if !sameEntries(got, want) {
			rep.add(SharesMatchClaims, "expense %s: ledger has %v, its claims imply %v", id, got, want)
		}
		if wantV == 1 {
			rep.Checked.Voided++
			if vg := entries[voids[id][0]]; !sameEntries(vg, negated(got)) {
				rep.add(SharesMatchClaims, "expense %s: the void has %v, not the negation of the posting %v", id, vg, got)
			}
		}
	}
	return nil
}

type settlementRow struct {
	id, group, from, to uuid.UUID
	amount              int64
	status              string
}

func loadSettlements(ctx context.Context, q Querier, groups []uuid.UUID) ([]settlementRow, error) {
	rows, err := q.Query(ctx, `SELECT id, group_id, from_member, to_member, amount_minor, status FROM settlements WHERE group_id = ANY($1) ORDER BY id`, groups)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []settlementRow
	for rows.Next() {
		var s settlementRow
		if err := rows.Scan(&s.id, &s.group, &s.from, &s.to, &s.amount, &s.status); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func checkSettlements(ctx context.Context, q Querier, groups []uuid.UUID, rep *Report) error {
	sets, err := loadSettlements(ctx, q, groups)
	if err != nil {
		return err
	}
	entries, err := postings(ctx, q, groups)
	if err != nil {
		return err
	}
	postOf := map[uuid.UUID][]uuid.UUID{}
	rows, err := q.Query(ctx, `SELECT id, settlement_id FROM ledger_transactions WHERE group_id = ANY($1) AND kind = 'settlement'`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, sid uuid.UUID
		if err := rows.Scan(&id, &sid); err != nil {
			rows.Close()
			return err
		}
		postOf[sid] = append(postOf[sid], id)
		rep.Checked.SettlementPostings++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rep.Checked.Settlements = len(sets)
	for _, s := range sets {
		ps := postOf[s.id]
		if s.status != "succeeded" {
			if len(ps) != 0 {
				rep.add(SettlementPosting, "settlement %s is %s but has %d postings", s.id, s.status, len(ps))
			}
			continue
		}
		if len(ps) != 1 {
			rep.add(SettlementPosting, "settlement %s succeeded with %d postings", s.id, len(ps))
			continue
		}
		want, err := ledger.SettlementEntries(s.from, s.to, s.amount)
		if err != nil {
			return fmt.Errorf("settlement %s: %w", s.id, err)
		}
		if got := entries[ps[0]]; !sameEntries(got, want) {
			rep.add(SettlementPosting, "settlement %s of %d: the posting has %v, want %v", s.id, s.amount, got, want)
		}
	}
	return nil
}

func checkCreates(ctx context.Context, q Querier, groups []uuid.UUID, creates []Create, rep *Report) error {
	rep.Checked.Creates = len(creates)
	if len(creates) == 0 {
		return nil
	}
	type expKey struct {
		group uuid.UUID
		desc  string
	}
	type setKey struct {
		group, from, to uuid.UUID
		amount          int64
	}
	exps := map[expKey]int{}
	rows, err := q.Query(ctx, `SELECT group_id, description, count(*)::int FROM expenses WHERE group_id = ANY($1) GROUP BY group_id, description`, groups)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k expKey
		var n int
		if err := rows.Scan(&k.group, &k.desc, &n); err != nil {
			rows.Close()
			return err
		}
		exps[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sets := map[setKey]int{}
	all, err := loadSettlements(ctx, q, groups)
	if err != nil {
		return err
	}
	for _, s := range all {
		sets[setKey{s.group, s.from, s.to, s.amount}]++
	}

	for _, c := range creates {
		var n int
		var what string
		if c.Settlement {
			n = sets[setKey{c.Group, c.From, c.To, c.Amount}]
			what = fmt.Sprintf("settlement %s -> %s of %d", c.From, c.To, c.Amount)
		} else {
			n = exps[expKey{c.Group, c.Description}]
			what = fmt.Sprintf("expense %q", c.Description)
		}
		switch {
		case n > 1:
			rep.add(DuplicateKey, "%s in group %s exists %d times for one key", what, c.Group, n)
		case c.Rows == One && n == 0:
			rep.add(KeyOutcome, "%s in group %s was acknowledged but does not exist", what, c.Group)
		case c.Rows == Zero && n != 0:
			rep.add(KeyOutcome, "%s in group %s was refused but exists", what, c.Group)
		}
	}
	return nil
}

func checkCapacity(ctx context.Context, q Querier, frozen []Frozen, rep *Report) error {
	for _, f := range frozen {
		sets, err := loadSettlements(ctx, q, []uuid.UUID{f.Group})
		if err != nil {
			return err
		}
		out, in := map[uuid.UUID]int64{}, map[uuid.UUID]int64{}
		for _, s := range sets {
			if s.status == "failed" {
				continue
			}
			out[s.from] += s.amount
			in[s.to] += s.amount
		}
		for m, sum := range out {
			if owed := max(0, -f.Balance[m]); sum > owed {
				rep.add(OverCapacity, "group %s: member %s sends %d in settlements but owed %d", f.Group, m, sum, owed)
			}
		}
		for m, sum := range in {
			if due := max(0, f.Balance[m]); sum > due {
				rep.add(OverCapacity, "group %s: member %s receives %d in settlements but was owed %d", f.Group, m, sum, due)
			}
		}
	}
	return nil
}

func checkProvider(ctx context.Context, q Querier, groups []uuid.UUID, provider []mockpay.Ledger, rep *Report) error {
	sets, err := loadSettlements(ctx, q, groups)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(sets))
	for i, s := range sets {
		ids[i] = s.id
	}
	vr, err := settlerig.Verify(ctx, q, provider, ids)
	if err != nil {
		return err
	}
	for _, a := range vr.Anomalies {
		rep.add(ProviderBooks, "%s", a)
	}
	return nil
}
