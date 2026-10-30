package expenses_test

import (
	"bytes"
	"context"
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

	"github.com/riyne/fifty-fifty/internal/teststack"
)

func claimURL(group, expense, item string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/items/" + item + "/claim"
}

func (w *world) put(t testing.TB, token, group, expense, item string) teststack.Response {
	return w.s.Do(t, token, "PUT", claimURL(group, expense, item), "")
}

func (w *world) del(t testing.TB, token, group, expense, item string) teststack.Response {
	return w.s.Do(t, token, "DELETE", claimURL(group, expense, item), "")
}

// newExpense creates an expense in the world's group and returns its id and item ids in order.
func (w *world) newExpense(t testing.TB, token, body string) (string, []string) {
	t.Helper()
	id := w.s.CreateExpense(t, token, w.group, body)
	var e expense
	w.s.Do(t, token, "GET", "/v1/groups/"+w.group+"/expenses/"+id, "").JSON(t, &e)
	items := make([]string, len(e.Items))
	for i, it := range e.Items {
		items[i] = it.ID
	}
	return id, items
}

func (w *world) claimedBy(t testing.TB, token, expenseID string) [][]uuid.UUID {
	t.Helper()
	var e expense
	w.s.Do(t, token, "GET", "/v1/groups/"+w.group+"/expenses/"+expenseID, "").JSON(t, &e)
	out := make([][]uuid.UUID, len(e.Items))
	for i, it := range e.Items {
		out[i] = it.ClaimedBy
	}
	return out
}

func (w *world) claimRows(t testing.TB, expenseID string) []string {
	t.Helper()
	rows, err := w.s.Pool.Query(context.Background(), `SELECT item_id::text || '/' || member_id::text FROM claims WHERE expense_id = $1 ORDER BY 1`, expenseID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestClaim_ClaimAndUnclaim(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)

	// Claim: the response is the item with its claimants.
	r := w.put(t, w.member, w.group, exp, items[0])
	if r.Status != 200 {
		t.Fatalf("claim = %d: %s", r.Status, r.Body)
	}
	var it struct {
		ID         string      `json:"id"`
		PriceMinor int64       `json:"price_minor"`
		ClaimedBy  []uuid.UUID `json:"claimed_by"`
	}
	r.JSON(t, &it)
	if it.ID != items[0] || it.PriceMinor != 1500 || len(it.ClaimedBy) != 1 || it.ClaimedBy[0] != w.memberID {
		t.Fatalf("claim response = %+v, want item 0 claimed by the member", it)
	}

	// Claiming again changes nothing and is still a 200.
	if r := w.put(t, w.member, w.group, exp, items[0]); r.Status != 200 {
		t.Fatalf("repeat claim = %d", r.Status)
	}
	if rows := w.claimRows(t, exp); len(rows) != 1 {
		t.Fatalf("claim rows after a repeat claim = %v, want exactly 1", rows)
	}

	// A second member claiming the same item shares it; the owner may claim a
	// member's expense too (anyone in the group claims for themselves).
	r = w.put(t, w.owner, w.group, exp, items[0])
	var shared struct {
		ClaimedBy []uuid.UUID `json:"claimed_by"`
	}
	r.JSON(t, &shared)
	if r.Status != 200 || len(shared.ClaimedBy) != 2 || shared.ClaimedBy[0].String() >= shared.ClaimedBy[1].String() {
		t.Fatalf("shared claim = %d %+v, want two claimants sorted", r.Status, shared)
	}
	if r := w.put(t, w.owner, w.group, exp, items[1]); r.Status != 200 {
		t.Fatalf("owner claim item 1 = %d", r.Status)
	}

	// Unclaim removes only the caller's own claim.
	if r := w.del(t, w.owner, w.group, exp, items[0]); r.Status != 204 || len(r.Body) != 0 {
		t.Fatalf("unclaim = %d %q, want 204 with no body", r.Status, r.Body)
	}
	got := w.claimedBy(t, w.member, exp)
	if len(got[0]) != 1 || got[0][0] != w.memberID {
		t.Errorf("after the owner unclaimed item 0, claimed_by = %v, want just the member (the owner must not remove other people's claims)", got[0])
	}
	if len(got[1]) != 1 || got[1][0] != w.ownerID {
		t.Errorf("item 1 claimed_by = %v, want the owner (unclaiming item 0 must not touch item 1)", got[1])
	}

	// Unclaiming something you do not hold is a no-op, not an error.
	if r := w.del(t, w.owner, w.group, exp, items[0]); r.Status != 204 {
		t.Errorf("unclaim of an item not held = %d, want 204", r.Status)
	}
	if rows := w.claimRows(t, exp); len(rows) != 2 {
		t.Errorf("claim rows = %v, want 2 (member/item0, owner/item1)", rows)
	}
}

func TestClaim_OnlyWhileClaiming(t *testing.T) {
	w := newWorld(t)
	for _, status := range []string{"parsing", "needs_review", "finalized", "void"} {
		t.Run(status, func(t *testing.T) {
			exp, items := w.newExpense(t, w.member, goodBody)
			w.put(t, w.member, w.group, exp, items[0]) // a claim to try to remove
			set := `UPDATE expenses SET status = $2 WHERE id = $1`
			switch status {
			case "finalized":
				set = `UPDATE expenses SET status = $2, finalized_at = now() WHERE id = $1`
			case "void":
				set = `UPDATE expenses SET status = $2, finalized_at = now(), voided_at = now() WHERE id = $1`
			}
			w.s.Exec(t, set, exp, status)
			before := w.claimRows(t, exp)

			r := w.put(t, w.owner, w.group, exp, items[1])
			if r.Status != 409 || !strings.Contains(string(r.Body), "expense_not_claimable") {
				t.Errorf("claim on a %s expense = %d %s, want 409 expense_not_claimable", status, r.Status, r.Body)
			}
			if r := w.del(t, w.member, w.group, exp, items[0]); r.Status != 409 {
				t.Errorf("unclaim on a %s expense = %d, want 409", status, r.Status)
			}
			if after := w.claimRows(t, exp); !slices.Equal(before, after) {
				t.Errorf("a refused request changed claims: %v -> %v", before, after)
			}
		})
	}
}

func TestClaim_NotFoundVariantsAreIdentical(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	otherExp := w.s.CreateExpense(t, w.outsider, w.otherGroup, goodBody)
	var oe expense
	w.s.Do(t, w.outsider, "GET", "/v1/groups/"+w.otherGroup+"/expenses/"+otherExp, "").JSON(t, &oe)
	otherItem := oe.Items[0].ID
	exp2, items2 := w.newExpense(t, w.member, goodBody) // same group, different expense
	_ = exp2

	// For a non-member every variant is the same "expense not found".
	wantExpenseGone := w.put(t, w.member, w.group, uuid.NewString(), items[0])
	if wantExpenseGone.Status != 404 || !strings.Contains(string(wantExpenseGone.Body), "expense not found") {
		t.Fatalf("ghost expense = %d %s", wantExpenseGone.Status, wantExpenseGone.Body)
	}
	for name, r := range map[string]teststack.Response{
		"non-member, real ids":             w.put(t, w.outsider, w.group, exp, items[0]),
		"non-member, malformed item id":    w.put(t, w.outsider, w.group, exp, "nope"),
		"non-member unclaim, real ids":     w.del(t, w.outsider, w.group, exp, items[0]),
		"malformed expense id":             w.put(t, w.member, w.group, "nope", items[0]),
		"malformed group id":               w.put(t, w.member, "nope", exp, items[0]),
		"expense of another group":         w.put(t, w.member, w.group, otherExp, otherItem),
		"real expense through wrong group": w.put(t, w.outsider, w.otherGroup, exp, items[0]),
	} {
		if r.Status != 404 || !bytes.Equal(r.Body, wantExpenseGone.Body) {
			t.Errorf("%s: %d %s, want the same 404 as a nonexistent expense: %s", name, r.Status, r.Body, wantExpenseGone.Body)
		}
	}

	// A member gets "item not found" for items that are not in this expense, and
	// the same body for a malformed id, a ghost, another expense's and another group's item.
	wantItemGone := w.put(t, w.member, w.group, exp, uuid.NewString())
	if wantItemGone.Status != 404 || !strings.Contains(string(wantItemGone.Body), "item not found") {
		t.Fatalf("ghost item = %d %s", wantItemGone.Status, wantItemGone.Body)
	}
	for name, r := range map[string]teststack.Response{
		"malformed item id":                   w.put(t, w.member, w.group, exp, "nope"),
		"item of another expense, same group": w.put(t, w.member, w.group, exp, items2[0]),
		"item of another group":               w.put(t, w.member, w.group, exp, otherItem),
		"unclaim, item of another expense":    w.del(t, w.member, w.group, exp, items2[0]),
	} {
		if r.Status != 404 || !bytes.Equal(r.Body, wantItemGone.Body) {
			t.Errorf("%s: %d %s, want the same 404 as a nonexistent item: %s", name, r.Status, r.Body, wantItemGone.Body)
		}
	}
	if n := len(w.claimRows(t, exp)) + len(w.claimRows(t, exp2)) + len(w.claimRows(t, otherExp)); n != 0 {
		t.Errorf("refused requests created %d claims", n)
	}
}

func TestClaim_Unauthenticated(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	if r := w.put(t, "", w.group, exp, items[0]); r.Status != 401 {
		t.Errorf("claim without a token = %d, want 401", r.Status)
	}
	if r := w.del(t, "", w.group, exp, items[0]); r.Status != 401 {
		t.Errorf("unclaim without a token = %d, want 401", r.Status)
	}
}

// ---- concurrency ----
//
// Finalize does not exist yet (M2.5), so a raw transaction plays its part:
// SELECT ... FOR UPDATE on the expense row, which is exactly the lock finalize
// will take.

func (w *world) begin(t testing.TB) pgx.Tx {
	t.Helper()
	tx, err := w.s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func lock(t testing.TB, tx pgx.Tx, mode, expenseID string) {
	t.Helper()
	var status string
	if err := tx.QueryRow(context.Background(), `SELECT status FROM expenses WHERE id = $1 `+mode, expenseID).Scan(&status); err != nil {
		t.Fatalf("lock %s: %v", mode, err)
	}
}

// Claimers take FOR SHARE, which is compatible with FOR SHARE. If claiming
// escalated to a stronger lock, N people claiming at once would queue up.
func TestClaim_ClaimersDoNotBlockEachOther(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)

	inFlight := w.begin(t) // another claim, mid-transaction, holding FOR SHARE
	lock(t, inFlight, "FOR SHARE", exp)

	done := make(chan teststack.Response, 1)
	go func() { done <- w.put(t, w.owner, w.group, exp, items[0]) }()
	select {
	case r := <-done:
		if r.Status != 200 {
			t.Fatalf("claim = %d: %s", r.Status, r.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a claim blocked behind another in-flight claim: claimers must share the lock")
	}
}

// A claim that arrives while finalize holds the lock must wait for it, and then
// see the new status and be rejected. It must not be accepted, and must not be
// rejected early on a stale 'claiming' read and then allowed through anyway.
func TestClaim_WaitsForFinalizerThenIsRejected(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)

	finalizer := w.begin(t)
	lock(t, finalizer, "FOR UPDATE", exp)

	done := make(chan teststack.Response, 1)
	go func() { done <- w.put(t, w.member, w.group, exp, items[0]) }()
	select {
	case r := <-done:
		t.Fatalf("a claim completed (%d %s) while the expense was locked FOR UPDATE; it must wait", r.Status, r.Body)
	case <-time.After(300 * time.Millisecond):
	}

	if _, err := finalizer.Exec(context.Background(), `UPDATE expenses SET status = 'finalized', finalized_at = now() WHERE id = $1`, exp); err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.Status != 409 {
			t.Fatalf("claim that waited for finalize = %d %s, want 409", r.Status, r.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the claim never returned after the finalizer committed")
	}
	if rows := w.claimRows(t, exp); len(rows) != 0 {
		t.Fatalf("a claim was accepted after finalize: %v", rows)
	}
}

// Item edits (M2.8) are a plain UPDATE of the expense row, which takes FOR NO KEY
// UPDATE. FOR SHARE conflicts with that, FOR KEY SHARE would not, so this pins
// the lock strength DESIGN.md specifies: a claim must not land while an edit to
// the expense is in flight.
func TestClaim_WaitsForAnEditInFlight(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)

	editor := w.begin(t)
	lock(t, editor, "FOR NO KEY UPDATE", exp)

	done := make(chan teststack.Response, 1)
	go func() { done <- w.put(t, w.member, w.group, exp, items[0]) }()
	select {
	case r := <-done:
		t.Fatalf("a claim completed (%d %s) while an edit held the row; it must wait", r.Status, r.Body)
	case <-time.After(300 * time.Millisecond):
	}
	if err := editor.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Status != 200 {
			t.Fatalf("claim after the edit committed = %d %s, want 200 (status is still claiming)", r.Status, r.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the claim never returned after the editor committed")
	}
}

// The invariant finalize depends on: the claim set it reads after taking the
// lock is the claim set that stays. Many users claim and unclaim at random while
// a stand-in finalizer takes the lock, reads the set, holds the lock for a
// moment (so claimers pile up behind it), then commits 'finalized'. Afterwards
// the table must equal what the finalizer read: nothing slipped in or out.
func TestClaim_RaceWithFinalizerLeavesTheClaimSetItRead(t *testing.T) {
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

	tenItems := `{"description":"race","items":[` + strings.TrimSuffix(strings.Repeat(`{"description":"x","price_minor":100},`, 10), ",") + `]}`
	const rounds = 15
	var accepted, rejected atomic.Int64

	for round := range rounds {
		exp, items := w.newExpense(t, w.member, tenItems)
		var wg sync.WaitGroup
		for i, tok := range tokens {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(round), uint64(i)))
				for range 5000 { // bounded, in case something is badly wrong
					item := items[rng.IntN(len(items))]
					var status int
					if rng.IntN(4) == 0 {
						status = w.del(t, tok, w.group, exp, item).Status
						if status != 204 && status != 409 {
							t.Errorf("unclaim = %d", status)
						}
					} else {
						status = w.put(t, tok, w.group, exp, item).Status
						if status != 200 && status != 409 {
							t.Errorf("claim = %d", status)
						}
					}
					if status == 409 {
						rejected.Add(1)
						return // finalized: stop
					}
					accepted.Add(1)
				}
			}()
		}

		time.Sleep(time.Duration(5+round%4*5) * time.Millisecond) // let claimers get going
		finalizer := w.begin(t)
		lock(t, finalizer, "FOR UPDATE", exp)
		read := w.claimRowsIn(t, finalizer, exp) // the set finalize would compute shares from
		time.Sleep(40 * time.Millisecond)        // claimers queue up behind the lock
		if _, err := finalizer.Exec(context.Background(), `UPDATE expenses SET status = 'finalized', finalized_at = now() WHERE id = $1`, exp); err != nil {
			t.Fatal(err)
		}
		if err := finalizer.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()

		if final := w.claimRows(t, exp); !slices.Equal(read, final) {
			t.Errorf("round %d: the claim set changed after the finalizer read it:\n read:  %v\n final: %v", round, read, final)
		}
	}
	if accepted.Load() == 0 || rejected.Load() == 0 {
		t.Fatalf("test did not exercise both sides of the race: %d accepted, %d rejected", accepted.Load(), rejected.Load())
	}
	t.Logf("%d rounds: %d requests accepted before finalize, %d rejected after", rounds, accepted.Load(), rejected.Load())
}

func (w *world) claimRowsIn(t testing.TB, tx pgx.Tx, expenseID string) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), `SELECT item_id::text || '/' || member_id::text FROM claims WHERE expense_id = $1 ORDER BY 1`, expenseID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
