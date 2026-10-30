package expenses_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

func voidURL(group, expense string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/void"
}

func (w *world) void(t testing.TB, token, group, expense, key string) teststack.Response {
	return w.s.Do(t, token, "POST", voidURL(group, expense), ``, "Idempotency-Key", key)
}

// finalizedDinner: `payer` creates an expense with two items, the owner's (1000)
// and the member's (500); each claims their own; the payer finalizes. If the
// member pays, the ledger is member +1000, owner -1000 (the member's own 500 is
// never owed to anyone). If the owner pays, it is owner +500, member -500.
func (w *world) finalizedDinner(t testing.TB, payer string) string {
	t.Helper()
	exp, items := w.newExpense(t, payer, `{"description":"dinner","items":[{"description":"owner's","price_minor":1000},{"description":"member's","price_minor":500}]}`)
	w.put(t, w.owner, w.group, exp, items[0])
	w.put(t, w.member, w.group, exp, items[1])
	if r := w.finalize(t, payer, w.group, exp, ``, uuid.NewString()); r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	return exp
}

func (w *world) groupBalances(t testing.TB, token string) map[uuid.UUID]int64 {
	t.Helper()
	var v struct {
		Balances []struct {
			MemberID     uuid.UUID `json:"member_id"`
			BalanceMinor int64     `json:"balance_minor"`
		} `json:"balances"`
	}
	r := w.s.Do(t, token, "GET", "/v1/groups/"+w.group+"/balances", "")
	if r.Status != 200 {
		t.Fatalf("balances = %d: %s", r.Status, r.Body)
	}
	r.JSON(t, &v)
	out := map[uuid.UUID]int64{}
	for _, b := range v.Balances {
		out[b.MemberID] = b.BalanceMinor
	}
	return out
}

type voided struct {
	expense
	LedgerTransactionID         string `json:"ledger_transaction_id"`
	ReversedLedgerTransactionID string `json:"reversed_ledger_transaction_id"`
}

func TestVoid_ByThePayerReversesTheLedgerExactly(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedDinner(t, w.member)
	before := w.ledgerEntries(t, exp)
	if before[w.memberID] != 1000 || before[w.ownerID] != -1000 || len(before) != 2 {
		t.Fatalf("setup: finalize posting = %v", before)
	}
	claimsBefore := w.claimRows(t, exp)
	var original string
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT id FROM ledger_transactions WHERE expense_id = $1 AND kind = 'expense'`, exp).Scan(&original); err != nil {
		t.Fatal(err)
	}

	r := w.void(t, w.member, w.group, exp, uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("void = %d: %s", r.Status, r.Body)
	}
	var v voided
	r.JSON(t, &v)
	if v.Status != "void" || v.VoidedAt == nil || v.FinalizedAt == nil {
		t.Errorf("expense = status %s voided_at %v finalized_at %v; a void expense keeps its finalized_at", v.Status, v.VoidedAt, v.FinalizedAt)
	}
	if v.ReversedLedgerTransactionID != original || v.LedgerTransactionID == original || v.LedgerTransactionID == "" {
		t.Errorf("ledger ids: reversed %s (want %s), new %s", v.ReversedLedgerTransactionID, original, v.LedgerTransactionID)
	}

	// The reversal is a second posting; the first one is untouched.
	var kind string
	var createdBy uuid.UUID
	var reverses *string
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT kind, created_by, reverses_id::text FROM ledger_transactions WHERE id = $1`, v.LedgerTransactionID).Scan(&kind, &createdBy, &reverses); err != nil {
		t.Fatal(err)
	}
	if kind != "void" || createdBy != w.memberID || reverses == nil || *reverses != original || w.ledgerTxCount(t, exp) != 2 {
		t.Errorf("void posting: kind %s created_by %s reverses %v; transactions for the expense %d", kind, createdBy, reverses, w.ledgerTxCount(t, exp))
	}
	if after := w.ledgerEntries(t, exp); len(after) != len(before) || after[w.memberID] != 1000 || after[w.ownerID] != -1000 {
		t.Errorf("the original posting changed: %v -> %v", before, after)
	}
	reversal := w.ledgerEntriesOfKind(t, exp, "void")
	if len(reversal) != 2 || reversal[w.memberID] != -1000 || reversal[w.ownerID] != 1000 {
		t.Errorf("reversal entries = %v, want member -1000 and owner +1000", reversal)
	}
	if n := w.s.QueryInt(t, `SELECT coalesce(sum(en.amount_minor), 0) FROM ledger_entries en JOIN ledger_transactions tx ON tx.id = en.transaction_id WHERE tx.expense_id = $1`, exp); n != 0 {
		t.Errorf("the expense's postings sum to %d, want 0", n)
	}

	// Everyone is back to zero, claims are the record they were, and the audit
	// trail says who voided and what it reversed.
	for m, bal := range w.groupBalances(t, w.owner) {
		if bal != 0 {
			t.Errorf("member %s is at %d after the void, want 0", m, bal)
		}
	}
	if got := w.claimRows(t, exp); strings.Join(got, ",") != strings.Join(claimsBefore, ",") {
		t.Errorf("claims changed: %v -> %v", claimsBefore, got)
	}
	var raw []byte
	var actor uuid.UUID
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT actor_user_id, data FROM audit_events WHERE kind = 'expense.voided' AND data->>'expense_id' = $1`, exp).Scan(&actor, &raw); err != nil {
		t.Fatalf("audit event: %v", err)
	}
	var data struct {
		Tx       string `json:"ledger_transaction_id"`
		Reverses string `json:"reverses_ledger_transaction_id"`
		Entries  int    `json:"reversed_entries"`
		As       string `json:"voided_as"`
	}
	_ = json.Unmarshal(raw, &data)
	if actor != w.memberID || data.Tx != v.LedgerTransactionID || data.Reverses != original || data.Entries != 2 || data.As != "payer" {
		t.Errorf("audit = actor %s data %+v", actor, data)
	}
	w.assertLedgerMatchesClaims(t, exp) // the original posting still matches the (unchanged) claims

	var read expense
	w.s.Do(t, w.owner, "GET", "/v1/groups/"+w.group+"/expenses/"+exp, "").JSON(t, &read)
	if read.Status != "void" || len(read.Items[0].ClaimedBy) != 1 {
		t.Errorf("GET after void = %+v", read)
	}
}

func TestVoid_TheGroupOwnerCanVoidAMembersExpense(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedDinner(t, w.member) // payer is the plain member
	r := w.void(t, w.owner, w.group, exp, uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("owner void = %d: %s", r.Status, r.Body)
	}
	var createdBy, actor uuid.UUID
	var raw []byte
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT created_by FROM ledger_transactions WHERE expense_id = $1 AND kind = 'void'`, exp).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT actor_user_id, data FROM audit_events WHERE kind = 'expense.voided' AND data->>'expense_id' = $1`, exp).Scan(&actor, &raw); err != nil {
		t.Fatal(err)
	}
	if createdBy != w.ownerID || actor != w.ownerID || !strings.Contains(string(raw), `"voided_as": "owner"`) {
		t.Errorf("created_by %s, audit actor %s data %s; all should name the owner who did it", createdBy, actor, raw)
	}
	for m, bal := range w.groupBalances(t, w.member) {
		if bal != 0 {
			t.Errorf("member %s is at %d after the void", m, bal)
		}
	}
}

func TestVoid_Authorization(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedDinner(t, w.owner) // payer is the owner; `member` is neither payer nor owner
	check := func(name, token string, want int) {
		t.Helper()
		if r := w.void(t, token, w.group, exp, uuid.NewString()); r.Status != want {
			t.Errorf("%s: void = %d, want %d: %s", name, r.Status, want, r.Body)
		}
	}
	check("non-member", w.outsider, 404)
	check("a member who is neither payer nor owner", w.member, 403)
	check("anonymous", "", 401)
	if w.ledgerTxCount(t, exp) != 1 || w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'expense.voided' AND data->>'expense_id' = $1`, exp) != 0 {
		t.Fatal("a refused void wrote something")
	}
	if st := w.s.QueryInt(t, `SELECT count(*) FROM expenses WHERE id = $1 AND status = 'finalized' AND voided_at IS NULL`, exp); st != 1 {
		t.Fatal("a refused void changed the expense")
	}

	// Refusals come before the key or body are read; the payer still needs a key.
	if r := w.s.Do(t, w.outsider, "POST", voidURL(w.group, exp), `junk`); r.Status != 404 {
		t.Errorf("non-member, no key, bad body = %d, want 404", r.Status)
	}
	if r := w.s.Do(t, w.member, "POST", voidURL(w.group, exp), `junk`); r.Status != 403 {
		t.Errorf("unauthorized member, no key, bad body = %d, want 403", r.Status)
	}
	if r := w.s.Do(t, w.owner, "POST", voidURL(w.group, exp), ``); r.Status != 400 {
		t.Errorf("authorized, no Idempotency-Key = %d, want 400", r.Status)
	}
	if r := w.s.Do(t, w.owner, "POST", voidURL(w.group, exp), `{"reason":"x"}`, "Idempotency-Key", uuid.NewString()); r.Status != 400 {
		t.Errorf("unknown body field = %d, want 400", r.Status)
	}

	// 404s are the same for ghosts, malformed ids and other groups' expenses.
	otherExp := w.s.CreateExpense(t, w.outsider, w.otherGroup, goodBody)
	want := w.void(t, w.owner, w.group, uuid.NewString(), uuid.NewString())
	for name, r := range map[string]teststack.Response{
		"non-member, real ids":    w.void(t, w.outsider, w.group, exp, uuid.NewString()),
		"malformed expense id":    w.void(t, w.owner, w.group, "nope", uuid.NewString()),
		"other group's expense":   w.void(t, w.owner, w.group, otherExp, uuid.NewString()),
		"wrong group for expense": w.void(t, w.outsider, w.otherGroup, exp, uuid.NewString()),
	} {
		if r.Status != 404 || !bytes.Equal(r.Body, want.Body) {
			t.Errorf("%s: %d %s, want the ghost 404 %s", name, r.Status, r.Body, want.Body)
		}
	}
	check("the payer (who is also the owner)", w.owner, 200)
}

func TestVoid_OnlyFromFinalized(t *testing.T) {
	w := newWorld(t)
	for _, status := range []string{"claiming", "parsing", "needs_review"} {
		exp, _ := w.newExpense(t, w.member, goodBody)
		if status != "claiming" {
			w.s.Exec(t, `UPDATE expenses SET status = $2 WHERE id = $1`, exp, status)
		}
		r := w.void(t, w.member, w.group, exp, uuid.NewString())
		if r.Status != 409 || !strings.Contains(string(r.Body), "invalid_state") {
			t.Errorf("void of a %s expense = %d %s, want 409 invalid_state", status, r.Status, r.Body)
		}
		if w.ledgerTxCount(t, exp) != 0 {
			t.Errorf("void of a %s expense posted to the ledger", status)
		}
	}

	// A second void with a different key is refused; there is only ever one reversal.
	exp := w.finalizedDinner(t, w.member)
	if r := w.void(t, w.member, w.group, exp, uuid.NewString()); r.Status != 200 {
		t.Fatalf("first void = %d", r.Status)
	}
	if r := w.void(t, w.owner, w.group, exp, uuid.NewString()); r.Status != 409 || !strings.Contains(string(r.Body), "invalid_state") {
		t.Errorf("second void = %d %s, want 409 invalid_state", r.Status, r.Body)
	}
	if w.ledgerTxCount(t, exp) != 2 {
		t.Errorf("ledger transactions = %d, want 2 (the posting and one reversal)", w.ledgerTxCount(t, exp))
	}

	// A void expense is frozen: no claims, no finalize.
	items := w.claimedBy(t, w.member, exp)
	_ = items
	var e expense
	w.s.Do(t, w.member, "GET", "/v1/groups/"+w.group+"/expenses/"+exp, "").JSON(t, &e)
	if r := w.put(t, w.owner, w.group, exp, e.Items[0].ID); r.Status != 409 {
		t.Errorf("claim on a void expense = %d, want 409", r.Status)
	}
	if r := w.del(t, w.owner, w.group, exp, e.Items[0].ID); r.Status != 409 {
		t.Errorf("unclaim on a void expense = %d, want 409", r.Status)
	}
	if r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()); r.Status != 409 {
		t.Errorf("finalize of a void expense = %d, want 409", r.Status)
	}
	if w.ledgerTxCount(t, exp) != 2 {
		t.Error("something posted to a void expense")
	}
}

func TestVoid_Idempotency(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedDinner(t, w.member)
	key := uuid.NewString()
	first := w.void(t, w.member, w.group, exp, key)
	if first.Status != 200 {
		t.Fatalf("void = %d: %s", first.Status, first.Body)
	}
	again := w.void(t, w.member, w.group, exp, key)
	if again.Status != 200 || !bytes.Equal(again.Body, first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d replayed=%q same body=%v", again.Status, again.Header.Get("Idempotent-Replayed"), bytes.Equal(again.Body, first.Body))
	}
	// The same key for another expense is a different request.
	other := w.finalizedDinner(t, w.member)
	if r := w.void(t, w.member, w.group, other, key); r.Status != 422 {
		t.Errorf("same key, different expense = %d, want 422", r.Status)
	}
	if w.ledgerTxCount(t, exp) != 2 || w.ledgerTxCount(t, other) != 1 {
		t.Errorf("ledger transactions: voided %d (want 2), untouched %d (want 1)", w.ledgerTxCount(t, exp), w.ledgerTxCount(t, other))
	}
}

func TestVoid_ConcurrentRequestsReverseOnce(t *testing.T) {
	const n = 10
	t.Run("same key", func(t *testing.T) {
		w := newWorld(t)
		exp := w.finalizedDinner(t, w.member)
		key := uuid.NewString()
		resps := make([]teststack.Response, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); resps[i] = w.void(t, w.member, w.group, exp, key) }()
		}
		wg.Wait()
		for i, r := range resps {
			if r.Status != 200 || !bytes.Equal(r.Body, resps[0].Body) {
				t.Fatalf("request %d = %d: %s", i, r.Status, r.Body)
			}
		}
		if w.ledgerTxCount(t, exp) != 2 {
			t.Fatalf("ledger transactions = %d, want 2", w.ledgerTxCount(t, exp))
		}
	})
	t.Run("different keys, payer and owner racing", func(t *testing.T) {
		w := newWorld(t)
		exp := w.finalizedDinner(t, w.member)
		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tok := []string{w.member, w.owner}[i%2]
				statuses[i] = w.void(t, tok, w.group, exp, uuid.NewString()).Status
			}()
		}
		wg.Wait()
		got := map[int]int{}
		for _, s := range statuses {
			got[s]++
		}
		if got[200] != 1 || got[409] != n-1 {
			t.Fatalf("statuses = %v, want one 200 and %d 409s", got, n-1)
		}
		if w.ledgerTxCount(t, exp) != 2 || w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'expense.voided' AND data->>'expense_id' = $1`, exp) != 1 {
			t.Fatalf("ledger transactions %d (want 2), voided audit events want 1", w.ledgerTxCount(t, exp))
		}
		for m, bal := range w.groupBalances(t, w.owner) {
			if bal != 0 {
				t.Errorf("member %s at %d", m, bal)
			}
		}
	})
}

// The point of a void: the group's money is exactly as if the expense had never
// been finalized, whatever else happened in between.
//
//	X: member paid, owner owes 1000.        Y: owner paid, third owes 700.
//	After both: member +1000, owner -300, third -700.
//	Void X:     member 0,     owner +700, third -700.   (exactly Y alone)
func TestVoid_BalancesAreAsIfTheExpenseNeverHappened(t *testing.T) {
	w := newWorld(t)
	thirdTok, third := w.addMember(t)
	_ = thirdTok

	x, items := w.newExpense(t, w.member, `{"description":"X","items":[{"description":"o","price_minor":1000}]}`)
	w.put(t, w.owner, w.group, x, items[0])
	w.finalize(t, w.member, w.group, x, ``, uuid.NewString())
	y, yItems := w.newExpense(t, w.owner, `{"description":"Y","items":[{"description":"t","price_minor":700}]}`)
	w.put(t, thirdTok, w.group, y, yItems[0])
	w.finalize(t, w.owner, w.group, y, ``, uuid.NewString())

	mid := w.groupBalances(t, w.owner)
	if mid[w.memberID] != 1000 || mid[w.ownerID] != -300 || mid[third] != -700 {
		t.Fatalf("before the void: %v", mid)
	}
	if r := w.void(t, w.member, w.group, x, uuid.NewString()); r.Status != 200 {
		t.Fatalf("void X = %d: %s", r.Status, r.Body)
	}
	after := w.groupBalances(t, w.owner)
	if after[w.memberID] != 0 || after[w.ownerID] != 700 || after[third] != -700 {
		t.Errorf("after voiding X: %v, want member 0, owner +700, third -700 (Y alone)", after)
	}
	var sum int64
	for _, b := range after {
		sum += b
	}
	if sum != 0 {
		t.Errorf("balances sum to %d", sum)
	}
}

func TestVoid_PayerAteEverything(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, `{"description":"d","items":[{"description":"a","price_minor":500}]}`)
	w.put(t, w.member, w.group, exp, items[0])
	if r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()); r.Status != 200 {
		t.Fatalf("finalize = %d", r.Status)
	}
	if r := w.void(t, w.member, w.group, exp, uuid.NewString()); r.Status != 200 {
		t.Fatalf("void = %d: %s", r.Status, r.Body)
	}
	if w.ledgerTxCount(t, exp) != 2 || len(w.ledgerEntriesOfKind(t, exp, "void")) != 0 {
		t.Errorf("a void of an entry-less posting should be a header with no entries: tx %d", w.ledgerTxCount(t, exp))
	}
	if st := w.s.QueryInt(t, `SELECT count(*) FROM expenses WHERE id = $1 AND status = 'void'`, exp); st != 1 {
		t.Error("expense not marked void")
	}
}

// The lock query authorizes in its own statement. The handler checks first, which
// hides that, so test the query directly.
func TestLockExpenseForVoid_AuthorizesInTheStatement(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedDinner(t, w.member) // payer: member; group owner: owner
	expID, groupID := uuid.MustParse(exp), uuid.MustParse(w.group)
	otherGroup := uuid.MustParse(w.otherGroup)
	outsiderID := w.s.UserID(t, w.outsider)
	_, third := w.addMember(t) // a plain member: neither payer nor owner

	try := func(user, group uuid.UUID) error {
		ctx := context.Background()
		tx, err := w.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // lock only; nothing to commit
		_, err = db.New(tx).LockExpenseForVoid(ctx, db.LockExpenseForVoidParams{UserID: user, ExpenseID: expID, GroupID: group})
		return err
	}
	if err := try(w.memberID, groupID); err != nil {
		t.Fatalf("control, the payer: %v", err)
	}
	if err := try(w.ownerID, groupID); err != nil {
		t.Fatalf("control, the group owner: %v", err)
	}
	for name, c := range map[string]struct{ user, group uuid.UUID }{
		"a plain member who is neither":      {third, groupID},
		"a user outside the group":           {outsiderID, groupID},
		"the payer, with another group's id": {w.memberID, otherGroup},
		"an owner of a different group":      {outsiderID, otherGroup},
	} {
		if err := try(c.user, c.group); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: err = %v, want no rows", name, err)
		}
	}
}

// Two voids must never be inside the critical section together, or both could pass
// the status check and race to post. That needs an exclusive lock: FOR UPDATE
// (or FOR NO KEY UPDATE), not FOR SHARE, which two lockers hold at once. Tested
// directly, because racing two real requests only collides by luck: the critical
// section is a few milliseconds long.
func TestLockExpenseForVoid_IsExclusive(t *testing.T) {
	w := newWorld(t)
	exp := w.finalizedDinner(t, w.member)
	expID, groupID := uuid.MustParse(exp), uuid.MustParse(w.group)
	ctx := context.Background()

	first, err := w.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx) //nolint:errcheck // lock only
	if _, err := db.New(first).LockExpenseForVoid(ctx, db.LockExpenseForVoidParams{UserID: w.memberID, ExpenseID: expID, GroupID: groupID}); err != nil {
		t.Fatalf("first locker: %v", err)
	}

	second, err := w.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(ctx) //nolint:errcheck // lock only
	if _, err := second.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = db.New(second).LockExpenseForVoid(ctx, db.LockExpenseForVoidParams{UserID: w.ownerID, ExpenseID: expID, GroupID: groupID})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" { // lock_not_available
		t.Fatalf("a second locker got err = %v; it must have to wait (lock_not_available) while the first holds the lock", err)
	}
}
