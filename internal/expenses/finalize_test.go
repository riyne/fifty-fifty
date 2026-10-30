package expenses_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/allocator"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/ledger"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

func finalizeURL(group, expense string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/finalize"
}

func (w *world) finalize(t testing.TB, token, group, expense, body, key string) teststack.Response {
	return w.s.Do(t, token, "POST", finalizeURL(group, expense), body, "Idempotency-Key", key)
}

func splitBody(ids ...uuid.UUID) string {
	b, _ := json.Marshal(map[string][]uuid.UUID{"split_rest_among": ids})
	return string(b)
}

// addMember joins one more user to the world's group.
func (w *world) addMember(t testing.TB) (string, uuid.UUID) {
	t.Helper()
	tok := w.s.Token(t, "third")
	_, code := w.s.CreateJoinCode(t, w.owner, w.group, 0, 0)
	if r := w.s.Join(t, tok, code); r.Status != http.StatusCreated {
		t.Fatalf("join = %d: %s", r.Status, r.Body)
	}
	return tok, w.s.UserID(t, tok)
}

type finalized struct {
	expense
	LedgerTransactionID string `json:"ledger_transaction_id"`
	Shares              []struct {
		MemberID      uuid.UUID `json:"member_id"`
		SubtotalMinor int64     `json:"subtotal_minor"`
		TaxMinor      int64     `json:"tax_minor"`
		TipMinor      int64     `json:"tip_minor"`
		TotalMinor    int64     `json:"total_minor"`
	} `json:"shares"`
}

func (w *world) ledgerTxCount(t testing.TB, expenseID string) int {
	return w.s.QueryInt(t, `SELECT count(*) FROM ledger_transactions WHERE expense_id = $1`, expenseID)
}

func (w *world) auditCount(t testing.TB, expenseID string) int {
	return w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'expense.finalized' AND data->>'expense_id' = $1`, expenseID)
}

// ledgerEntries returns the entries of an expense's finalize posting.
func (w *world) ledgerEntries(t testing.TB, expenseID string) map[uuid.UUID]int64 {
	return w.ledgerEntriesOfKind(t, expenseID, "expense")
}

// ledgerEntriesOfKind returns the entries of one posting of an expense:
// kind "expense" is the finalize posting, "void" is the reversal.
func (w *world) ledgerEntriesOfKind(t testing.TB, expenseID, kind string) map[uuid.UUID]int64 {
	t.Helper()
	rows, err := w.s.Pool.Query(context.Background(),
		`SELECT en.member_id, en.amount_minor FROM ledger_entries en JOIN ledger_transactions tx ON tx.id = en.transaction_id WHERE tx.expense_id = $1 AND tx.kind = $2`, expenseID, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var m uuid.UUID
		var a int64
		if err := rows.Scan(&m, &a); err != nil {
			t.Fatal(err)
		}
		out[m] = a
	}
	return out
}

// assertLedgerMatchesClaims is the invariant the whole design rests on: read the
// claims table back, recompute every share with the allocator, derive the entries
// the ledger should hold, and require the ledger to contain exactly those and sum
// to zero. It does not look at the finalize response at all.
func (w *world) assertLedgerMatchesClaims(t testing.TB, expenseID string) {
	t.Helper()
	ctx := context.Background()
	var payer uuid.UUID
	var tax, tip int64
	if err := w.s.Pool.QueryRow(ctx, `SELECT payer_id, tax_minor, tip_minor FROM expenses WHERE id = $1`, expenseID).Scan(&payer, &tax, &tip); err != nil {
		t.Fatal(err)
	}
	rows, err := w.s.Pool.Query(ctx, `
		SELECT i.id, i.price_minor, COALESCE(array_agg(c.member_id) FILTER (WHERE c.member_id IS NOT NULL), '{}')::uuid[]
		FROM expense_items i LEFT JOIN claims c ON c.item_id = i.id
		WHERE i.expense_id = $1 GROUP BY i.id`, expenseID)
	if err != nil {
		t.Fatal(err)
	}
	var items []allocator.Item
	for rows.Next() {
		var it allocator.Item
		if err := rows.Scan(&it.ID, &it.Price, &it.Claimants); err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	rows.Close()

	members, err := allocator.Allocate(items, tax, tip)
	if err != nil {
		t.Errorf("expense %s: claims in the database cannot be allocated: %v", expenseID, err)
		return
	}
	want, err := ledger.ExpenseEntries(payer, members)
	if err != nil {
		t.Fatal(err)
	}
	wantMap := map[uuid.UUID]int64{}
	for _, e := range want {
		wantMap[e.MemberID] = e.Amount
	}
	got := w.ledgerEntries(t, expenseID)
	if len(got) != len(wantMap) {
		t.Errorf("expense %s: ledger has entries %v, claims imply %v", expenseID, got, wantMap)
		return
	}
	var sum int64
	for m, a := range got {
		sum += a
		if wantMap[m] != a {
			t.Errorf("expense %s: ledger entry for %s is %d, recomputing from claims gives %d", expenseID, m, a, wantMap[m])
		}
	}
	if sum != 0 {
		t.Errorf("expense %s: ledger entries sum to %d", expenseID, sum)
	}
}

// ---- the arithmetic, hand-checked ----

// Dinner paid by `member`; `owner` and a third member share. Chosen so every
// division is exact and the expected numbers can be checked by hand:
//
//	pizza 1000  member+owner            -> 500 / 500
//	salad  600  owner                   -> 600
//	wine   300  member+owner+third      -> 100 each
//	subtotals   member 600, owner 1200, third 100 (sum 1900)
//	tax 190, tip 95 prorated 600:1200:100 -> 60/120/10 and 30/60/5
//	totals      member 690, owner 1380, third 115 (sum 2185 = 1900 + 190 + 95)
//
// The payer ate 690 of it and paid 2185, so is owed 1495 = 1380 + 115.
func TestFinalize_HandCheckedExample(t *testing.T) {
	w := newWorld(t)
	thirdTok, third := w.addMember(t)
	body := `{"description":"dinner","tax_minor":190,"tip_minor":95,"items":[
		{"description":"pizza","price_minor":1000},{"description":"salad","price_minor":600},{"description":"wine","price_minor":300}]}`
	exp, items := w.newExpense(t, w.member, body)
	for _, c := range []struct {
		tok  string
		item int
	}{{w.member, 0}, {w.owner, 0}, {w.owner, 1}, {w.member, 2}, {w.owner, 2}, {thirdTok, 2}} {
		if r := w.put(t, c.tok, w.group, exp, items[c.item]); r.Status != 200 {
			t.Fatalf("claim = %d: %s", r.Status, r.Body)
		}
	}

	r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	var f finalized
	r.JSON(t, &f)

	if f.Status != "finalized" || f.FinalizedAt == nil || f.VoidedAt != nil || f.TotalMinor != 2185 {
		t.Errorf("expense = status %s finalized_at %v total %d", f.Status, f.FinalizedAt, f.TotalMinor)
	}
	wantTotals := map[uuid.UUID]int64{w.memberID: 690, w.ownerID: 1380, third: 115}
	var sum int64
	if len(f.Shares) != 3 {
		t.Fatalf("shares = %+v, want 3", f.Shares)
	}
	for _, s := range f.Shares {
		sum += s.TotalMinor
		if s.TotalMinor != wantTotals[s.MemberID] || s.SubtotalMinor+s.TaxMinor+s.TipMinor != s.TotalMinor {
			t.Errorf("share %+v, want total %d", s, wantTotals[s.MemberID])
		}
	}
	if sum != f.TotalMinor {
		t.Errorf("shares sum to %d, expense total is %d", sum, f.TotalMinor)
	}

	// The ledger: one posting; the payer is credited what the others owe.
	wantEntries := map[uuid.UUID]int64{w.memberID: 1495, w.ownerID: -1380, third: -115}
	got := w.ledgerEntries(t, exp)
	if len(got) != 3 || got[w.memberID] != 1495 || got[w.ownerID] != -1380 || got[third] != -115 {
		t.Errorf("ledger entries = %v, want %v", got, wantEntries)
	}
	var kind string
	var createdBy uuid.UUID
	var reverses *uuid.UUID
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT kind, created_by, reverses_id FROM ledger_transactions WHERE id = $1`, f.LedgerTransactionID).Scan(&kind, &createdBy, &reverses); err != nil {
		t.Fatal(err)
	}
	if kind != "expense" || createdBy != w.memberID || reverses != nil || w.ledgerTxCount(t, exp) != 1 {
		t.Errorf("ledger transaction: kind %s created_by %s reverses %v count %d", kind, createdBy, reverses, w.ledgerTxCount(t, exp))
	}
	w.assertLedgerMatchesClaims(t, exp)

	// The audit event is written with the finalize, and names the transaction.
	if n := w.auditCount(t, exp); n != 1 {
		t.Fatalf("audit events = %d, want 1", n)
	}
	var data struct {
		LedgerTx string `json:"ledger_transaction_id"`
		Total    int64  `json:"total_minor"`
		Split    int    `json:"split_rest_items"`
	}
	var raw []byte
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT data FROM audit_events WHERE kind = 'expense.finalized' AND data->>'expense_id' = $1`, exp).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &data)
	if data.LedgerTx != f.LedgerTransactionID || data.Total != 2185 || data.Split != 0 {
		t.Errorf("audit data = %+v", data)
	}

	// Reading it back shows the same finalized expense with its claims intact.
	var read expense
	w.s.Do(t, w.owner, "GET", "/v1/groups/"+w.group+"/expenses/"+exp, "").JSON(t, &read)
	if read.Status != "finalized" || len(read.Items[2].ClaimedBy) != 3 {
		t.Errorf("GET after finalize: %+v", read)
	}
}

// Cents that do not divide evenly go to the lowest member id, and conservation
// still holds exactly.
func TestFinalize_RemaindersAreDeterministicAndExact(t *testing.T) {
	w := newWorld(t)
	thirdTok, third := w.addMember(t)
	exp, items := w.newExpense(t, w.member, `{"description":"d","tax_minor":1,"items":[{"description":"x","price_minor":100}]}`)
	for _, tok := range []string{w.member, w.owner, thirdTok} {
		w.put(t, tok, w.group, exp, items[0])
	}
	r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	var f finalized
	r.JSON(t, &f)

	ids := []uuid.UUID{w.memberID, w.ownerID, third}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	var sub, total int64
	for i, s := range f.Shares {
		sub += s.SubtotalMinor
		total += s.TotalMinor
		wantSub := int64(33)
		if i == 0 {
			wantSub = 34 // 100 / 3: the extra cent goes to the lowest id
		}
		if s.MemberID != ids[i] || s.SubtotalMinor != wantSub {
			t.Errorf("share %d = %+v, want member %s subtotal %d", i, s, ids[i], wantSub)
		}
	}
	if sub != 100 || total != 101 {
		t.Errorf("subtotals sum to %d and totals to %d, want 100 and 101 (item 100 + tax 1)", sub, total)
	}
	w.assertLedgerMatchesClaims(t, exp)
}

// ---- unclaimed items and "split the rest" ----

func TestFinalize_BlockedUntilEveryItemHasAClaimant(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.member, w.group, exp, items[0]) // beer (item 1) stays unclaimed

	key := uuid.NewString()
	r := w.finalize(t, w.member, w.group, exp, ``, key)
	if r.Status != 409 || !strings.Contains(string(r.Body), "items_unclaimed") {
		t.Fatalf("finalize with an unclaimed item = %d %s, want 409 items_unclaimed", r.Status, r.Body)
	}
	if w.ledgerTxCount(t, exp) != 0 || w.auditCount(t, exp) != 0 || len(w.claimRows(t, exp)) != 1 {
		t.Errorf("a refused finalize wrote something: ledger %d audit %d claims %v", w.ledgerTxCount(t, exp), w.auditCount(t, exp), w.claimRows(t, exp))
	}
	if st := w.s.QueryInt(t, `SELECT count(*) FROM expenses WHERE id = $1 AND status = 'claiming' AND finalized_at IS NULL`, exp); st != 1 {
		t.Error("a refused finalize changed the expense")
	}

	// The key's first outcome is final: retrying with the SAME key replays the 409
	// even though the item is claimed now. The client uses a new key per attempt.
	w.put(t, w.owner, w.group, exp, items[1])
	if r := w.finalize(t, w.member, w.group, exp, ``, key); r.Status != 409 || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("same key after fixing = %d, want the replayed 409", r.Status)
	}
	if r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()); r.Status != 200 {
		t.Fatalf("new key once everything is claimed = %d: %s", r.Status, r.Body)
	}
	w.assertLedgerMatchesClaims(t, exp)
}

func TestFinalize_SplitTheRest(t *testing.T) {
	w := newWorld(t)
	thirdTok, third := w.addMember(t)
	_ = thirdTok
	exp, items := w.newExpense(t, w.member, `{"description":"d","items":[
		{"description":"a","price_minor":1000},{"description":"b","price_minor":600},{"description":"c","price_minor":301}]}`)
	w.put(t, w.owner, w.group, exp, items[0]) // a is the owner's; b and c are unclaimed

	r := w.finalize(t, w.member, w.group, exp, splitBody(w.memberID, third), uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	var f finalized
	r.JSON(t, &f)

	// The split is recorded as real claims, so the response and the table agree.
	if len(f.Items[0].ClaimedBy) != 1 || len(f.Items[1].ClaimedBy) != 2 || len(f.Items[2].ClaimedBy) != 2 {
		t.Errorf("claimed_by after split = %v / %v / %v", f.Items[0].ClaimedBy, f.Items[1].ClaimedBy, f.Items[2].ClaimedBy)
	}
	if n := len(w.claimRows(t, exp)); n != 5 {
		t.Errorf("claim rows = %d, want 5 (owner on a; member and third on b and c)", n)
	}
	shares := map[uuid.UUID]int64{}
	for _, s := range f.Shares {
		shares[s.MemberID] = s.TotalMinor
	}
	lo, hi := w.memberID, third
	if bytes.Compare(hi[:], lo[:]) < 0 {
		lo, hi = hi, lo
	}
	// b: 300 each. c: 301 over two, the odd cent to the lowest id. a: 1000 to the owner.
	if shares[w.ownerID] != 1000 || shares[lo] != 300+151 || shares[hi] != 300+150 {
		t.Errorf("shares = %v, want owner 1000, %s 451, %s 450", shares, lo, hi)
	}
	var data []byte
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT data FROM audit_events WHERE kind = 'expense.finalized' AND data->>'expense_id' = $1`, exp).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"split_rest_items": 2`) {
		t.Errorf("audit data = %s, want split_rest_items 2", data)
	}
	w.assertLedgerMatchesClaims(t, exp)
}

func TestFinalize_SplitListIsIgnoredWhenEverythingIsClaimed(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	for _, it := range items {
		w.put(t, w.owner, w.group, exp, it)
	}
	if r := w.finalize(t, w.member, w.group, exp, splitBody(w.memberID), uuid.NewString()); r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	if rows := w.claimRows(t, exp); len(rows) != 2 {
		t.Errorf("claims = %v, want only the owner's two", rows)
	}
	w.assertLedgerMatchesClaims(t, exp)
}

func TestFinalize_SplitListValidation(t *testing.T) {
	w := newWorld(t)
	outsiderID := w.s.UserID(t, w.outsider)
	exp, _ := w.newExpense(t, w.member, goodBody)
	tests := []struct{ name, body string }{
		{"duplicates", splitBody(w.memberID, w.memberID)},
		{"a user outside the group", splitBody(outsiderID)},
		{"a member plus a user outside the group", splitBody(w.memberID, outsiderID)},
		{"an unknown id", splitBody(uuid.New())},
		{"malformed id", `{"split_rest_among":["nope"]}`},
		{"wrong type", `{"split_rest_among":"all"}`},
		{"unknown field", `{"split":[]}`},
		{"not json", `nope`},
	}
	for _, tc := range tests {
		key := uuid.NewString()
		r := w.finalize(t, w.member, w.group, exp, tc.body, key)
		if r.Status != 400 {
			t.Errorf("%s: finalize = %d, want 400: %s", tc.name, r.Status, r.Body)
		}
		// A duplicate is also caught by the member count, but should say what is wrong.
		if tc.name == "duplicates" && !strings.Contains(string(r.Body), "at most once") {
			t.Errorf("duplicates: message = %s, want it to say each member can be listed at most once", r.Body)
		}
		// Validation happens before the key is claimed.
		if n := w.s.QueryInt(t, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key); n != 0 {
			t.Errorf("%s: a validation failure stored its idempotency key", tc.name)
		}
	}
	if w.ledgerTxCount(t, exp) != 0 || len(w.claimRows(t, exp)) != 0 {
		t.Error("a rejected request wrote something")
	}
}

func TestFinalize_PayerAteEverything(t *testing.T) {
	w := newWorld(t)
	// A free item claimed by someone else too: that member owes nothing and gets no entry.
	exp, items := w.newExpense(t, w.member, `{"description":"d","items":[{"description":"a","price_minor":500},{"description":"free","price_minor":0}]}`)
	w.put(t, w.member, w.group, exp, items[0])
	w.put(t, w.member, w.group, exp, items[1])
	w.put(t, w.owner, w.group, exp, items[1])

	r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	if w.ledgerTxCount(t, exp) != 1 || len(w.ledgerEntries(t, exp)) != 0 {
		t.Errorf("a payer who ate everything should post a header with no entries: tx %d entries %v", w.ledgerTxCount(t, exp), w.ledgerEntries(t, exp))
	}
	w.assertLedgerMatchesClaims(t, exp)
}

// ---- who may finalize, and when ----

func TestFinalize_Authorization(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	for _, it := range items {
		w.put(t, w.member, w.group, exp, it)
	}

	check := func(name, token string, want int) {
		t.Helper()
		if r := w.finalize(t, token, w.group, exp, ``, uuid.NewString()); r.Status != want {
			t.Errorf("%s: finalize = %d, want %d: %s", name, r.Status, want, r.Body)
		}
	}
	check("non-member", w.outsider, 404)
	check("group owner who is not the payer", w.owner, 403)
	check("anonymous", "", 401)
	if w.ledgerTxCount(t, exp) != 0 || w.auditCount(t, exp) != 0 {
		t.Fatal("a refused finalize wrote something")
	}

	// Refusals come before the key or the body are even looked at.
	if r := w.s.Do(t, w.outsider, "POST", finalizeURL(w.group, exp), `not json`); r.Status != 404 {
		t.Errorf("non-member with no key and a bad body = %d, want 404", r.Status)
	}
	if r := w.s.Do(t, w.owner, "POST", finalizeURL(w.group, exp), `not json`); r.Status != 403 {
		t.Errorf("non-payer with no key and a bad body = %d, want 403", r.Status)
	}
	if r := w.s.Do(t, w.member, "POST", finalizeURL(w.group, exp), ``); r.Status != 400 {
		t.Errorf("payer without an Idempotency-Key = %d, want 400", r.Status)
	}

	// 404s are identical for ghosts, malformed ids and other groups' expenses.
	otherExp := w.s.CreateExpense(t, w.outsider, w.otherGroup, goodBody)
	want := w.finalize(t, w.member, w.group, uuid.NewString(), ``, uuid.NewString())
	for name, r := range map[string]teststack.Response{
		"non-member, real ids":    w.finalize(t, w.outsider, w.group, exp, ``, uuid.NewString()),
		"malformed expense id":    w.finalize(t, w.member, w.group, "nope", ``, uuid.NewString()),
		"other group's expense":   w.finalize(t, w.member, w.group, otherExp, ``, uuid.NewString()),
		"wrong group for expense": w.finalize(t, w.outsider, w.otherGroup, exp, ``, uuid.NewString()),
	} {
		if r.Status != 404 || !bytes.Equal(r.Body, want.Body) {
			t.Errorf("%s: %d %s, want the ghost-expense 404 %s", name, r.Status, r.Body, want.Body)
		}
	}

	check("the payer", w.member, 200)
}

func TestFinalize_OnlyFromClaiming(t *testing.T) {
	w := newWorld(t)
	for _, status := range []string{"parsing", "needs_review", "void"} {
		exp, items := w.newExpense(t, w.member, goodBody)
		for _, it := range items {
			w.put(t, w.member, w.group, exp, it)
		}
		set := `UPDATE expenses SET status = $2 WHERE id = $1`
		if status == "void" {
			set = `UPDATE expenses SET status = $2, finalized_at = now(), voided_at = now() WHERE id = $1`
		}
		w.s.Exec(t, set, exp, status)
		r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString())
		if r.Status != 409 || !strings.Contains(string(r.Body), "invalid_state") {
			t.Errorf("finalize of a %s expense = %d %s, want 409 invalid_state", status, r.Status, r.Body)
		}
		if w.ledgerTxCount(t, exp) != 0 {
			t.Errorf("finalize of a %s expense posted to the ledger", status)
		}
	}

	// Finalizing twice with different keys: the second is refused, one posting only.
	exp, items := w.newExpense(t, w.member, goodBody)
	for _, it := range items {
		w.put(t, w.member, w.group, exp, it)
	}
	if r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()); r.Status != 200 {
		t.Fatalf("first finalize = %d: %s", r.Status, r.Body)
	}
	if r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()); r.Status != 409 || !strings.Contains(string(r.Body), "invalid_state") {
		t.Errorf("second finalize with a new key = %d %s, want 409 invalid_state", r.Status, r.Body)
	}
	if w.ledgerTxCount(t, exp) != 1 || w.auditCount(t, exp) != 1 {
		t.Errorf("after two finalizes: ledger transactions %d, audit events %d, want 1 and 1", w.ledgerTxCount(t, exp), w.auditCount(t, exp))
	}
	w.assertLedgerMatchesClaims(t, exp)

	// And once finalized, claims are frozen.
	if r := w.put(t, w.owner, w.group, exp, items[0]); r.Status != 409 {
		t.Errorf("claim after finalize = %d, want 409", r.Status)
	}
	if r := w.del(t, w.member, w.group, exp, items[0]); r.Status != 409 {
		t.Errorf("unclaim after finalize = %d, want 409", r.Status)
	}
	w.assertLedgerMatchesClaims(t, exp)
}

// ---- idempotency ----

func TestFinalize_Idempotency(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])

	key := uuid.NewString()
	first := w.finalize(t, w.member, w.group, exp, splitBody(w.memberID), key)
	if first.Status != 200 {
		t.Fatalf("finalize = %d: %s", first.Status, first.Body)
	}
	again := w.finalize(t, w.member, w.group, exp, splitBody(w.memberID), key)
	if again.Status != 200 || !bytes.Equal(again.Body, first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d replayed=%q same body=%v", again.Status, again.Header.Get("Idempotent-Replayed"), bytes.Equal(again.Body, first.Body))
	}
	// The same key for a different request (a different split list) is refused.
	if r := w.finalize(t, w.member, w.group, exp, splitBody(w.ownerID), key); r.Status != 422 {
		t.Errorf("same key, different split list = %d, want 422", r.Status)
	}
	if w.ledgerTxCount(t, exp) != 1 || w.auditCount(t, exp) != 1 {
		t.Errorf("ledger transactions %d, audit %d after replays, want 1 and 1", w.ledgerTxCount(t, exp), w.auditCount(t, exp))
	}
}

func TestFinalize_ConcurrentDuplicatesPostOnce(t *testing.T) {
	w := newWorld(t)
	exp, _ := w.newExpense(t, w.member, goodBody)
	key := uuid.NewString()

	const n = 10
	resps := make([]teststack.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i] = w.finalize(t, w.member, w.group, exp, splitBody(w.memberID, w.ownerID), key)
		}()
	}
	wg.Wait()

	replays := 0
	for i, r := range resps {
		if r.Status != 200 || !bytes.Equal(r.Body, resps[0].Body) {
			t.Fatalf("request %d = %d (body equal to request 0: %v): %s", i, r.Status, bytes.Equal(r.Body, resps[0].Body), r.Body)
		}
		if r.Header.Get("Idempotent-Replayed") == "true" {
			replays++
		}
	}
	if replays != n-1 {
		t.Errorf("replays = %d, want %d", replays, n-1)
	}
	if w.ledgerTxCount(t, exp) != 1 || w.auditCount(t, exp) != 1 {
		t.Fatalf("ledger transactions %d, audit %d, want exactly 1 each", w.ledgerTxCount(t, exp), w.auditCount(t, exp))
	}
	w.assertLedgerMatchesClaims(t, exp)
}

// Many DIFFERENT keys at once, all for the same expense: exactly one wins, the
// rest get 409 invalid_state, and there is one posting. This is what the row lock
// and the one-posting-per-expense index are for; the key cannot help here.
func TestFinalize_ConcurrentDifferentKeysPostOnce(t *testing.T) {
	w := newWorld(t)
	exp, _ := w.newExpense(t, w.member, goodBody)

	const n = 10
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = w.finalize(t, w.member, w.group, exp, splitBody(w.memberID), uuid.NewString()).Status
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
	if w.ledgerTxCount(t, exp) != 1 || w.auditCount(t, exp) != 1 {
		t.Fatalf("ledger transactions %d, audit %d, want exactly 1 each", w.ledgerTxCount(t, exp), w.auditCount(t, exp))
	}
	w.assertLedgerMatchesClaims(t, exp)
}

// ---- concurrency with claimers ----

// A claim that is already in flight (holding FOR SHARE, uncommitted) must be
// waited for, and must then be part of what finalize reads.
func TestFinalize_WaitsForAnInFlightClaim(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.member, w.group, exp, items[0])

	inFlight := w.begin(t) // an owner claim of item 1, not yet committed
	lock(t, inFlight, "FOR SHARE", exp)

	done := make(chan teststack.Response, 1)
	go func() { done <- w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()) }()
	select {
	case r := <-done:
		t.Fatalf("finalize completed (%d %s) while a claim was in flight; it must wait", r.Status, r.Body)
	case <-time.After(300 * time.Millisecond):
	}

	if _, err := inFlight.Exec(context.Background(),
		`INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`, items[1], exp, w.group, w.ownerID); err != nil {
		t.Fatal(err)
	}
	if err := inFlight.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		// Item 1 had no claimant when finalize started; the in-flight claim made it
		// in, so finalize succeeds instead of reporting it unclaimed.
		if r.Status != 200 {
			t.Fatalf("finalize after the claim committed = %d %s, want 200 (the claim must be visible)", r.Status, r.Body)
		}
		var f finalized
		r.JSON(t, &f)
		if len(f.Items[1].ClaimedBy) != 1 || f.Items[1].ClaimedBy[0] != w.ownerID {
			t.Errorf("item 1 claimed_by = %v, want the owner", f.Items[1].ClaimedBy)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("finalize never returned")
	}
	w.assertLedgerMatchesClaims(t, exp)
}

// Real finalize against random claimers. After each round the ledger must be
// exactly what the claims table implies, and the claims must be exactly what
// finalize reported: nothing slipped in or out around it. (The deterministic
// version of the wait is TestFinalize_WaitsForAnInFlightClaim; this one hunts for
// interleavings nobody thought of.)
func TestFinalize_RaceWithClaimers(t *testing.T) {
	w := newWorld(t)
	_, code := w.s.CreateJoinCode(t, w.owner, w.group, 20, 0)
	tokens := []string{w.member, w.owner}
	for range 6 {
		tok := w.s.Token(t, "racer")
		if r := w.s.Join(t, tok, code); r.Status != http.StatusCreated {
			t.Fatalf("join = %d: %s", r.Status, r.Body)
		}
		tokens = append(tokens, tok)
	}

	tenItems := `{"description":"race","tax_minor":77,"tip_minor":33,"items":[` + strings.TrimSuffix(strings.Repeat(`{"description":"x","price_minor":101},`, 10), ",") + `]}`
	const rounds = 25
	var accepted, rejected atomic.Int64

	for round := range rounds {
		exp, items := w.newExpense(t, w.member, tenItems)
		var wg sync.WaitGroup
		for i, tok := range tokens {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(round), uint64(i)))
				for range 5000 {
					item := items[rng.IntN(len(items))]
					var status int
					if rng.IntN(4) == 0 {
						status = w.del(t, tok, w.group, exp, item).Status
					} else {
						status = w.put(t, tok, w.group, exp, item).Status
					}
					if status == 409 {
						rejected.Add(1)
						return
					}
					if status != 200 && status != 204 {
						t.Errorf("claim/unclaim = %d", status)
						return
					}
					accepted.Add(1)
				}
			}()
		}

		time.Sleep(time.Duration(2+round%5*4) * time.Millisecond)
		r := w.finalize(t, w.member, w.group, exp, splitBody(w.memberID), uuid.NewString())
		wg.Wait()
		if r.Status != 200 {
			t.Fatalf("round %d: finalize = %d: %s", round, r.Status, r.Body)
		}

		var f finalized
		r.JSON(t, &f)
		var reported []string
		for _, it := range f.Items {
			for _, m := range it.ClaimedBy {
				reported = append(reported, it.ID+"/"+m.String())
			}
		}
		slices.Sort(reported)
		if final := w.claimRows(t, exp); !slices.Equal(reported, final) {
			t.Errorf("round %d: claims changed around finalize:\n finalize reported: %v\n table after:       %v", round, reported, final)
		}
		w.assertLedgerMatchesClaims(t, exp)
	}
	if accepted.Load() == 0 || rejected.Load() == 0 {
		t.Fatalf("test did not exercise both sides: %d accepted, %d rejected", accepted.Load(), rejected.Load())
	}
	t.Logf("%d rounds: %d claim/unclaim requests accepted before finalize, %d rejected after", rounds, accepted.Load(), rejected.Load())
}

// Across everything the tests above posted to this group, balances net to zero.
func TestFinalize_GroupBalancesNetToZero(t *testing.T) {
	w := newWorld(t)
	for i := range 4 {
		exp, items := w.newExpense(t, w.member, goodBody)
		for j, it := range items {
			tok := []string{w.member, w.owner}[(i+j)%2]
			w.put(t, tok, w.group, exp, it)
		}
		if r := w.finalize(t, w.member, w.group, exp, ``, uuid.NewString()); r.Status != 200 {
			t.Fatalf("finalize = %d: %s", r.Status, r.Body)
		}
	}
	if n := w.s.QueryInt(t, `SELECT coalesce(sum(amount_minor), 0) FROM ledger_entries WHERE group_id = $1`, w.group); n != 0 {
		t.Errorf("group entries sum to %d, want 0", n)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM ledger_transactions WHERE group_id = $1`, w.group); n != 4 {
		t.Errorf("ledger transactions = %d, want 4", n)
	}
}

// An idempotency key identifies one request. The same key and body for a
// different expense is a different request: replaying would hand back the first
// expense's receipt while the second is silently left unfinalized.
func TestFinalize_KeyReusedForAnotherExpenseIsRefused(t *testing.T) {
	w := newWorld(t)
	first, _ := w.newExpense(t, w.member, goodBody)
	second, _ := w.newExpense(t, w.member, goodBody)
	key := uuid.NewString()
	body := splitBody(w.memberID)

	if r := w.finalize(t, w.member, w.group, first, body, key); r.Status != 200 {
		t.Fatalf("first finalize = %d: %s", r.Status, r.Body)
	}
	r := w.finalize(t, w.member, w.group, second, body, key)
	if r.Status != 422 {
		t.Fatalf("same key and body for another expense = %d, want 422: %s", r.Status, r.Body)
	}
	if w.ledgerTxCount(t, second) != 0 {
		t.Error("the second expense was posted by a refused request")
	}
}

// The lock query authorizes in its own statement. The handler checks the payer
// before calling it, which hides the query's own conditions; this tests them
// directly, so the query stays safe on its own if the handler is ever changed.
func TestLockExpenseForFinalize_AuthorizesInTheStatement(t *testing.T) {
	w := newWorld(t)
	exp, _ := w.newExpense(t, w.member, goodBody)
	expID, groupID := uuid.MustParse(exp), uuid.MustParse(w.group)
	otherGroup := uuid.MustParse(w.otherGroup)
	outsiderID := w.s.UserID(t, w.outsider)

	try := func(user, group uuid.UUID) error {
		ctx := context.Background()
		tx, err := w.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // lock only; nothing to commit
		_, err = db.New(tx).LockExpenseForFinalize(ctx, db.LockExpenseForFinalizeParams{UserID: user, ExpenseID: expID, GroupID: group})
		return err
	}
	if err := try(w.memberID, groupID); err != nil {
		t.Fatalf("control: the payer in the right group: %v", err)
	}
	for name, c := range map[string]struct{ user, group uuid.UUID }{
		"a member who is not the payer":     {w.ownerID, groupID},
		"a user outside the group":          {outsiderID, groupID},
		"the payer, but another group's id": {w.memberID, otherGroup},
		"an outsider using their own group": {outsiderID, otherGroup},
	} {
		if err := try(c.user, c.group); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: err = %v, want no rows", name, err)
		}
	}
}
