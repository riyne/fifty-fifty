package stressrig_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/stressrig"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

// world is a small, healthy, committed state in the shared test database: one group
// with a payer and two members; one finalized expense, one finalized and then voided,
// one still claiming; one succeeded settlement (with its posting) and one pending.
type world struct {
	s                    *teststack.Stack
	pool                 *pgxpool.Pool
	group                uuid.UUID
	payer, a, b          uuid.UUID
	e1, e2, e3           uuid.UUID
	d1, d2, d3           string // expense descriptions (the idempotency "tags")
	s1, s2               uuid.UUID
	other                uuid.UUID // an unrelated group
	payerTok, aTok, bTok string
}

func (w *world) path(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func newWorld(t *testing.T) *world {
	t.Helper()
	s := teststack.New(t)
	w := &world{s: s, pool: s.Pool}
	w.payerTok, w.aTok, w.bTok = s.Token(t, "payer"), s.Token(t, "a"), s.Token(t, "b")
	group := s.CreateGroup(t, w.payerTok, "check")
	w.group = uuid.MustParse(group)
	for _, tok := range []string{w.aTok, w.bTok} {
		_, code := s.CreateJoinCode(t, w.payerTok, group, 0, 0)
		if r := s.Join(t, tok, code); r.Status != http.StatusCreated {
			t.Fatalf("join = %d: %s", r.Status, r.Body)
		}
	}
	w.payer, w.a, w.b = s.UserID(t, w.payerTok), s.UserID(t, w.aTok), s.UserID(t, w.bTok)
	w.other = uuid.MustParse(s.CreateGroup(t, s.Token(t, "loner"), "elsewhere"))

	tag := func(n string) string { return n + "-" + uuid.NewString() }
	w.d1, w.d2, w.d3 = tag("e1"), tag("e2"), tag("e3")
	mk := func(desc string, items ...[2]any) (uuid.UUID, []string) {
		body := `{"description":"` + desc + `","items":[`
		for i, it := range items {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(`{"description":%q,"price_minor":%d}`, it[0], it[1])
		}
		body += `],"tax_minor":300,"tip_minor":200}`
		id := s.CreateExpense(t, w.payerTok, group, body)
		var e struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		s.Do(t, w.payerTok, "GET", "/v1/groups/"+group+"/expenses/"+id, "").JSON(t, &e)
		ids := make([]string, len(e.Items))
		for i, it := range e.Items {
			ids[i] = it.ID
		}
		return uuid.MustParse(id), ids
	}
	claim := func(tok, exp, item string) {
		if r := s.Do(t, tok, "PUT", "/v1/groups/"+group+"/expenses/"+exp+"/items/"+item+"/claim", ""); r.Status != 200 {
			t.Fatalf("claim = %d: %s", r.Status, r.Body)
		}
	}
	post := func(tok, exp, verb string) {
		r := s.Do(t, tok, "POST", "/v1/groups/"+group+"/expenses/"+exp+"/"+verb, "", "Idempotency-Key", uuid.NewString())
		if r.Status != 200 {
			t.Fatalf("%s = %d: %s", verb, r.Status, r.Body)
		}
	}

	var items []string
	w.e1, items = mk(w.d1, [2]any{"pizza", 1500}, [2]any{"beer", 800})
	claim(w.aTok, w.e1.String(), items[0])
	claim(w.payerTok, w.e1.String(), items[0])
	claim(w.bTok, w.e1.String(), items[1])
	post(w.payerTok, w.e1.String(), "finalize")

	w.e2, items = mk(w.d2, [2]any{"soup", 1000})
	claim(w.aTok, w.e2.String(), items[0])
	post(w.payerTok, w.e2.String(), "finalize")
	post(w.payerTok, w.e2.String(), "void")

	w.e3, items = mk(w.d3, [2]any{"tea", 500})
	claim(w.bTok, w.e3.String(), items[0])

	ctx := context.Background()
	q := db.New(w.pool)
	s1, err := q.CreateSettlement(ctx, db.CreateSettlementParams{GroupID: w.group, FromMember: w.a, ToMember: w.payer, AmountMinor: 100})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := q.CreateSettlement(ctx, db.CreateSettlementParams{GroupID: w.group, FromMember: w.a, ToMember: w.payer, AmountMinor: 50})
	if err != nil {
		t.Fatal(err)
	}
	w.s1, w.s2 = s1.ID, s2.ID
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := settlements.Resolve(ctx, tx, w.s1, settlements.Success()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// expect is the harness's knowledge of the world: every logical create, as if each had
// been sent once and acknowledged.
func (w *world) expect() stressrig.Expect {
	return stressrig.Expect{
		Groups: []uuid.UUID{w.group},
		Creates: []stressrig.Create{
			{Group: w.group, Description: w.d1, Rows: stressrig.One},
			{Group: w.group, Description: w.d2, Rows: stressrig.One},
			{Group: w.group, Description: w.d3, Rows: stressrig.ZeroOrOne},
			{Group: w.group, Settlement: true, From: w.a, To: w.payer, Amount: 100, Rows: stressrig.One},
			{Group: w.group, Settlement: true, From: w.a, To: w.payer, Amount: 50, Rows: stressrig.One},
			{Group: w.group, Settlement: true, From: w.a, To: w.payer, Amount: 999, Rows: stressrig.Zero},
		},
	}
}

// inTx runs fn in a transaction that is always rolled back, so corrupted state never
// reaches the shared database.
func (w *world) inTx(t *testing.T, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	fn(tx)
}

func mustExec(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// requireOnly fails unless exactly the wanted invariants were violated: a corruption
// must be seen by the invariant it breaks, and by no other, or the checker is blurry.
func requireOnly(t *testing.T, rep stressrig.Report, want ...stressrig.Invariant) {
	t.Helper()
	var got []stressrig.Invariant
	for inv, n := range rep.Counts {
		if n > 0 {
			got = append(got, inv)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("violated %v, want %v\n%s", got, want, rep)
	}
}

func check(t *testing.T, tx pgx.Tx, exp stressrig.Expect) stressrig.Report {
	t.Helper()
	rep, err := stressrig.Check(context.Background(), tx, exp)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestCheck_HealthyWorldIsClean(t *testing.T) {
	w := newWorld(t)
	rep, err := stressrig.Check(context.Background(), w.pool, w.expect())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("a healthy world reported anomalies:\n%s", rep)
	}
	// A clean report is only worth something if the checker looked at something.
	want := stressrig.Checked{Groups: 1, Expenses: 3, Finalized: 2, Voided: 1, LedgerTransactions: 4, Settlements: 2, SettlementPostings: 1, Creates: 6}
	if rep.Checked != want {
		t.Fatalf("checked %+v, want %+v", rep.Checked, want)
	}
}

func TestCheck_ScopedToItsGroups(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `UPDATE expenses SET status = 'finalized', finalized_at = now() WHERE id = $1`, w.e3) // a corruption in w.group
		rep := check(t, tx, stressrig.Expect{Groups: []uuid.UUID{w.other}})
		if !rep.OK() || rep.Checked.Groups != 1 || rep.Checked.Expenses != 0 {
			t.Fatalf("a check of another group saw this group's state:\n%s %+v", rep, rep.Checked)
		}
		if rep := check(t, tx, stressrig.Expect{Groups: []uuid.UUID{w.group}}); rep.OK() {
			t.Fatal("the same corruption must be seen when its group is checked")
		}
	})
}

func TestCheck_UnbalancedLedgerTransaction(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		// One entry and no counterpart; the deferred zero-sum trigger would refuse this at
		// COMMIT, which a rolled-back transaction never reaches. The checker must see it
		// without the database's help.
		mustExec(t, tx, `INSERT INTO ledger_transactions (id, group_id, kind, settlement_id, created_by)
			VALUES ('00000000-0000-0000-0000-0000000000aa', $1, 'settlement', $2, $3)`, w.group, w.s2, w.a)
		mustExec(t, tx, `INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor)
			VALUES ('00000000-0000-0000-0000-0000000000aa', $1, $2, 100)`, w.group, w.a)
		rep := check(t, tx, w.expect())
		requireOnly(t, rep, stressrig.ZeroSumTransaction, stressrig.ZeroSumGroup, stressrig.ZeroSumLedger, stressrig.SettlementPosting)
	})
}

func TestCheck_ClaimAddedAfterFinalize(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO claims (item_id, expense_id, group_id, member_id)
			SELECT i.id, i.expense_id, $2, $3 FROM expense_items i WHERE i.expense_id = $1 AND i.description = 'beer'`, w.e1, w.group, w.a)
		requireOnly(t, check(t, tx, w.expect()), stressrig.SharesMatchClaims)
	})
}

func TestCheck_ClaimRemovedAfterFinalize(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `DELETE FROM claims WHERE expense_id = $1 AND member_id = $2`, w.e1, w.a)
		requireOnly(t, check(t, tx, w.expect()), stressrig.SharesMatchClaims)
	})
}

func TestCheck_FinalizedItemLeftWithoutAClaimant(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `DELETE FROM claims WHERE expense_id = $1 AND item_id IN (SELECT id FROM expense_items WHERE expense_id = $1 AND description = 'beer')`, w.e1)
		requireOnly(t, check(t, tx, w.expect()), stressrig.SharesMatchClaims)
	})
}

func TestCheck_ExpenseStatusDisagreesWithLedger(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `UPDATE expenses SET status = 'finalized', finalized_at = now() WHERE id = $1`, w.e3) // no posting
		requireOnly(t, check(t, tx, w.expect()), stressrig.ExpenseState)
	})
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `UPDATE expenses SET status = 'claiming', finalized_at = NULL WHERE id = $1`, w.e1) // posting on a claiming expense
		requireOnly(t, check(t, tx, w.expect()), stressrig.ExpenseState)
	})
}

func TestCheck_VoidThatIsNotTheNegation(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO ledger_transactions (id, group_id, kind, expense_id, reverses_id, created_by)
			SELECT '00000000-0000-0000-0000-0000000000bb', $1, 'void', $2, t.id, $3 FROM ledger_transactions t WHERE t.expense_id = $2 AND t.kind = 'expense'`, w.group, w.e1, w.payer)
		mustExec(t, tx, `INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor)
			VALUES ('00000000-0000-0000-0000-0000000000bb', $1, $2, 1), ('00000000-0000-0000-0000-0000000000bb', $1, $3, -1)`, w.group, w.a, w.b)
		mustExec(t, tx, `UPDATE expenses SET status = 'void', voided_at = now() WHERE id = $1`, w.e1)
		requireOnly(t, check(t, tx, w.expect()), stressrig.SharesMatchClaims)
	})
}

func TestCheck_DuplicateExpenseForOneKey(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO expenses (group_id, payer_id, description, status) SELECT group_id, payer_id, description, 'claiming' FROM expenses WHERE id = $1`, w.e1)
		requireOnly(t, check(t, tx, w.expect()), stressrig.DuplicateKey)
	})
}

func TestCheck_DuplicateSettlementForOneKey(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 100)`, w.group, w.a, w.payer)
		requireOnly(t, check(t, tx, w.expect()), stressrig.DuplicateKey)
	})
}

func TestCheck_KeyOutcomeDisagreesWithRows(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		exp := w.expect()
		exp.Creates = append(exp.Creates,
			stressrig.Create{Group: w.group, Description: "acknowledged-but-never-created", Rows: stressrig.One},
			stressrig.Create{Group: w.group, Description: w.d1, Rows: stressrig.Zero}, // refused, yet it exists
		)
		rep := check(t, tx, exp)
		requireOnly(t, rep, stressrig.KeyOutcome)
		if rep.Counts[stressrig.KeyOutcome] != 2 {
			t.Fatalf("want 2 mismatches, got %d\n%s", rep.Counts[stressrig.KeyOutcome], rep)
		}
	})
}

func TestCheck_SettlementsOverCapacity(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		// a owes 100 and the payer is owed 100, but 150 is succeeded or pending.
		exp := w.expect()
		exp.Frozen = []stressrig.Frozen{{Group: w.group, Balance: map[uuid.UUID]int64{w.a: -100, w.payer: 100}}}
		rep := check(t, tx, exp)
		requireOnly(t, rep, stressrig.OverCapacity)
		if rep.Counts[stressrig.OverCapacity] != 2 { // too much out of a, too much into the payer
			t.Fatalf("want the sender's and the payee's side, got %d\n%s", rep.Counts[stressrig.OverCapacity], rep)
		}
		// A failed settlement released its reservation: 100 fits in 100.
		mustExec(t, tx, `UPDATE settlements SET status = 'failed', failure_code = 'card_declined', resolved_at = now() WHERE id = $1`, w.s2)
		if rep := check(t, tx, exp); !rep.OK() {
			t.Fatalf("a failed settlement must not count against capacity:\n%s", rep)
		}
	})
}

func TestCheck_SettlementPostingWithTheWrongAmount(t *testing.T) {
	w := newWorld(t)
	w.inTx(t, func(tx pgx.Tx) {
		var s3 uuid.UUID
		if err := tx.QueryRow(context.Background(), `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 70) RETURNING id`, w.group, w.a, w.payer).Scan(&s3); err != nil {
			t.Fatal(err)
		}
		mustExec(t, tx, `INSERT INTO ledger_transactions (id, group_id, kind, settlement_id, created_by) VALUES ('00000000-0000-0000-0000-0000000000cc', $1, 'settlement', $2, $3)`, w.group, s3, w.a)
		mustExec(t, tx, `INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ('00000000-0000-0000-0000-0000000000cc', $1, $2, 60), ('00000000-0000-0000-0000-0000000000cc', $1, $3, -60)`, w.group, w.a, w.payer)
		mustExec(t, tx, `UPDATE settlements SET status = 'succeeded', ledger_transaction_id = '00000000-0000-0000-0000-0000000000cc', resolved_at = now() WHERE id = $1`, s3)
		requireOnly(t, check(t, tx, w.expect()), stressrig.SettlementPosting)
	})
}

func TestCheck_AgainstTheProvidersBooks(t *testing.T) {
	w := newWorld(t)
	paid := mockpay.Ledger{Payment: mockpay.Payment{
		Reference: w.s1.String(), IdempotencyKey: w.s1.String(), From: w.a.String(), To: w.payer.String(),
		AmountMinor: 100, Currency: "CAD", Status: mockpay.StatusSucceeded,
	}}
	exp := w.expect()

	books := []mockpay.Ledger{paid}
	exp.Provider = &books
	rep, err := stressrig.Check(context.Background(), w.pool, exp)
	if err != nil || !rep.OK() {
		t.Fatalf("matching books: %v\n%s", err, rep)
	}

	none := []mockpay.Ledger{} // the app says s1 succeeded; the provider never charged it
	exp.Provider = &none
	rep, err = stressrig.Check(context.Background(), w.pool, exp)
	if err != nil {
		t.Fatal(err)
	}
	requireOnly(t, rep, stressrig.ProviderBooks)

	twice := []mockpay.Ledger{paid, paid} // charged twice
	exp.Provider = &twice
	rep, err = stressrig.Check(context.Background(), w.pool, exp)
	if err != nil {
		t.Fatal(err)
	}
	requireOnly(t, rep, stressrig.ProviderBooks)
}

func TestBalances(t *testing.T) {
	w := newWorld(t)
	got, err := stressrig.Balances(context.Background(), w.pool, w.group)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, v := range got {
		sum += v
	}
	if sum != 0 || len(got) == 0 {
		t.Fatalf("balances %v must be non-empty and sum to zero", got)
	}
}
