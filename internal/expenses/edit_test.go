package expenses_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/teststack"
)

func expenseURL(group, expense string) string { return "/v1/groups/" + group + "/expenses/" + expense }

func (w *world) patch(t testing.TB, token, group, expense, body, key string) teststack.Response {
	t.Helper()
	return w.s.Do(t, token, "PATCH", expenseURL(group, expense), body, "Idempotency-Key", key)
}

func (w *world) confirm(t testing.TB, token, group, expense, body, key string) teststack.Response {
	t.Helper()
	return w.s.Do(t, token, "POST", expenseURL(group, expense)+"/confirm", body, "Idempotency-Key", key)
}

func (w *world) get(t testing.TB, token, expenseID string) expense {
	t.Helper()
	var e expense
	r := w.s.Do(t, token, "GET", expenseURL(w.group, expenseID), "")
	if r.Status != 200 {
		t.Fatalf("get expense = %d: %s", r.Status, r.Body)
	}
	r.JSON(t, &e)
	return e
}

func (w *world) setStatus(t testing.TB, expenseID, status string) {
	t.Helper()
	w.s.Exec(t, `UPDATE expenses SET status = $2 WHERE id = $1`, expenseID, status)
}

func (w *world) editAudit(t testing.TB, expenseID, kind string) int {
	return w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = $2 AND data->>'expense_id' = $1`, expenseID, kind)
}

// snapshot is everything a refused edit must leave alone.
func (w *world) snapshot(t testing.TB, expenseID string) string {
	t.Helper()
	var s string
	err := w.s.Pool.QueryRow(context.Background(), `
		SELECT e.version::text || '/' || e.status || '/' || e.tax_minor || '/' || e.tip_minor || '/' ||
		       coalesce((SELECT string_agg(i.id || ':' || i.position || ':' || i.description || ':' || i.price_minor, ',' ORDER BY i.position) FROM expense_items i WHERE i.expense_id = e.id), '') || '/' ||
		       coalesce((SELECT string_agg(c.item_id || ':' || c.member_id, ',' ORDER BY c.item_id, c.member_id) FROM claims c WHERE c.expense_id = e.id), '') || '/' ||
		       (SELECT count(*) FROM audit_events a WHERE a.data->>'expense_id' = e.id::text)
		FROM expenses e WHERE e.id = $1`, expenseID).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- behaviour ----

func TestEdit_PriceChangeDeletesClaimsAndOthersSurvive(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody) // pizza 1500, beer 800; payer is member
	for _, it := range items {
		if r := w.put(t, w.owner, w.group, exp, it); r.Status != 200 {
			t.Fatalf("claim = %d %s", r.Status, r.Body)
		}
	}
	if r := w.put(t, w.member, w.group, exp, items[1]); r.Status != 200 { // payer shares the beer
		t.Fatal(r.Body)
	}

	r := w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1600}]}`, items[0]), uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("edit = %d: %s", r.Status, r.Body)
	}
	var got expense
	r.JSON(t, &got)
	if got.Version != 2 || got.Status != "claiming" {
		t.Errorf("version/status = %d/%s, want 2/claiming", got.Version, got.Status)
	}
	if got.Items[0].PriceMinor != 1600 || len(got.Items[0].ClaimedBy) != 0 {
		t.Errorf("edited item = %+v, want price 1600 and no claimants", got.Items[0])
	}
	if len(got.Items[1].ClaimedBy) != 2 {
		t.Errorf("the other item lost its claims: %+v", got.Items[1])
	}
	if got.SubtotalMinor != 2400 || got.TotalMinor != 2400+300+200 {
		t.Errorf("subtotal/total = %d/%d, want 2400/2900", got.SubtotalMinor, got.TotalMinor)
	}
	if n := w.editAudit(t, exp, "expense.items_edited"); n != 1 {
		t.Errorf("audit events = %d, want 1", n)
	}
	var data string
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT data::text FROM audit_events WHERE kind = 'expense.items_edited' AND data->>'expense_id' = $1`, exp).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"claims_removed": 1`) || strings.Contains(data, "pizza") {
		t.Errorf("audit data = %s, want claims_removed 1 and no item text", data)
	}
}

func TestEdit_DescriptionOnlyKeepsClaims(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])

	// Same price written again is not a price change either.
	r := w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"description":"  margherita ","price_minor":1500}]}`, items[0]), uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("edit = %d: %s", r.Status, r.Body)
	}
	var got expense
	r.JSON(t, &got)
	if got.Items[0].Description != "margherita" || len(got.Items[0].ClaimedBy) != 1 {
		t.Errorf("item = %+v, want trimmed description and the claim kept", got.Items[0])
	}
}

// An update that writes the values already there changes nothing: no claim is
// deleted and the audit event counts no updated item.
func TestEdit_UpdateToTheSameValuesIsNotAChange(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])
	if r := w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"description":"pizza","price_minor":1500}]}`, items[0]), uuid.NewString()); r.Status != 200 {
		t.Fatalf("edit = %d: %s", r.Status, r.Body)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'expense.items_edited' AND data->>'expense_id' = $1 AND (data->>'updated')::int = 0 AND (data->>'claims_removed')::int = 0`, exp); n != 1 {
		t.Errorf("audit events with updated=0 and claims_removed=0: %d, want 1", n)
	}
	if rows := w.claimRows(t, exp); len(rows) != 1 {
		t.Errorf("claims = %v", rows)
	}
}

func TestEdit_AddRemoveTaxTip(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])
	w.put(t, w.owner, w.group, exp, items[1])

	body := fmt.Sprintf(`{"version":1,"remove":[%q],"add":[{"description":"fries","price_minor":500},{"description":"soda","price_minor":250}],"tax_minor":0,"tip_minor":100}`, items[1])
	r := w.patch(t, w.member, w.group, exp, body, uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("edit = %d: %s", r.Status, r.Body)
	}
	var got expense
	r.JSON(t, &got)
	if len(got.Items) != 3 || got.Items[0].ID != items[0] || got.Items[1].Description != "fries" || got.Items[2].Description != "soda" {
		t.Fatalf("items = %+v, want pizza, fries, soda in that order", got.Items)
	}
	if got.Items[1].Position <= got.Items[0].Position || got.Items[2].Position <= got.Items[1].Position {
		t.Errorf("positions not increasing: %+v", got.Items)
	}
	if len(got.Items[0].ClaimedBy) != 1 {
		t.Errorf("untouched item lost its claim")
	}
	if got.TaxMinor != 0 || got.TipMinor != 100 || got.SubtotalMinor != 2250 {
		t.Errorf("tax/tip/subtotal = %d/%d/%d", got.TaxMinor, got.TipMinor, got.SubtotalMinor)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM claims WHERE item_id = $1`, items[1]); n != 0 {
		t.Errorf("claims on the removed item = %d", n)
	}
}

func TestEdit_TaxAndTipOnlyTouchNoClaims(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])
	before := w.claimRows(t, exp)
	if r := w.patch(t, w.member, w.group, exp, `{"version":1,"tax_minor":999}`, uuid.NewString()); r.Status != 200 {
		t.Fatalf("edit = %d: %s", r.Status, r.Body)
	}
	if after := w.claimRows(t, exp); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("claims changed by a tax edit: %v -> %v", before, after)
	}
}

func TestEdit_ClaimsDoNotMakeAnEditStale(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	// The payer read version 1; a member claims meanwhile; claims never bump the version.
	w.put(t, w.owner, w.group, exp, items[1])
	if e := w.get(t, w.member, exp); e.Version != 1 {
		t.Fatalf("a claim changed the version to %d", e.Version)
	}
	if r := w.patch(t, w.member, w.group, exp, `{"version":1,"tip_minor":1}`, uuid.NewString()); r.Status != 200 {
		t.Fatalf("edit after a claim = %d: %s", r.Status, r.Body)
	}
}

func TestEdit_FinalizeUsesTheEditedPrices(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	for _, it := range items {
		w.put(t, w.owner, w.group, exp, it)
	}
	w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":2000}]}`, items[0]), uuid.NewString())
	w.put(t, w.owner, w.group, exp, items[0]) // claims again at the new price
	if r := w.finalize(t, w.member, w.group, exp, "", uuid.NewString()); r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	w.assertLedgerMatchesClaims(t, exp)
	if got := w.ledgerEntries(t, exp)[w.ownerID]; got != -(2000 + 800 + 300 + 200) {
		t.Errorf("owner's entry = %d, want the whole edited total debited", got)
	}
}

// ---- state, version, authorization ----

func TestEdit_Statuses(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{"claiming", 200}, {"needs_review", 200}, {"parsing", 409}, {"finalized", 409}, {"void", 409},
	} {
		t.Run(tc.status, func(t *testing.T) {
			w := newWorld(t)
			exp, _ := w.newExpense(t, w.member, goodBody)
			switch tc.status {
			case "finalized", "void":
				w.s.Exec(t, `UPDATE expenses SET status = $2, finalized_at = now(), voided_at = CASE WHEN $2 = 'void' THEN now() END WHERE id = $1`, exp, tc.status)
			default:
				w.setStatus(t, exp, tc.status)
			}
			before := w.snapshot(t, exp)
			r := w.patch(t, w.member, w.group, exp, `{"version":1,"tip_minor":1}`, uuid.NewString())
			if r.Status != tc.want {
				t.Fatalf("edit = %d: %s, want %d", r.Status, r.Body, tc.want)
			}
			if tc.want == 409 {
				if !strings.Contains(string(r.Body), "invalid_state") {
					t.Errorf("body = %s", r.Body)
				}
				if w.snapshot(t, exp) != before {
					t.Error("a refused edit changed state")
				}
			}
		})
	}
}

func TestEdit_VersionConflict(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	if r := w.patch(t, w.member, w.group, exp, `{"version":1,"tip_minor":1}`, uuid.NewString()); r.Status != 200 {
		t.Fatal(r.Body)
	}
	w.put(t, w.owner, w.group, exp, items[0])
	before := w.snapshot(t, exp)

	for _, v := range []int{1, 3, 99} { // stale, and "from the future"
		r := w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":%d,"tip_minor":500}`, v), uuid.NewString())
		if r.Status != 409 || !strings.Contains(string(r.Body), "version_conflict") {
			t.Fatalf("version %d: %d %s, want 409 version_conflict", v, r.Status, r.Body)
		}
	}
	if w.snapshot(t, exp) != before {
		t.Error("a conflicting edit changed state")
	}
	if r := w.patch(t, w.member, w.group, exp, `{"version":2,"tip_minor":500}`, uuid.NewString()); r.Status != 200 {
		t.Fatalf("edit at the current version = %d: %s", r.Status, r.Body)
	}
}

func TestEdit_Authorization(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody) // payer is member; owner is the group owner
	other := w.s.CreateExpense(t, w.outsider, w.otherGroup, goodBody)
	body := fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1}]}`, items[0])
	before := w.snapshot(t, exp)

	for _, tc := range []struct {
		name, token, group, exp string
		want                    int
	}{
		{"group owner is not the payer", w.owner, w.group, exp, 403},
		{"outsider", w.outsider, w.group, exp, 404},
		{"payer through another group's path", w.member, w.otherGroup, exp, 404},
		{"another group's expense through this path", w.member, w.group, other, 404},
		{"malformed id", w.member, w.group, "nope", 404},
		{"anonymous", "", w.group, exp, 401},
	} {
		r := w.patch(t, tc.token, tc.group, tc.exp, body, uuid.NewString())
		if r.Status != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, r.Status, r.Body, tc.want)
		}
	}
	if w.snapshot(t, exp) != before {
		t.Error("a refused edit changed state")
	}
	// Same 404 bytes for a missing expense and for someone else's.
	a := w.patch(t, w.outsider, w.group, exp, body, uuid.NewString())
	b := w.patch(t, w.outsider, w.group, uuid.NewString(), body, uuid.NewString())
	if a.Status != b.Status || string(a.Body) != string(b.Body) {
		t.Errorf("404s differ: %q vs %q", a.Body, b.Body)
	}
}

// ---- validation ----

func TestEdit_Validation(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	long := strings.Repeat("x", 201)
	many := strings.Repeat(`{"description":"a","price_minor":1},`, 101)
	for name, body := range map[string]string{
		"no body":             ``,
		"no version":          `{"tip_minor":1}`,
		"version zero":        `{"version":0,"tip_minor":1}`,
		"nothing to change":   `{"version":1}`,
		"empty lists":         `{"version":1,"update":[],"remove":[],"add":[]}`,
		"unknown field":       `{"version":1,"tip_minor":1,"status":"claiming"}`,
		"float price":         fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1.5}]}`, items[0]),
		"negative price":      fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":-1}]}`, items[0]),
		"huge price":          fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":100000001}]}`, items[0]),
		"update changes none": fmt.Sprintf(`{"version":1,"update":[{"item_id":%q}]}`, items[0]),
		"update no id":        `{"version":1,"update":[{"price_minor":1}]}`,
		"update bad id":       `{"version":1,"update":[{"item_id":"x","price_minor":1}]}`,
		"empty description":   fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"description":"  "}]}`, items[0]),
		"long description":    fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"description":%q}]}`, items[0], long),
		"same item twice":     fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1},{"item_id":%q,"price_minor":2}]}`, items[0], items[0]),
		"update and remove":   fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1}],"remove":[%q]}`, items[0], items[0]),
		"remove twice":        fmt.Sprintf(`{"version":1,"remove":[%q,%q]}`, items[0], items[0]),
		"add without price":   `{"version":1,"add":[{"description":"x"}]}`,
		"add without text":    `{"version":1,"add":[{"price_minor":1}]}`,
		"add too many":        `{"version":1,"add":[` + strings.TrimSuffix(many, ",") + `]}`,
		"negative tax":        `{"version":1,"tax_minor":-1}`,
		"tax as string":       `{"version":1,"tax_minor":"1"}`,
		"trailing data":       `{"version":1,"tip_minor":1} {}`,
	} {
		before := w.snapshot(t, exp)
		key := uuid.NewString()
		r := w.patch(t, w.member, w.group, exp, body, key)
		if r.Status != 400 {
			t.Errorf("%s: %d %s, want 400", name, r.Status, r.Body)
		}
		if w.snapshot(t, exp) != before {
			t.Errorf("%s changed state", name)
		}
		// A 400 from validation does not claim the key: it can be used for a valid request.
		if ok := w.patch(t, w.member, w.group, exp, `{"version":1,"tip_minor":200}`, key); ok.Status != 200 {
			t.Fatalf("%s: key was burned by a 400: %d %s", name, ok.Status, ok.Body)
		}
		w.s.Exec(t, `UPDATE expenses SET version = 1 WHERE id = $1`, exp) // reset for the next case
	}

	if r := w.s.Do(t, w.member, "PATCH", expenseURL(w.group, exp), `{"version":1,"tip_minor":1}`); r.Status != 400 {
		t.Errorf("missing Idempotency-Key = %d, want 400", r.Status)
	}
}

func TestEdit_FinalStateRules(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])

	other, otherItems := w.newExpense(t, w.member, goodBody)
	_ = other
	for name, tc := range map[string]struct {
		body string
		want int
		code string
	}{
		"unknown item to update":      {fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1}]}`, uuid.NewString()), 404, "item_not_found"},
		"another expense's item":      {fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1}]}`, otherItems[0]), 404, "item_not_found"},
		"another expense's to remove": {fmt.Sprintf(`{"version":1,"remove":[%q]}`, otherItems[0]), 404, "item_not_found"},
		"remove every item":           {fmt.Sprintf(`{"version":1,"remove":[%q,%q]}`, items[0], items[1]), 400, "invalid_request"},
		"all free with tax left":      {fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":0},{"item_id":%q,"price_minor":0}]}`, items[0], items[1]), 400, "invalid_request"},
		"over 100 items":              {`{"version":1,"add":[` + strings.TrimSuffix(strings.Repeat(`{"description":"a","price_minor":1},`, 99), ",") + `]}`, 400, "invalid_request"},
	} {
		before := w.snapshot(t, exp)
		r := w.patch(t, w.member, w.group, exp, tc.body, uuid.NewString())
		if r.Status != tc.want || !strings.Contains(string(r.Body), tc.code) {
			t.Errorf("%s: %d %s, want %d %s", name, r.Status, r.Body, tc.want, tc.code)
		}
		if w.snapshot(t, exp) != before {
			t.Errorf("%s: a refused edit changed state (or its claims)", name)
		}
	}
	// Making everything free is fine once tax and tip go to zero in the same edit.
	body := fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":0},{"item_id":%q,"price_minor":0}],"tax_minor":0,"tip_minor":0}`, items[0], items[1])
	if r := w.patch(t, w.member, w.group, exp, body, uuid.NewString()); r.Status != 200 {
		t.Errorf("all free with zero tax and tip = %d: %s", r.Status, r.Body)
	}
}

func TestEdit_NeedsReviewMayBeADraft(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.setStatus(t, exp, "needs_review")
	// A draft may be emptied and refilled; the checks come at confirm.
	// Tax and tip go too, so the missing items are the only thing confirm can object to.
	if r := w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"remove":[%q,%q],"tax_minor":0,"tip_minor":0}`, items[0], items[1]), uuid.NewString()); r.Status != 200 {
		t.Fatalf("emptying a draft = %d: %s", r.Status, r.Body)
	}
	if r := w.confirm(t, w.member, w.group, exp, `{"version":2}`, uuid.NewString()); r.Status != 400 {
		t.Errorf("confirm with no items = %d: %s, want 400", r.Status, r.Body)
	}
	if r := w.patch(t, w.member, w.group, exp, `{"version":2,"add":[{"description":"soup","price_minor":900}]}`, uuid.NewString()); r.Status != 200 {
		t.Fatalf("refilling = %d: %s", r.Status, r.Body)
	}
	if r := w.confirm(t, w.member, w.group, exp, `{"version":3}`, uuid.NewString()); r.Status != 200 {
		t.Fatalf("confirm = %d: %s", r.Status, r.Body)
	}
}

// ---- idempotency ----

func TestEdit_Idempotency(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	key := uuid.NewString()
	body := fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1700}]}`, items[0])

	first := w.patch(t, w.member, w.group, exp, body, key)
	if first.Status != 200 {
		t.Fatal(first.Body)
	}
	// A retry after a lost response: same answer, not a version conflict, no second bump.
	again := w.patch(t, w.member, w.group, exp, body, key)
	if again.Status != 200 || string(again.Body) != string(first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d replayed=%q same=%v", again.Status, again.Header.Get("Idempotent-Replayed"), string(again.Body) == string(first.Body))
	}
	if e := w.get(t, w.member, exp); e.Version != 2 {
		t.Errorf("version = %d after a replay, want 2", e.Version)
	}
	if n := w.editAudit(t, exp, "expense.items_edited"); n != 1 {
		t.Errorf("audit events = %d", n)
	}
	// Same key, different edit.
	if r := w.patch(t, w.member, w.group, exp, `{"version":1,"tip_minor":5}`, key); r.Status != 422 {
		t.Errorf("key reuse = %d, want 422", r.Status)
	}
	// Same key, a request that differs only in the version, or only in what is added,
	// is a different request.
	if r := w.patch(t, w.member, w.group, exp, strings.Replace(body, `"version":1`, `"version":2`, 1), key); r.Status != 422 {
		t.Errorf("key reuse with another version = %d, want 422", r.Status)
	}
	k3 := uuid.NewString()
	if r := w.patch(t, w.member, w.group, exp, `{"version":2,"add":[{"description":"a","price_minor":1}]}`, k3); r.Status != 200 {
		t.Fatal(r.Body)
	}
	if r := w.patch(t, w.member, w.group, exp, `{"version":2,"add":[{"description":"b","price_minor":1}]}`, k3); r.Status != 422 {
		t.Errorf("key reuse with another added item = %d, want 422", r.Status)
	}
	// Equivalent bodies (field order, whitespace in text) are the same request.
	k2 := uuid.NewString()
	a := w.patch(t, w.member, w.group, exp, `{"version":3,"add":[{"description":" x ","price_minor":1}],"tip_minor":9}`, k2)
	b := w.patch(t, w.member, w.group, exp, `{"tip_minor":9,"version":3,"add":[{"price_minor":1,"description":"x"}]}`, k2)
	if a.Status != 200 || b.Status != 200 || b.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("equivalent bodies: %d / %d", a.Status, b.Status)
	}
}

// A refusal inside the transaction is stored under the key (Stripe model), like finalize.
func TestEdit_ConflictIsStoredUnderTheKey(t *testing.T) {
	w := newWorld(t)
	exp, _ := w.newExpense(t, w.member, goodBody)
	key := uuid.NewString()
	if r := w.patch(t, w.member, w.group, exp, `{"version":7,"tip_minor":1}`, key); r.Status != 409 {
		t.Fatal(r.Status)
	}
	// Even once the version matches, the key's first outcome is final.
	w.s.Exec(t, `UPDATE expenses SET version = 7 WHERE id = $1`, exp)
	r := w.patch(t, w.member, w.group, exp, `{"version":7,"tip_minor":1}`, key)
	if r.Status != 409 || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("= %d replayed=%q, want the stored 409", r.Status, r.Header.Get("Idempotent-Replayed"))
	}
}

// ---- concurrency ----

// Two edits from the same version, released together: exactly one wins, the other
// gets 409 and changes nothing. This is what the version check is for.
func TestEdit_TwoEditsFromTheSameVersion(t *testing.T) {
	for round := 0; round < 15; round++ {
		w := newWorld(t)
		exp, items := w.newExpense(t, w.member, goodBody)
		var wg sync.WaitGroup
		start := make(chan struct{})
		res := make([]teststack.Response, 2)
		for i := range res {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				body := fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":%d}]}`, items[0], 1000+i)
				res[i] = w.patch(t, w.member, w.group, exp, body, uuid.NewString())
			}()
		}
		close(start)
		wg.Wait()
		ok, conflict := 0, 0
		for _, r := range res {
			switch r.Status {
			case 200:
				ok++
			case 409:
				conflict++
			default:
				t.Fatalf("round %d: unexpected %d %s", round, r.Status, r.Body)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: %d ok, %d conflicts, want 1 and 1", round, ok, conflict)
		}
		if e := w.get(t, w.member, exp); e.Version != 2 {
			t.Fatalf("round %d: version = %d, want 2", round, e.Version)
		}
	}
}

// The edit takes FOR NO KEY UPDATE, so it waits for a claim that is in flight (FOR
// SHARE). A claim that has landed by then is deleted if its item's price changes.
func TestEdit_WaitsForAnInFlightClaimThenDeletesIt(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)

	claimer := w.begin(t)
	lock(t, claimer, "FOR SHARE", exp)
	if _, err := claimer.Exec(context.Background(),
		`INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`, items[0], exp, w.group, w.ownerID); err != nil {
		t.Fatal(err)
	}

	done := make(chan teststack.Response, 1)
	go func() {
		done <- w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1600}]}`, items[0]), uuid.NewString())
	}()
	select {
	case r := <-done:
		t.Fatalf("the edit completed (%d %s) while a claim was in flight; it must wait", r.Status, r.Body)
	case <-time.After(300 * time.Millisecond):
	}
	if err := claimer.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Status != 200 {
			t.Fatalf("edit = %d %s", r.Status, r.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the edit never returned")
	}
	if rows := w.claimRows(t, exp); len(rows) != 0 {
		t.Fatalf("a claim made at the old price survived the edit: %v", rows)
	}
}

// And the other direction: a claim arriving while an edit holds the row waits, then
// lands on the edited item (it saw the new price, so it may stay).
func TestEdit_ClaimArrivingDuringAnEditWaitsAndSurvives(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)

	editor := w.begin(t)
	lock(t, editor, "FOR NO KEY UPDATE", exp)
	done := make(chan teststack.Response, 1)
	go func() { done <- w.put(t, w.owner, w.group, exp, items[0]) }()
	select {
	case r := <-done:
		t.Fatalf("a claim completed (%d) while an edit held the row", r.Status)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := editor.Exec(context.Background(), `UPDATE expense_items SET price_minor = 1 WHERE id = $1`, items[0]); err != nil {
		t.Fatal(err)
	}
	if err := editor.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != 200 {
		t.Fatalf("claim = %d", r.Status)
	}
	if rows := w.claimRows(t, exp); len(rows) != 1 {
		t.Fatalf("claims = %v", rows)
	}
}

// An edit queued behind a finalize sees `finalized` and is refused; nothing changes
// on a finalized expense.
func TestEdit_WaitsForFinalizeThenIsRefused(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.put(t, w.owner, w.group, exp, items[0])
	w.put(t, w.owner, w.group, exp, items[1])

	finalizer := w.begin(t)
	lock(t, finalizer, "FOR UPDATE", exp)
	done := make(chan teststack.Response, 1)
	go func() {
		done <- w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":1,"update":[{"item_id":%q,"price_minor":1}]}`, items[0]), uuid.NewString())
	}()
	select {
	case r := <-done:
		t.Fatalf("the edit completed (%d) while finalize held the row", r.Status)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := finalizer.Exec(context.Background(), `UPDATE expenses SET status = 'finalized', finalized_at = now() WHERE id = $1`, exp); err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Status != 409 || !strings.Contains(string(r.Body), "invalid_state") {
		t.Fatalf("edit after finalize = %d %s, want 409 invalid_state", r.Status, r.Body)
	}
	if e := w.get(t, w.member, exp); e.Items[0].PriceMinor != 1500 {
		t.Errorf("price changed on a finalized expense: %d", e.Items[0].PriceMinor)
	}
}

// Many claimers, edits and a finalize at once: whatever the interleaving, the ledger
// equals what the claims table implies (an edit never leaves a finalized expense
// priced differently from what its claims were made at).
func TestEdit_RaceWithClaimsAndFinalizeKeepsTheLedgerHonest(t *testing.T) {
	for round := 0; round < 12; round++ {
		w := newWorld(t)
		exp, items := w.newExpense(t, w.member, goodBody)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var fin teststack.Response
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fin = w.finalize(t, w.member, w.group, exp, splitBody(w.memberID), uuid.NewString())
		}()
		for i := 0; i < 3; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				w.patch(t, w.member, w.group, exp, fmt.Sprintf(`{"version":%d,"update":[{"item_id":%q,"price_minor":%d}]}`, 1+i, items[0], 1600+i), uuid.NewString())
			}()
			go func() {
				defer wg.Done()
				<-start
				w.put(t, w.owner, w.group, exp, items[i%2])
			}()
		}
		close(start)
		wg.Wait()
		if fin.Status == 200 {
			w.assertLedgerMatchesClaims(t, exp)
		}
	}
}

// ---- confirm ----

func TestConfirm(t *testing.T) {
	w := newWorld(t)
	exp, items := w.newExpense(t, w.member, goodBody)
	w.setStatus(t, exp, "needs_review")
	key := uuid.NewString()

	if r := w.confirm(t, w.member, w.group, exp, `{"version":4}`, uuid.NewString()); r.Status != 409 || !strings.Contains(string(r.Body), "version_conflict") {
		t.Errorf("stale version: %d %s", r.Status, r.Body)
	}
	if r := w.claimedAttempt(t, exp, items[0]); r != 409 {
		t.Errorf("a claim on a needs_review expense = %d, want 409", r)
	}
	for name, body := range map[string]string{"no body": ``, "no version": `{}`, "zero": `{"version":0}`, "extra": `{"version":1,"x":1}`} {
		if r := w.confirm(t, w.member, w.group, exp, body, uuid.NewString()); r.Status != 400 {
			t.Errorf("%s: %d, want 400", name, r.Status)
		}
	}
	if r := w.confirm(t, w.owner, w.group, exp, `{"version":1}`, uuid.NewString()); r.Status != 403 {
		t.Errorf("group owner = %d, want 403", r.Status)
	}
	if r := w.confirm(t, w.outsider, w.group, exp, `{"version":1}`, uuid.NewString()); r.Status != 404 {
		t.Errorf("outsider = %d, want 404", r.Status)
	}
	if e := w.get(t, w.member, exp); e.Status != "needs_review" || e.Version != 1 {
		t.Fatalf("refusals changed the expense: %+v", e)
	}

	r := w.confirm(t, w.member, w.group, exp, `{"version":1}`, key)
	if r.Status != 200 {
		t.Fatalf("confirm = %d: %s", r.Status, r.Body)
	}
	var got expense
	r.JSON(t, &got)
	if got.Status != "claiming" || got.Version != 2 {
		t.Errorf("after confirm: %s v%d", got.Status, got.Version)
	}
	if again := w.confirm(t, w.member, w.group, exp, `{"version":1}`, key); again.Status != 200 || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay = %d", again.Status)
	}
	if n := w.editAudit(t, exp, "expense.confirmed"); n != 1 {
		t.Errorf("audit events = %d", n)
	}
	// It is open for claiming now, and no longer confirmable.
	if r := w.put(t, w.owner, w.group, exp, items[0]); r.Status != 200 {
		t.Errorf("claim after confirm = %d", r.Status)
	}
	if r := w.confirm(t, w.member, w.group, exp, `{"version":2}`, uuid.NewString()); r.Status != 409 || !strings.Contains(string(r.Body), "invalid_state") {
		t.Errorf("confirm of a claiming expense: %d %s", r.Status, r.Body)
	}
}

func (w *world) claimedAttempt(t testing.TB, exp, item string) int {
	return w.put(t, w.owner, w.group, exp, item).Status
}

func TestConfirm_TwoConfirmsAtOnce(t *testing.T) {
	w := newWorld(t)
	exp, _ := w.newExpense(t, w.member, goodBody)
	w.setStatus(t, exp, "needs_review")
	var wg sync.WaitGroup
	res := make([]int, 2)
	start := make(chan struct{})
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res[i] = w.confirm(t, w.member, w.group, exp, `{"version":1}`, uuid.NewString()).Status
		}()
	}
	close(start)
	wg.Wait()
	if !(res[0] == 200 && res[1] == 409) && !(res[0] == 409 && res[1] == 200) {
		t.Fatalf("statuses = %v, want one 200 and one 409", res)
	}
	if n := w.editAudit(t, exp, "expense.confirmed"); n != 1 {
		t.Errorf("audit events = %d", n)
	}
}

func TestEdit_Unauthenticated(t *testing.T) {
	w := newWorld(t)
	exp, _ := w.newExpense(t, w.member, goodBody)
	if r := w.s.Do(t, "", "PATCH", expenseURL(w.group, exp), `{"version":1,"tip_minor":1}`, "Idempotency-Key", uuid.NewString()); r.Status != http.StatusUnauthorized {
		t.Errorf("PATCH = %d", r.Status)
	}
	if r := w.s.Do(t, "", "POST", expenseURL(w.group, exp)+"/confirm", `{"version":1}`, "Idempotency-Key", uuid.NewString()); r.Status != http.StatusUnauthorized {
		t.Errorf("confirm = %d", r.Status)
	}
}
