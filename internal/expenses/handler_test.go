package expenses_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/teststack"
)

const goodBody = `{"description":"dinner","items":[{"description":"pizza","price_minor":1500},{"description":"beer","price_minor":800}],"tax_minor":300,"tip_minor":200}`

// world is one group with an owner, a plain member, and an outsider with their own group.
type world struct {
	s                       *teststack.Stack
	owner, member, outsider string // tokens
	ownerID, memberID       uuid.UUID
	group, otherGroup       string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	s := teststack.New(t)
	w := &world{s: s, owner: s.Token(t, "owner"), member: s.Token(t, "member"), outsider: s.Token(t, "outsider")}
	w.group = s.CreateGroup(t, w.owner, "G")
	w.otherGroup = s.CreateGroup(t, w.outsider, "Other")
	_, code := s.CreateJoinCode(t, w.owner, w.group, 0, 0)
	if r := s.Join(t, w.member, code); r.Status != http.StatusCreated {
		t.Fatalf("join = %d: %s", r.Status, r.Body)
	}
	w.ownerID, w.memberID = s.UserID(t, w.owner), s.UserID(t, w.member)
	return w
}

func (w *world) post(t *testing.T, token, group, body, key string) teststack.Response {
	t.Helper()
	return w.s.Do(t, token, "POST", "/v1/groups/"+group+"/expenses", body, "Idempotency-Key", key)
}

func (w *world) count(t *testing.T, table, group string) int {
	t.Helper()
	switch table {
	case "expenses":
		return w.s.QueryInt(t, `SELECT count(*) FROM expenses WHERE group_id = $1`, group)
	case "items":
		return w.s.QueryInt(t, `SELECT count(*) FROM expense_items i JOIN expenses e ON e.id = i.expense_id WHERE e.group_id = $1`, group)
	}
	t.Fatalf("unknown table %q", table)
	return 0
}

type expense struct {
	ID            string     `json:"id"`
	GroupID       string     `json:"group_id"`
	PayerID       uuid.UUID  `json:"payer_id"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	Version       int        `json:"version"`
	Currency      string     `json:"currency"`
	TaxMinor      int64      `json:"tax_minor"`
	TipMinor      int64      `json:"tip_minor"`
	SubtotalMinor int64      `json:"subtotal_minor"`
	TotalMinor    int64      `json:"total_minor"`
	FinalizedAt   *time.Time `json:"finalized_at"`
	VoidedAt      *time.Time `json:"voided_at"`
	Items         []struct {
		ID          string      `json:"id"`
		Position    int         `json:"position"`
		Description string      `json:"description"`
		PriceMinor  int64       `json:"price_minor"`
		ClaimedBy   []uuid.UUID `json:"claimed_by"`
	} `json:"items"`
}

func TestCreate_PlainMemberCreatesAsPayer(t *testing.T) {
	w := newWorld(t)
	r := w.post(t, w.member, w.group, goodBody, uuid.NewString())
	if r.Status != http.StatusCreated {
		t.Fatalf("create = %d: %s", r.Status, r.Body)
	}
	var e expense
	r.JSON(t, &e)

	if e.PayerID != w.memberID {
		t.Errorf("payer = %s, want the creator %s", e.PayerID, w.memberID)
	}
	if e.Status != "claiming" || e.Version != 1 || e.Currency != "CAD" || e.GroupID != w.group {
		t.Errorf("unexpected header fields: %+v", e)
	}
	if e.SubtotalMinor != 2300 || e.TaxMinor != 300 || e.TipMinor != 200 || e.TotalMinor != 2800 {
		t.Errorf("amounts = subtotal %d tax %d tip %d total %d, want 2300/300/200/2800", e.SubtotalMinor, e.TaxMinor, e.TipMinor, e.TotalMinor)
	}
	if e.FinalizedAt != nil || e.VoidedAt != nil {
		t.Errorf("new expense has finalized_at/voided_at set")
	}
	if len(e.Items) != 2 || e.Items[0].Description != "pizza" || e.Items[0].Position != 0 || e.Items[1].Description != "beer" || e.Items[1].Position != 1 {
		t.Errorf("items = %+v, want pizza@0 then beer@1", e.Items)
	}
	// claimed_by is an empty array, never null, so clients can iterate it.
	if !strings.Contains(string(r.Body), `"claimed_by":[]`) || strings.Contains(string(r.Body), `"claimed_by":null`) {
		t.Errorf("claimed_by must serialize as []: %s", r.Body)
	}
	if w.count(t, "expenses", w.group) != 1 || w.count(t, "items", w.group) != 2 {
		t.Errorf("rows = %d expenses, %d items; want 1 and 2", w.count(t, "expenses", w.group), w.count(t, "items", w.group))
	}
}

func TestCreate_Validation(t *testing.T) {
	w := newWorld(t)
	items := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `{"description":"x","price_minor":1}`
		}
		return `[` + strings.Join(parts, ",") + `]`
	}
	tests := []struct {
		name, body string
		want       int
	}{
		{"valid, minimal", `{"description":"d","items":[{"description":"i","price_minor":0}]}`, 201},
		{"valid, max amounts", `{"description":"d","items":[{"description":"i","price_minor":100000000}],"tax_minor":100000000,"tip_minor":100000000}`, 201},
		{"valid, 100 items", `{"description":"d","items":` + items(100) + `}`, 201},
		{"valid, trims text", `{"description":"  d  ","items":[{"description":" i ","price_minor":5}]}`, 201},

		{"empty body", ``, 400},
		{"not json", `nope`, 400},
		{"trailing data", goodBody + `{}`, 400},
		{"unknown field", `{"description":"d","items":[{"description":"i","price_minor":1}],"total_minor":1}`, 400},
		{"unknown item field", `{"description":"d","items":[{"description":"i","price_minor":1,"qty":3}]}`, 400},
		{"missing description", `{"items":[{"description":"i","price_minor":1}]}`, 400},
		{"blank description", `{"description":"   ","items":[{"description":"i","price_minor":1}]}`, 400},
		{"description too long", `{"description":"` + strings.Repeat("a", 201) + `","items":[{"description":"i","price_minor":1}]}`, 400},
		{"no items key", `{"description":"d"}`, 400},
		{"empty items", `{"description":"d","items":[]}`, 400},
		{"101 items", `{"description":"d","items":` + items(101) + `}`, 400},
		{"blank item description", `{"description":"d","items":[{"description":" ","price_minor":1}]}`, 400},
		{"missing price", `{"description":"d","items":[{"description":"i"}]}`, 400},
		{"null price", `{"description":"d","items":[{"description":"i","price_minor":null}]}`, 400},
		{"fractional price", `{"description":"d","items":[{"description":"i","price_minor":12.5}]}`, 400},
		{"float-looking price", `{"description":"d","items":[{"description":"i","price_minor":1e3}]}`, 400},
		{"string price", `{"description":"d","items":[{"description":"i","price_minor":"12"}]}`, 400},
		{"negative price", `{"description":"d","items":[{"description":"i","price_minor":-1}]}`, 400},
		{"price over cap", `{"description":"d","items":[{"description":"i","price_minor":100000001}]}`, 400},
		{"price overflows int64", `{"description":"d","items":[{"description":"i","price_minor":9223372036854775808}]}`, 400},
		{"negative tax", `{"description":"d","items":[{"description":"i","price_minor":1}],"tax_minor":-1}`, 400},
		{"tax over cap", `{"description":"d","items":[{"description":"i","price_minor":1}],"tax_minor":100000001}`, 400},
		{"negative tip", `{"description":"d","items":[{"description":"i","price_minor":1}],"tip_minor":-1}`, 400},
		{"tax with only free items", `{"description":"d","items":[{"description":"i","price_minor":0}],"tax_minor":5}`, 400},
		{"tip with only free items", `{"description":"d","items":[{"description":"i","price_minor":0}],"tip_minor":5}`, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			beforeExp := w.count(t, "expenses", w.group) // this group only: the database is shared with other packages' tests
			key := uuid.NewString()
			r := w.post(t, w.member, w.group, tc.body, key)
			if r.Status != tc.want {
				t.Fatalf("status = %d, want %d: %s", r.Status, tc.want, r.Body)
			}
			if tc.want == 400 {
				if w.count(t, "expenses", w.group) != beforeExp {
					t.Error("a rejected request created an expense")
				}
				// Validation happens before the key is claimed, so the key is not burned.
				if n := w.s.QueryInt(t, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key); n != 0 {
					t.Errorf("a validation failure stored its idempotency key (%d rows)", n)
				}
			}
		})
	}

	t.Run("missing Idempotency-Key", func(t *testing.T) {
		r := w.s.Do(t, w.member, "POST", "/v1/groups/"+w.group+"/expenses", goodBody)
		if r.Status != 400 {
			t.Fatalf("status = %d, want 400: %s", r.Status, r.Body)
		}
	})
	t.Run("malformed Idempotency-Key", func(t *testing.T) {
		if r := w.post(t, w.member, w.group, goodBody, "has space"); r.Status != 400 {
			t.Fatalf("status = %d, want 400: %s", r.Status, r.Body)
		}
	})
}

func TestCreate_NonMemberGets404BeforeAnythingElse(t *testing.T) {
	w := newWorld(t)
	before := w.count(t, "expenses", w.group)

	// A good body, a bad body and no key all give the same 404: a non-member
	// cannot even learn whether their request was well formed.
	for name, tc := range map[string]struct{ body, key string }{
		"valid":      {goodBody, uuid.NewString()},
		"invalid":    {`not json`, uuid.NewString()},
		"missingkey": {goodBody, ""},
	} {
		var r teststack.Response
		if tc.key == "" {
			r = w.s.Do(t, w.outsider, "POST", "/v1/groups/"+w.group+"/expenses", tc.body)
		} else {
			r = w.post(t, w.outsider, w.group, tc.body, tc.key)
		}
		if r.Status != 404 {
			t.Errorf("%s: status = %d, want 404: %s", name, r.Status, r.Body)
		}
	}
	if w.count(t, "expenses", w.group) != before {
		t.Error("a non-member created an expense")
	}
	if r := w.post(t, w.member, "not-a-uuid", goodBody, uuid.NewString()); r.Status != 404 {
		t.Errorf("malformed group id = %d, want 404", r.Status)
	}
}

func TestCreate_Idempotency(t *testing.T) {
	w := newWorld(t)
	key := uuid.NewString()

	first := w.post(t, w.member, w.group, goodBody, key)
	if first.Status != 201 {
		t.Fatalf("first = %d: %s", first.Status, first.Body)
	}
	if first.Header.Get("Idempotent-Replayed") != "" {
		t.Error("the original response is marked as a replay")
	}

	// Same key, same request: the stored response, byte for byte, no new rows.
	again := w.post(t, w.member, w.group, goodBody, key)
	if again.Status != 201 || !bytes.Equal(again.Body, first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d replayed=%q, body equal=%v", again.Status, again.Header.Get("Idempotent-Replayed"), bytes.Equal(again.Body, first.Body))
	}

	// Same meaning, different bytes (whitespace, key order, explicit zero handled below): still a replay.
	reordered := `{ "tip_minor": 200, "tax_minor": 300, "items": [ {"price_minor": 1500, "description": " pizza "}, {"price_minor": 800, "description": "beer"} ], "description": "dinner" }`
	if r := w.post(t, w.member, w.group, reordered, key); r.Status != 201 || !bytes.Equal(r.Body, first.Body) {
		t.Errorf("equivalent request with the same key = %d (want a replay of the original): %s", r.Status, r.Body)
	}

	// Same key, different request: refused, and nothing is created.
	different := strings.Replace(goodBody, `"price_minor":1500`, `"price_minor":1501`, 1)
	if r := w.post(t, w.member, w.group, different, key); r.Status != 422 {
		t.Errorf("same key, different body = %d, want 422: %s", r.Status, r.Body)
	}
	if n := w.count(t, "expenses", w.group); n != 1 {
		t.Fatalf("expenses after replays = %d, want exactly 1", n)
	}

	// Same key from a different user is an unrelated key (keys are scoped per user).
	if r := w.post(t, w.owner, w.group, goodBody, key); r.Status != 201 || r.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("same key, other user = %d replayed=%q, want a fresh 201", r.Status, r.Header.Get("Idempotent-Replayed"))
	}

	// A different key is a different request: two identical dinners are legitimate.
	if r := w.post(t, w.member, w.group, goodBody, uuid.NewString()); r.Status != 201 {
		t.Fatalf("new key = %d", r.Status)
	}
	if n := w.count(t, "expenses", w.group); n != 3 {
		t.Errorf("expenses = %d, want 3 (member x2 distinct keys, owner x1)", n)
	}
}

func TestCreate_ConcurrentDuplicatesCreateOnce(t *testing.T) {
	w := newWorld(t)
	key := uuid.NewString()

	const n = 10
	resps := make([]teststack.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i] = w.post(t, w.member, w.group, goodBody, key)
		}()
	}
	wg.Wait()

	replays := 0
	for i, r := range resps {
		if r.Status != 201 {
			t.Fatalf("request %d = %d: %s", i, r.Status, r.Body)
		}
		if !bytes.Equal(r.Body, resps[0].Body) {
			t.Errorf("request %d body differs from request 0", i)
		}
		if r.Header.Get("Idempotent-Replayed") == "true" {
			replays++
		}
	}
	if replays != n-1 {
		t.Errorf("replays = %d, want %d (exactly one original)", replays, n-1)
	}
	if got := w.count(t, "expenses", w.group); got != 1 {
		t.Fatalf("expenses = %d after %d concurrent duplicates, want 1", got, n)
	}
	if got := w.count(t, "items", w.group); got != 2 {
		t.Fatalf("items = %d, want 2", got)
	}
}

func TestGet_ShapeAndClaimedBy(t *testing.T) {
	w := newWorld(t)
	id := w.s.CreateExpense(t, w.member, w.group, goodBody)

	var e expense
	r := w.s.Do(t, w.owner, "GET", "/v1/groups/"+w.group+"/expenses/"+id, "") // owner reads a member's expense
	if r.Status != 200 {
		t.Fatalf("get = %d: %s", r.Status, r.Body)
	}
	r.JSON(t, &e)
	if e.ID != id || e.PayerID != w.memberID || e.TotalMinor != 2800 || len(e.Items) != 2 {
		t.Fatalf("unexpected expense: %+v", e)
	}
	for _, it := range e.Items {
		if it.ClaimedBy == nil || len(it.ClaimedBy) != 0 {
			t.Errorf("item %d claimed_by = %v, want []", it.Position, it.ClaimedBy)
		}
	}

	// Claims do not have an endpoint yet (M2.4): arrange them directly.
	for _, m := range []uuid.UUID{w.memberID, w.ownerID} {
		w.s.Exec(t, `INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`, e.Items[0].ID, id, w.group, m)
	}
	r = w.s.Do(t, w.member, "GET", "/v1/groups/"+w.group+"/expenses/"+id, "")
	var after expense
	r.JSON(t, &after)
	if len(after.Items[0].ClaimedBy) != 2 || len(after.Items[1].ClaimedBy) != 0 {
		t.Fatalf("claimed_by = %v / %v, want 2 claimants on item 0 and none on item 1", after.Items[0].ClaimedBy, after.Items[1].ClaimedBy)
	}
	a, b := after.Items[0].ClaimedBy[0].String(), after.Items[0].ClaimedBy[1].String()
	if a >= b {
		t.Errorf("claimed_by not sorted: %s, %s", a, b)
	}
}

func TestGet_NotFoundVariantsAreIdentical(t *testing.T) {
	w := newWorld(t)
	id := w.s.CreateExpense(t, w.member, w.group, goodBody)
	otherID := w.s.CreateExpense(t, w.outsider, w.otherGroup, goodBody)

	get := func(token, path string) teststack.Response { return w.s.Do(t, token, "GET", path, "") }
	want := get(w.member, "/v1/groups/"+w.group+"/expenses/"+uuid.NewString())
	if want.Status != 404 {
		t.Fatalf("ghost expense = %d", want.Status)
	}
	for name, r := range map[string]teststack.Response{
		"malformed expense id":      get(w.member, "/v1/groups/"+w.group+"/expenses/nope"),
		"malformed group id":        get(w.member, "/v1/groups/nope/expenses/"+id),
		"expense of another group":  get(w.member, "/v1/groups/"+w.group+"/expenses/"+otherID),
		"non-member, real ids":      get(w.outsider, "/v1/groups/"+w.group+"/expenses/"+id),
		"real expense, wrong group": get(w.outsider, "/v1/groups/"+w.otherGroup+"/expenses/"+id),
	} {
		if r.Status != 404 || !bytes.Equal(r.Body, want.Body) {
			t.Errorf("%s: %d %s, want the same 404 as a nonexistent id: %s", name, r.Status, r.Body, want.Body)
		}
	}
}

func TestList(t *testing.T) {
	w := newWorld(t)
	type summary struct {
		ID            string `json:"id"`
		Description   string `json:"description"`
		ItemCount     int    `json:"item_count"`
		SubtotalMinor int64  `json:"subtotal_minor"`
		TotalMinor    int64  `json:"total_minor"`
		Status        string `json:"status"`
	}
	list := func(token, group string) (int, []summary, string) {
		r := w.s.Do(t, token, "GET", "/v1/groups/"+group+"/expenses", "")
		var out struct {
			Expenses []summary `json:"expenses"`
		}
		if r.Status == 200 {
			r.JSON(t, &out)
		}
		return r.Status, out.Expenses, string(r.Body)
	}

	// A member of a group with no expenses gets 200 and an empty array (not null,
	// not 404); a non-member gets 404.
	status, got, body := list(w.member, w.group)
	if status != 200 || len(got) != 0 || !strings.Contains(body, `"expenses":[]`) {
		t.Fatalf("empty list = %d %s", status, body)
	}
	if status, _, _ := list(w.outsider, w.group); status != 404 {
		t.Fatalf("non-member list = %d, want 404", status)
	}

	first := w.s.CreateExpense(t, w.member, w.group, goodBody)
	time.Sleep(5 * time.Millisecond) // created_at is the transaction start; keep the order unambiguous
	second := w.s.CreateExpense(t, w.owner, w.group, `{"description":"lunch","items":[{"description":"soup","price_minor":700}],"tip_minor":100}`)
	w.s.CreateExpense(t, w.outsider, w.otherGroup, goodBody)

	_, got, _ = list(w.owner, w.group)
	if len(got) != 2 || got[0].ID != second || got[1].ID != first {
		t.Fatalf("list = %+v, want [second, first] and nothing from the other group", got)
	}
	if got[0].ItemCount != 1 || got[0].SubtotalMinor != 700 || got[0].TotalMinor != 800 || got[0].Status != "claiming" {
		t.Errorf("summary 0 = %+v", got[0])
	}
	if got[1].ItemCount != 2 || got[1].SubtotalMinor != 2300 || got[1].TotalMinor != 2800 {
		t.Errorf("summary 1 = %+v", got[1])
	}
}

// Every request needs authentication.
func TestUnauthenticated(t *testing.T) {
	w := newWorld(t)
	id := w.s.CreateExpense(t, w.member, w.group, goodBody)
	for _, req := range []struct{ method, path, body string }{
		{"POST", "/v1/groups/" + w.group + "/expenses", goodBody},
		{"GET", "/v1/groups/" + w.group + "/expenses", ""},
		{"GET", "/v1/groups/" + w.group + "/expenses/" + id, ""},
	} {
		if r := w.s.Do(t, "", req.method, req.path, req.body, "Idempotency-Key", uuid.NewString()); r.Status != 401 {
			t.Errorf("%s %s without a token = %d, want 401", req.method, fmt.Sprint(req.path), r.Status)
		}
	}
}

// joinSecondGroup makes token's user a member of a brand-new second group (owned
// by the outsider) and returns that group's id. Membership alone no longer
// separates the two groups for this user, so only the group scoping in the
// queries and the idempotency fingerprint can.
func (w *world) joinSecondGroup(t *testing.T, token string) string {
	t.Helper()
	g2 := w.s.CreateGroup(t, w.outsider, "Second")
	_, code := w.s.CreateJoinCode(t, w.outsider, g2, 0, 0)
	if r := w.s.Join(t, token, code); r.Status != http.StatusCreated {
		t.Fatalf("join second group = %d: %s", r.Status, r.Body)
	}
	return g2
}

// The case that plain "are you a member" checks get wrong: one user in two
// groups must still see each group's expenses only through that group's path.
func TestUserInTwoGroupsCannotCrossThem(t *testing.T) {
	w := newWorld(t)
	g1, g2 := w.group, w.joinSecondGroup(t, w.member)
	e1 := w.s.CreateExpense(t, w.member, g1, goodBody)
	e2 := w.s.CreateExpense(t, w.member, g2, goodBody)

	ids := func(group string) []string {
		r := w.s.Do(t, w.member, "GET", "/v1/groups/"+group+"/expenses", "")
		var out struct {
			Expenses []struct {
				ID string `json:"id"`
			} `json:"expenses"`
		}
		r.JSON(t, &out)
		var got []string
		for _, e := range out.Expenses {
			got = append(got, e.ID)
		}
		return got
	}
	if got := ids(g1); len(got) != 1 || got[0] != e1 {
		t.Errorf("group 1 list = %v, want only %s", got, e1)
	}
	if got := ids(g2); len(got) != 1 || got[0] != e2 {
		t.Errorf("group 2 list = %v, want only %s", got, e2)
	}

	for _, tc := range []struct{ name, group, expense string }{
		{"group 2's expense through group 1's path", g1, e2},
		{"group 1's expense through group 2's path", g2, e1},
	} {
		if r := w.s.Do(t, w.member, "GET", "/v1/groups/"+tc.group+"/expenses/"+tc.expense, ""); r.Status != 404 {
			t.Errorf("%s = %d, want 404 even though the caller is a member of both groups: %s", tc.name, r.Status, r.Body)
		}
	}
	if r := w.s.Do(t, w.member, "GET", "/v1/groups/"+g1+"/expenses/"+e1, ""); r.Status != 200 {
		t.Errorf("control: own path = %d", r.Status)
	}
}

// An idempotency key identifies one request. The same key and body sent to a
// different group is a different request, not a replay of the first.
func TestCreate_KeyReusedAcrossGroupsIsRefused(t *testing.T) {
	w := newWorld(t)
	g2 := w.joinSecondGroup(t, w.member)
	key := uuid.NewString()

	if r := w.post(t, w.member, w.group, goodBody, key); r.Status != 201 {
		t.Fatalf("first = %d: %s", r.Status, r.Body)
	}
	r := w.post(t, w.member, g2, goodBody, key)
	if r.Status != 422 {
		t.Fatalf("same key and body in another group = %d, want 422 (a replay would hand back group 1's expense): %s", r.Status, r.Body)
	}
	if n := w.count(t, "expenses", g2); n != 0 {
		t.Errorf("group 2 has %d expenses, want 0", n)
	}
}
