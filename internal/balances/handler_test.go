package balances_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/harness/teststack"
)

// env is one group whose members are named, so tests read like the scenario.
// The first name is the group owner; the rest join through a join code.
type env struct {
	s     *teststack.Stack
	group string
	tok   map[string]string
	id    map[string]uuid.UUID
	names []string
}

func newEnv(t *testing.T, names ...string) *env {
	t.Helper()
	s := teststack.New(t)
	e := &env{s: s, tok: map[string]string{}, id: map[string]uuid.UUID{}, names: names}
	for _, n := range names {
		e.tok[n] = s.Token(t, n)
		e.id[n] = s.UserID(t, e.tok[n])
	}
	e.group = s.CreateGroup(t, e.tok[names[0]], "G")
	_, code := s.CreateJoinCode(t, e.tok[names[0]], e.group, 20, 0)
	for _, n := range names[1:] {
		if r := s.Join(t, e.tok[n], code); r.Status != http.StatusCreated {
			t.Fatalf("join %s = %d: %s", n, r.Status, r.Body)
		}
	}
	return e
}

type view struct {
	Currency string `json:"currency"`
	Balances []struct {
		MemberID     uuid.UUID `json:"member_id"`
		DisplayName  string    `json:"display_name"`
		BalanceMinor int64     `json:"balance_minor"`
	} `json:"balances"`
	Transfers []struct {
		From   uuid.UUID `json:"from_member_id"`
		To     uuid.UUID `json:"to_member_id"`
		Amount int64     `json:"amount_minor"`
	} `json:"suggested_transfers"`
}

func (e *env) balances(t testing.TB, as string) (view, teststack.Response) {
	t.Helper()
	r := e.s.Do(t, e.tok[as], "GET", "/v1/groups/"+e.group+"/balances", "")
	var v view
	if r.Status == 200 {
		r.JSON(t, &v)
	}
	return v, r
}

// byName maps each member's name to their balance.
func (e *env) byName(v view) map[string]int64 {
	out := map[string]int64{}
	for _, b := range v.Balances {
		for n, id := range e.id {
			if id == b.MemberID {
				out[n] = b.BalanceMinor
			}
		}
	}
	return out
}

// pay makes `payer` create an expense with one item per entry in cost, each priced
// as given and claimed by the member it is named for, then finalize it. No tax or
// tip, so every share is exactly the item's price and the expected balances are
// obvious: the payer is credited the sum of everyone else's items.
func (e *env) pay(t testing.TB, payer string, cost map[string]int64) {
	t.Helper()
	exp, items := e.expense(t, payer, cost)
	if r := e.finalize(t, payer, exp); r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	_ = items
}

func (e *env) expense(t testing.TB, payer string, cost map[string]int64) (string, map[string]string) {
	t.Helper()
	claimers := make([]string, 0, len(cost))
	for n := range cost {
		claimers = append(claimers, n)
	}
	slices.Sort(claimers)
	var parts []string
	for _, n := range claimers {
		parts = append(parts, fmt.Sprintf(`{"description":%q,"price_minor":%d}`, n, cost[n]))
	}
	exp := e.s.CreateExpense(t, e.tok[payer], e.group, `{"description":"d","items":[`+strings.Join(parts, ",")+`]}`)

	var detail struct {
		Items []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"items"`
	}
	e.s.Do(t, e.tok[payer], "GET", "/v1/groups/"+e.group+"/expenses/"+exp, "").JSON(t, &detail)
	itemOf := map[string]string{}
	for _, it := range detail.Items {
		itemOf[it.Description] = it.ID
		path := "/v1/groups/" + e.group + "/expenses/" + exp + "/items/" + it.ID + "/claim"
		if r := e.s.Do(t, e.tok[it.Description], "PUT", path, ""); r.Status != 200 {
			t.Fatalf("claim = %d: %s", r.Status, r.Body)
		}
	}
	return exp, itemOf
}

func (e *env) finalize(t testing.TB, payer, expense string) teststack.Response {
	return e.s.Do(t, e.tok[payer], "POST", "/v1/groups/"+e.group+"/expenses/"+expense+"/finalize", ``, "Idempotency-Key", uuid.NewString())
}

func TestBalances_EmptyGroupListsEveryoneAtZero(t *testing.T) {
	e := newEnv(t, "owner", "member")
	v, r := e.balances(t, "member")
	if r.Status != 200 {
		t.Fatalf("status = %d: %s", r.Status, r.Body)
	}
	if v.Currency != "CAD" || len(v.Balances) != 2 {
		t.Fatalf("view = %+v", v)
	}
	// Members appear in the order they joined, with their names, at zero.
	if v.Balances[0].MemberID != e.id["owner"] || v.Balances[1].MemberID != e.id["member"] ||
		v.Balances[0].DisplayName != "owner" || v.Balances[1].DisplayName != "member" {
		t.Errorf("balances = %+v, want owner then member", v.Balances)
	}
	for _, b := range v.Balances {
		if b.BalanceMinor != 0 {
			t.Errorf("fresh group: %s at %d", b.DisplayName, b.BalanceMinor)
		}
	}
	// An empty list, not null, so clients can iterate it.
	if !strings.Contains(string(r.Body), `"suggested_transfers":[]`) {
		t.Errorf("suggested_transfers must serialize as []: %s", r.Body)
	}
}

// member paid; owner's items came to 1380, third's to 115, member's own to 805.
// The payer is owed 1380 + 115 = 1495. Two payments settle everything.
func TestBalances_HandCheckedDinner(t *testing.T) {
	e := newEnv(t, "owner", "member", "third")
	e.pay(t, "member", map[string]int64{"owner": 1380, "third": 115, "member": 805})

	v, _ := e.balances(t, "third") // any member can read it
	got := e.byName(v)
	if got["member"] != 1495 || got["owner"] != -1380 || got["third"] != -115 {
		t.Fatalf("balances = %v, want member +1495, owner -1380, third -115", got)
	}
	if len(v.Transfers) != 2 {
		t.Fatalf("transfers = %+v, want 2", v.Transfers)
	}
	// Biggest debtor pays first; both pay the one creditor.
	if t0 := v.Transfers[0]; t0.From != e.id["owner"] || t0.To != e.id["member"] || t0.Amount != 1380 {
		t.Errorf("transfer 0 = %+v, want owner -> member 1380", t0)
	}
	if t1 := v.Transfers[1]; t1.From != e.id["third"] || t1.To != e.id["member"] || t1.Amount != 115 {
		t.Errorf("transfer 1 = %+v, want third -> member 115", t1)
	}
}

// Expenses between the same people cancel out; an unfinalized one counts for nothing.
func TestBalances_NettingAndUnfinalizedExpenses(t *testing.T) {
	e := newEnv(t, "owner", "member")
	e.pay(t, "member", map[string]int64{"owner": 1000}) // owner owes member 1000
	e.pay(t, "owner", map[string]int64{"member": 1000}) // member owes owner 1000

	v, _ := e.balances(t, "owner")
	if got := e.byName(v); got["owner"] != 0 || got["member"] != 0 {
		t.Errorf("balances = %v, want both 0", got)
	}
	if len(v.Transfers) != 0 {
		t.Errorf("transfers = %+v, want none: the two debts cancel", v.Transfers)
	}

	// Claimed but not finalized: nothing is on the ledger yet.
	e.expense(t, "member", map[string]int64{"owner": 5000})
	v, _ = e.balances(t, "owner")
	if got := e.byName(v); got["owner"] != 0 || got["member"] != 0 {
		t.Errorf("an unfinalized expense changed the balances: %v", got)
	}
}

// member is owed by owner, owner is owed by third. Settling each debt separately
// takes two payments; the suggestion is the single payment third -> member.
func TestBalances_ChainCollapsesToOneTransfer(t *testing.T) {
	e := newEnv(t, "owner", "member", "third")
	e.pay(t, "member", map[string]int64{"owner": 1000})
	e.pay(t, "owner", map[string]int64{"third": 1000})

	v, _ := e.balances(t, "member")
	got := e.byName(v)
	if got["member"] != 1000 || got["owner"] != 0 || got["third"] != -1000 {
		t.Fatalf("balances = %v, want member +1000, owner 0, third -1000", got)
	}
	if len(v.Transfers) != 1 || v.Transfers[0].From != e.id["third"] || v.Transfers[0].To != e.id["member"] || v.Transfers[0].Amount != 1000 {
		t.Errorf("transfers = %+v, want exactly third -> member 1000", v.Transfers)
	}
}

func TestBalances_NewMemberAppearsAtZero(t *testing.T) {
	e := newEnv(t, "owner", "member")
	e.pay(t, "owner", map[string]int64{"member": 700})

	late := e.s.Token(t, "late")
	_, code := e.s.CreateJoinCode(t, e.tok["owner"], e.group, 0, 0)
	if r := e.s.Join(t, late, code); r.Status != http.StatusCreated {
		t.Fatalf("join = %d", r.Status)
	}
	v, _ := e.balances(t, "owner")
	if len(v.Balances) != 3 || v.Balances[2].DisplayName != "late" || v.Balances[2].BalanceMinor != 0 {
		t.Errorf("balances = %+v, want the newcomer last at 0", v.Balances)
	}
}

func TestBalances_Authorization(t *testing.T) {
	e := newEnv(t, "owner", "member")
	outsider := e.s.Token(t, "outsider")

	if r := e.s.Do(t, "", "GET", "/v1/groups/"+e.group+"/balances", ""); r.Status != 401 {
		t.Errorf("no token = %d, want 401", r.Status)
	}
	ghost := e.s.Do(t, e.tok["member"], "GET", "/v1/groups/"+uuid.NewString()+"/balances", "")
	for name, r := range map[string]teststack.Response{
		"non-member":   e.s.Do(t, outsider, "GET", "/v1/groups/"+e.group+"/balances", ""),
		"malformed id": e.s.Do(t, e.tok["member"], "GET", "/v1/groups/nope/balances", ""),
	} {
		if r.Status != 404 || string(r.Body) != string(ghost.Body) {
			t.Errorf("%s: %d %s, want the same 404 as a nonexistent group: %s", name, r.Status, r.Body, ghost.Body)
		}
	}
}

// Whatever is being finalized at the moment of the read, a response is one
// snapshot: balances sum to zero and the suggested transfers settle them all.
// Several expenses are finalized at once while readers poll continuously.
func TestBalances_EveryResponseIsAConsistentSnapshot(t *testing.T) {
	e := newEnv(t, "owner", "member", "third")
	payers := []string{"owner", "member", "third"}

	type pending struct{ payer, expense string }
	var todo []pending
	const expenses = 60
	for i := range expenses {
		payer := payers[i%3]
		cost := map[string]int64{}
		for j, n := range payers {
			if n != payer {
				cost[n] = int64(100 + 37*i + 11*j)
			}
		}
		exp, _ := e.expense(t, payer, cost)
		todo = append(todo, pending{payer, exp})
	}

	var wg sync.WaitGroup
	var done atomic.Bool
	var reads atomic.Int64
	check := func(v view) {
		var sum int64
		start := map[uuid.UUID]int64{}
		for _, b := range v.Balances {
			sum += b.BalanceMinor
			start[b.MemberID] = b.BalanceMinor
		}
		if sum != 0 {
			t.Errorf("a response's balances sum to %d: %+v", sum, v.Balances)
			return
		}
		for _, tr := range v.Transfers {
			start[tr.From] += tr.Amount
			start[tr.To] -= tr.Amount
		}
		for m, left := range start {
			if left != 0 {
				t.Errorf("after the suggested transfers %s is at %d: %+v", m, left, v)
			}
		}
	}
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				v, r := e.balances(t, "member")
				if r.Status != 200 {
					t.Errorf("read = %d: %s", r.Status, r.Body)
					return
				}
				check(v)
				reads.Add(1)
			}
		}()
	}
	var fw sync.WaitGroup
	for _, p := range todo {
		fw.Add(1)
		go func() {
			defer fw.Done()
			if r := e.finalize(t, p.payer, p.expense); r.Status != 200 {
				t.Errorf("finalize = %d: %s", r.Status, r.Body)
			}
		}()
	}
	fw.Wait()
	done.Store(true)
	wg.Wait()

	// And at the end the API agrees with a direct SUM over the ledger.
	v, _ := e.balances(t, "owner")
	check(v)
	for _, b := range v.Balances {
		want := e.s.QueryInt(t, `SELECT coalesce(sum(amount_minor), 0) FROM ledger_entries WHERE group_id = $1 AND member_id = $2`, e.group, b.MemberID)
		if b.BalanceMinor != int64(want) {
			t.Errorf("%s: API says %d, SUM over the ledger says %d", b.DisplayName, b.BalanceMinor, want)
		}
	}
	if reads.Load() == 0 {
		t.Fatal("no reads happened during the finalizes")
	}
	t.Logf("%d balance reads raced against %d finalizes; every one was consistent", reads.Load(), expenses)
}

// The query authorizes in its own statement (a join on the caller's membership),
// separately from the handler's 404. The handler check hides that, so test the
// query directly: a user outside the group gets no rows. (Per-group scoping for a
// user who is in two groups is tested in test/bola.)
func TestGetGroupBalances_AuthorizesAndScopesInTheStatement(t *testing.T) {
	e := newEnv(t, "owner", "member")
	e.pay(t, "owner", map[string]int64{"member": 900})
	outsider := e.s.UserID(t, e.s.Token(t, "outsider"))
	q := db.New(e.s.Pool)
	ctx := context.Background()
	group := uuid.MustParse(e.group)

	rows, err := q.GetGroupBalances(ctx, db.GetGroupBalancesParams{CallerID: outsider, GroupID: group})
	if err != nil || len(rows) != 0 {
		t.Errorf("non-member: rows %v err %v, want none", rows, err)
	}
	rows, err = q.GetGroupBalances(ctx, db.GetGroupBalancesParams{CallerID: e.id["member"], GroupID: group})
	if err != nil || len(rows) != 2 {
		t.Fatalf("member: rows %v err %v, want 2", rows, err)
	}

}
