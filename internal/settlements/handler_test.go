package settlements_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

func TestAvailable(t *testing.T) {
	tests := []struct {
		name                                          string
		fromBal, toBal, reservedOut, reservedIn, want int64
	}{
		{"sender owes 600, payee is owed 1000", -600, 1000, 0, 0, 600},
		{"payee is the limit", -5600, 1000, 0, 0, 1000},
		{"sender is the limit", -600, 5000, 0, 0, 600},
		{"sender's open settlements reduce it", -600, 1000, 250, 0, 350},
		{"payee's open settlements reduce it", -5600, 1000, 0, 400, 600},
		{"fully reserved", -600, 1000, 600, 0, 0},
		{"over-reserved never goes negative", -600, 1000, 900, 0, 0},
		{"sender is owed money: cannot send", 300, 1000, 0, 0, 0},
		{"sender at zero", 0, 1000, 0, 0, 0},
		{"payee owes money: cannot receive", -600, -200, 0, 0, 0},
	}
	for _, tc := range tests {
		if got := settlements.Available(tc.fromBal, tc.toBal, tc.reservedOut, tc.reservedIn); got != tc.want {
			t.Errorf("%s: Available(%d, %d, %d, %d) = %d, want %d", tc.name, tc.fromBal, tc.toBal, tc.reservedOut, tc.reservedIn, got, tc.want)
		}
	}
}

// ---- API fixtures ----

type env struct {
	s     *teststack.Stack
	group string
	tok   map[string]string
	id    map[string]uuid.UUID
}

// newEnv makes a group owned by names[0] with the rest joined as members.
func newEnv(t *testing.T, names ...string) *env {
	t.Helper()
	e := &env{s: teststack.New(t), tok: map[string]string{}, id: map[string]uuid.UUID{}}
	for _, n := range names {
		e.tok[n] = e.s.Token(t, n)
		e.id[n] = e.s.UserID(t, e.tok[n])
	}
	e.group = e.s.CreateGroup(t, e.tok[names[0]], "settle")
	_, code := e.s.CreateJoinCode(t, e.tok[names[0]], e.group, 0, 0)
	for _, n := range names[1:] {
		if r := e.s.Join(t, e.tok[n], code); r.Status != http.StatusCreated {
			t.Fatalf("join %s = %d: %s", n, r.Status, r.Body)
		}
	}
	return e
}

// owe makes debtor owe payer `amount` in the ledger: payer records one item, debtor
// claims it, payer finalizes. It goes through the real endpoints.
func (e *env) owe(t *testing.T, payer, debtor string, amount int64) {
	t.Helper()
	exp := e.s.CreateExpense(t, e.tok[payer], e.group,
		fmt.Sprintf(`{"description":"owe","items":[{"description":"x","price_minor":%d}]}`, amount))
	var d struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	e.s.Do(t, e.tok[payer], "GET", "/v1/groups/"+e.group+"/expenses/"+exp, "").JSON(t, &d)
	claim := "/v1/groups/" + e.group + "/expenses/" + exp + "/items/" + d.Items[0].ID + "/claim"
	if r := e.s.Do(t, e.tok[debtor], "PUT", claim, ""); r.Status != 200 {
		t.Fatalf("claim = %d: %s", r.Status, r.Body)
	}
	r := e.s.Do(t, e.tok[payer], "POST", "/v1/groups/"+e.group+"/expenses/"+exp+"/finalize", `{}`, "Idempotency-Key", uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
}

func (e *env) body(to string, amount int64) string {
	return fmt.Sprintf(`{"to_member_id":"%s","amount_minor":%d}`, e.id[to], amount)
}

func (e *env) settle(t *testing.T, from, to string, amount int64) teststack.Response {
	t.Helper()
	return e.post(t, from, e.body(to, amount), uuid.NewString())
}

func (e *env) post(t *testing.T, from, body, key string) teststack.Response {
	t.Helper()
	return e.s.Do(t, e.tok[from], "POST", "/v1/groups/"+e.group+"/settlements", body, "Idempotency-Key", key)
}

type settlement struct {
	ID                  uuid.UUID  `json:"id"`
	GroupID             uuid.UUID  `json:"group_id"`
	FromMemberID        uuid.UUID  `json:"from_member_id"`
	ToMemberID          uuid.UUID  `json:"to_member_id"`
	AmountMinor         int64      `json:"amount_minor"`
	Currency            string     `json:"currency"`
	Status              string     `json:"status"`
	FailureCode         *string    `json:"failure_code"`
	LedgerTransactionID *uuid.UUID `json:"ledger_transaction_id"`
	ResolvedAt          *time.Time `json:"resolved_at"`
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	return e.s.QueryInt(t, sql, args...)
}

func (e *env) settlementCount(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM settlements WHERE group_id = $1`, e.group)
}

func (e *env) jobCount(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM jobs j JOIN settlements s ON s.id::text = j.payload->>'settlement_id'
	                   WHERE j.kind = 'settlement.execute' AND s.group_id = $1`, e.group)
}

// ---- create ----

func TestCreate_AcceptsAndEnqueuesInOneTransaction(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 1500)

	r := e.settle(t, "alice", "owner", 1000)
	if r.Status != http.StatusAccepted {
		t.Fatalf("create = %d: %s", r.Status, r.Body)
	}
	var s settlement
	r.JSON(t, &s)
	if s.Status != "pending" || s.AmountMinor != 1000 || s.Currency != "CAD" || s.FromMemberID != e.id["alice"] ||
		s.ToMemberID != e.id["owner"] || s.GroupID.String() != e.group || s.FailureCode != nil || s.LedgerTransactionID != nil || s.ResolvedAt != nil {
		t.Errorf("settlement = %+v", s)
	}

	// The execute job is queued, carries only the id, and an audit event exists.
	var kind, state, payload string
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT kind, state, payload::text FROM jobs WHERE payload->>'settlement_id' = $1`, s.ID.String()).Scan(&kind, &state, &payload); err != nil {
		t.Fatalf("no job for the settlement: %v", err)
	}
	if kind != settlements.KindExecute || state != "queued" || strings.Count(payload, ":") != 1 {
		t.Errorf("job = %s %s %s", kind, state, payload)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE group_id = $1 AND kind = 'settlement.created' AND data->>'settlement_id' = $2`, e.group, s.ID.String()); n != 1 {
		t.Errorf("created audit events = %d, want 1", n)
	}
	// Creating moves no money: the ledger waits for a confirmed success.
	if n := e.count(t, `SELECT count(*) FROM ledger_transactions WHERE settlement_id = $1`, s.ID.String()); n != 0 {
		t.Errorf("a pending settlement has %d ledger postings", n)
	}

	// Both members can read it, by id and in the list.
	for _, who := range []string{"alice", "owner"} {
		var got settlement
		g := e.s.Do(t, e.tok[who], "GET", "/v1/groups/"+e.group+"/settlements/"+s.ID.String(), "")
		if g.Status != 200 {
			t.Fatalf("%s get = %d: %s", who, g.Status, g.Body)
		}
		g.JSON(t, &got)
		if got != s {
			t.Errorf("%s reads %+v, created %+v", who, got, s)
		}
		var list struct {
			Settlements []settlement `json:"settlements"`
		}
		e.s.Do(t, e.tok[who], "GET", "/v1/groups/"+e.group+"/settlements", "").JSON(t, &list)
		if len(list.Settlements) != 1 || list.Settlements[0] != s {
			t.Errorf("%s list = %+v", who, list.Settlements)
		}
	}
}

func TestCreate_Validation(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 1500)
	stranger := e.s.UserID(t, e.s.Token(t, "stranger")) // a real user, not in the group
	ok := e.id["owner"].String()

	tests := []struct {
		name, body string
		key        string
		want       int
	}{
		{"valid", `{"to_member_id":"` + ok + `","amount_minor":10}`, uuid.NewString(), 202},
		{"no key", `{"to_member_id":"` + ok + `","amount_minor":10}`, "", 400},
		{"empty body", ``, uuid.NewString(), 400},
		{"not json", `nope`, uuid.NewString(), 400},
		{"missing to", `{"amount_minor":10}`, uuid.NewString(), 400},
		{"missing amount", `{"to_member_id":"` + ok + `"}`, uuid.NewString(), 400},
		{"to is not a uuid", `{"to_member_id":"alice","amount_minor":10}`, uuid.NewString(), 400},
		{"zero amount", `{"to_member_id":"` + ok + `","amount_minor":0}`, uuid.NewString(), 400},
		{"negative amount", `{"to_member_id":"` + ok + `","amount_minor":-5}`, uuid.NewString(), 400},
		{"over the cap", `{"to_member_id":"` + ok + `","amount_minor":100000001}`, uuid.NewString(), 400},
		{"float amount", `{"to_member_id":"` + ok + `","amount_minor":10.5}`, uuid.NewString(), 400},
		{"string amount", `{"to_member_id":"` + ok + `","amount_minor":"10"}`, uuid.NewString(), 400},
		{"unknown field (a from)", `{"to_member_id":"` + ok + `","amount_minor":10,"from_member_id":"` + ok + `"}`, uuid.NewString(), 400},
		{"paying yourself", `{"to_member_id":"` + e.id["alice"].String() + `","amount_minor":10}`, uuid.NewString(), 400},
		{"a user outside the group", `{"to_member_id":"` + stranger.String() + `","amount_minor":10}`, uuid.NewString(), 400},
		{"a user that does not exist", `{"to_member_id":"` + uuid.NewString() + `","amount_minor":10}`, uuid.NewString(), 400},
	}
	wantRows := 0
	for _, tc := range tests {
		headers := []string{}
		if tc.key != "" {
			headers = []string{"Idempotency-Key", tc.key}
		}
		r := e.s.Do(t, e.tok["alice"], "POST", "/v1/groups/"+e.group+"/settlements", tc.body, headers...)
		if r.Status != tc.want {
			t.Errorf("%s: status %d, want %d: %s", tc.name, r.Status, tc.want, r.Body)
		}
		if tc.want == 202 {
			wantRows++
		}
	}
	if n := e.settlementCount(t); n != wantRows {
		t.Errorf("%d settlements after the table, want %d (a rejected request wrote one)", n, wantRows)
	}
	if n := e.jobCount(t); n != wantRows {
		t.Errorf("%d jobs after the table, want %d", n, wantRows)
	}
}

// The check order is membership, key, body: a non-member learns nothing.
func TestCreate_NonMemberIsRefusedBeforeAnythingElse(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	out := e.s.Token(t, "out")
	for _, body := range []string{`{"to_member_id":"` + e.id["owner"].String() + `","amount_minor":1}`, `garbage`, ``} {
		r := e.s.Do(t, out, "POST", "/v1/groups/"+e.group+"/settlements", body) // no key either
		if r.Status != 404 {
			t.Errorf("non-member with body %q: %d, want 404: %s", body, r.Status, r.Body)
		}
	}
}

// ---- idempotency ----

func TestCreate_IdempotentReplayAndKeyReuse(t *testing.T) {
	e := newEnv(t, "owner", "alice", "bob")
	e.owe(t, "owner", "alice", 3000)
	key := uuid.NewString()
	body := e.body("owner", 700)

	first := e.post(t, "alice", body, key)
	if first.Status != 202 {
		t.Fatalf("first = %d: %s", first.Status, first.Body)
	}
	// Whitespace and key order do not make it a different request.
	again := e.post(t, "alice", fmt.Sprintf(`{ "amount_minor": 700,  "to_member_id": "%s" }`, e.id["owner"]), key)
	if again.Status != 202 || string(again.Body) != string(first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d replayed=%q:\n%s\nfirst:\n%s", again.Status, again.Header.Get("Idempotent-Replayed"), again.Body, first.Body)
	}
	if e.settlementCount(t) != 1 || e.jobCount(t) != 1 {
		t.Errorf("replay created rows: settlements=%d jobs=%d", e.settlementCount(t), e.jobCount(t))
	}

	// Same key, different amount / payee: refused, nothing written.
	for name, b := range map[string]string{"amount": e.body("owner", 701), "payee": e.body("bob", 700)} {
		if r := e.post(t, "alice", b, key); r.Status != 422 {
			t.Errorf("key reuse with another %s = %d, want 422: %s", name, r.Status, r.Body)
		}
	}
	// Another user's use of the same key string is their own key.
	e.owe(t, "owner", "bob", 3000)
	if r := e.post(t, "bob", e.body("owner", 700), key); r.Status != 202 {
		t.Errorf("same key string, different user = %d, want 202: %s", r.Status, r.Body)
	}
	if e.settlementCount(t) != 2 {
		t.Errorf("settlements = %d, want 2", e.settlementCount(t))
	}
}

// A business-rule refusal is stored and replayed too (Stripe model), even if the
// balances change in between; the way out is a new key. Nothing is written.
func TestCreate_RefusalIsReplayedAndWritesNothing(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 500)
	key := uuid.NewString()
	r := e.post(t, "alice", e.body("owner", 600), key)
	if r.Status != 409 || !strings.Contains(string(r.Body), "exceeds_available") || !strings.Contains(string(r.Body), "500") {
		t.Fatalf("over-limit = %d: %s", r.Status, r.Body)
	}
	if e.settlementCount(t) != 0 || e.jobCount(t) != 0 {
		t.Fatal("a refused settlement wrote rows")
	}
	e.owe(t, "owner", "alice", 5000) // now 600 would be fine
	replay := e.post(t, "alice", e.body("owner", 600), key)
	if replay.Status != 409 || replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay of the refusal = %d replayed=%q", replay.Status, replay.Header.Get("Idempotent-Replayed"))
	}
	if r := e.settle(t, "alice", "owner", 600); r.Status != 202 {
		t.Errorf("a new key = %d, want 202: %s", r.Status, r.Body)
	}
}

func TestCreate_ConcurrentDuplicatesCreateOne(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 5000)
	key, body := uuid.NewString(), e.body("owner", 800)

	const n = 10
	results := make([]teststack.Response, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = e.post(t, "alice", body, key)
		}()
	}
	close(start)
	wg.Wait()
	for i, r := range results {
		if r.Status != 202 || string(r.Body) != string(results[0].Body) {
			t.Errorf("request %d = %d: %s", i, r.Status, r.Body)
		}
	}
	if e.settlementCount(t) != 1 || e.jobCount(t) != 1 {
		t.Errorf("settlements=%d jobs=%d, want 1 and 1", e.settlementCount(t), e.jobCount(t))
	}
}

// ---- availability ----

func TestCreate_AvailabilityIsBoundedByBothSides(t *testing.T) {
	// owner is owed 1000 (alice 600 + bob 400); carol is owed 5000 by alice.
	e := newEnv(t, "owner", "alice", "bob", "carol")
	e.owe(t, "owner", "alice", 600)
	e.owe(t, "owner", "bob", 400)
	e.owe(t, "carol", "alice", 5000) // alice owes 5600 in all

	refused := func(from, to string, amount int64, wantAvail int64) {
		t.Helper()
		r := e.settle(t, from, to, amount)
		if r.Status != 409 || !strings.Contains(string(r.Body), fmt.Sprintf("at most %d ", wantAvail)) {
			t.Errorf("%s -> %s %d = %d, want 409 with at most %d: %s", from, to, amount, r.Status, wantAvail, r.Body)
		}
	}
	accepted := func(from, to string, amount int64) {
		t.Helper()
		if r := e.settle(t, from, to, amount); r.Status != 202 {
			t.Errorf("%s -> %s %d = %d, want 202: %s", from, to, amount, r.Status, r.Body)
		}
	}

	// The payee is the limit: alice owes 5600 but owner is only owed 1000.
	refused("alice", "owner", 1001, 1000)
	accepted("alice", "owner", 1000)
	// Owner's 1000 is now reserved, so nothing more can go to them ...
	refused("alice", "owner", 1, 0)
	// ... and alice's sender side has 4600 left, which is the limit toward carol.
	refused("alice", "carol", 4601, 4600)
	accepted("alice", "carol", 4600)
	refused("alice", "carol", 1, 0)

	// Someone who is owed money, or who owes nothing, cannot send.
	refused("owner", "alice", 1, 0)
	refused("carol", "owner", 1, 0)
	// bob owes 400, but owner's capacity is already reserved by alice.
	refused("bob", "owner", 400, 0)
	// Nobody who owes the group can send to someone who owes the group.
	refused("bob", "alice", 1, 0)
}

// An open settlement reserves money on both sides: the sender's room to send and the
// payee's room to receive. `unknown` may have moved money, so it keeps reserving; a
// failed one releases both. Each side is probed by a request that only that side can
// refuse, so one side's rule cannot hide the other's.
func TestCreate_OpenSettlementsReserveBothSides(t *testing.T) {
	e := newEnv(t, "owner", "alice", "carol", "dave")
	e.owe(t, "owner", "alice", 1000) // alice owes 1000; owner is owed 1000
	e.owe(t, "carol", "dave", 9000)  // dave owes 9000; carol is owed 9000

	var s settlement
	r := e.settle(t, "alice", "owner", 1000)
	if r.Status != 202 {
		t.Fatalf("create = %d: %s", r.Status, r.Body)
	}
	r.JSON(t, &s)

	// alice -> carol: carol has 9000 of room, so only alice's own reservation can refuse it.
	// dave -> owner: dave can send 9000, so only owner's reserved room can refuse it.
	probe := func(state string, want int) {
		t.Helper()
		if got := e.settle(t, "alice", "carol", 1).Status; got != want {
			t.Errorf("%s: alice -> carol = %d, want %d (sender side)", state, got, want)
		}
		if got := e.settle(t, "dave", "owner", 1).Status; got != want {
			t.Errorf("%s: dave -> owner = %d, want %d (payee side)", state, got, want)
		}
	}
	probe("pending", 409)
	e.s.Exec(t, `UPDATE settlements SET status = 'unknown' WHERE id = $1`, s.ID)
	probe("unknown", 409)
	e.s.Exec(t, `UPDATE settlements SET status = 'failed', failure_code = 'declined', resolved_at = now() WHERE id = $1`, s.ID)
	probe("failed", 202)
}

// ---- the group lock ----

// Settlement creation takes FOR NO KEY UPDATE on the group row. Hold that lock (taken
// with the production query) the way a concurrent creation would and show creation waits for it; and show the lock
// does NOT stop ordinary work in the group (creating an expense inserts a row with a
// foreign key to the group, which takes FOR KEY SHARE and would block behind a plain
// FOR UPDATE).
func TestCreate_WaitsForTheGroupLockButNothingElseDoes(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 1000)

	ctx := context.Background()
	holder, err := e.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	// The production query itself, so this test checks ITS lock mode, not one written here.
	if _, err := db.New(holder).LockGroupForSettlement(ctx, uuid.MustParse(e.group)); err != nil {
		t.Fatal(err)
	}

	// An expense can still be created (and a membership read) under that lock.
	expenseDone := make(chan teststack.Response, 1)
	go func() {
		expenseDone <- e.s.Do(t, e.tok["alice"], "POST", "/v1/groups/"+e.group+"/expenses",
			`{"description":"x","items":[{"description":"y","price_minor":1}]}`, "Idempotency-Key", uuid.NewString())
	}()
	select {
	case r := <-expenseDone:
		if r.Status != 201 {
			t.Errorf("expense under the group lock = %d: %s", r.Status, r.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("creating an expense blocked behind the settlement lock (it must not)")
	}

	// A settlement creation waits.
	created := make(chan teststack.Response, 1)
	go func() { created <- e.settle(t, "alice", "owner", 500) }()
	select {
	case r := <-created:
		t.Fatalf("settlement was created while the group lock was held: %d %s", r.Status, r.Body)
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-created:
		if r.Status != 202 {
			t.Errorf("after release: %d %s", r.Status, r.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settlement creation did not proceed after the lock was released")
	}
}

// Randomized complement: many creations at once cannot reserve more than is owed.
func TestCreate_ConcurrentCreationsCannotOverReserve(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 1000)

	const n = 12 // each asks for 300: at most 3 fit in 1000
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses[i] = e.settle(t, "alice", "owner", 300).Status
		}()
	}
	close(start)
	wg.Wait()

	accepted := 0
	for _, s := range statuses {
		switch s {
		case 202:
			accepted++
		case 409:
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	var reserved int
	if err := e.s.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount_minor), 0) FROM settlements WHERE group_id = $1 AND status IN ('pending', 'unknown')`, e.group).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if accepted != 3 || reserved != 900 || e.jobCount(t) != 3 {
		t.Errorf("accepted=%d reserved=%d jobs=%d, want 3, 900, 3 (statuses %v)", accepted, reserved, e.jobCount(t), statuses)
	}
}

// ---- read ----

func TestRead_ListIsNewestFirst(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 3000)
	var ids []string
	for _, amt := range []int64{100, 200, 300} {
		var s settlement
		e.settle(t, "alice", "owner", amt).JSON(t, &s)
		ids = append(ids, s.ID.String())
	}
	var list struct {
		Settlements []settlement `json:"settlements"`
	}
	e.s.Do(t, e.tok["owner"], "GET", "/v1/groups/"+e.group+"/settlements", "").JSON(t, &list)
	if len(list.Settlements) != 3 {
		t.Fatalf("list has %d, want 3", len(list.Settlements))
	}
	for i, s := range list.Settlements {
		if want := ids[len(ids)-1-i]; s.ID.String() != want {
			t.Errorf("position %d = %s, want %s (newest first)", i, s.ID, want)
		}
	}

	// An empty list is 200, not 404.
	empty := newEnv(t, "owner", "alice")
	if r := empty.s.Do(t, empty.tok["alice"], "GET", "/v1/groups/"+empty.group+"/settlements", ""); r.Status != 200 || !strings.Contains(string(r.Body), `"settlements":[]`) {
		t.Errorf("empty list = %d: %s", r.Status, r.Body)
	}
}
