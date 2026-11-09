package expenses_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/riyne/fifty-fifty/internal/harness/teststack"
)

func (w *world) listAll(t testing.TB, token string, limit int) []teststack.ListPage {
	t.Helper()
	return w.s.ListAll(t, token, "/v1/groups/"+w.group+"/expenses", "expenses", limit)
}

// The order the list promises: newest first, ties by id. Read straight from the database.
func (w *world) expenseOrder(t testing.TB) []string {
	t.Helper()
	rows, err := w.s.Pool.Query(t.Context(), `SELECT id::text FROM expenses WHERE group_id = $1 ORDER BY created_at DESC, id`, w.group)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func flat(pages []teststack.ListPage) []string {
	var out []string
	for _, p := range pages {
		out = append(out, p.IDs...)
	}
	return out
}

// Every page size gives every expense exactly once in the promised order, including when
// several expenses have the very same created_at (only the id then orders them, and a page
// boundary can fall between them).
func TestListExpenses_EverySizeReturnsEveryExpenseOnceInOrder(t *testing.T) {
	w := newWorld(t)
	var ids []string
	for range 7 {
		id, _ := w.newExpense(t, w.member, goodBody)
		ids = append(ids, id)
	}
	w.s.Exec(t, `UPDATE expenses SET created_at = '2026-01-02T03:04:05.123456Z' WHERE id = ANY($1::uuid[])`, ids[:5])
	want := w.expenseOrder(t)
	if len(want) != 7 {
		t.Fatalf("setup: %d expenses", len(want))
	}
	for _, limit := range []int{1, 2, 3, 4, 6, 7, 8, 100, 0} {
		if got := flat(w.listAll(t, w.member, limit)); !slices.Equal(got, want) {
			t.Errorf("limit %d: the pages together are %v, want %v", limit, got, want)
		}
	}
	// Exactly a page: no cursor. One more: a cursor and a last page of one.
	if p := w.s.ListPage(t, w.member, "/v1/groups/"+w.group+"/expenses", "expenses", "limit=7"); len(p.IDs) != 7 || p.Next != "" {
		t.Errorf("limit 7 of 7: %d items, cursor %q", len(p.IDs), p.Next)
	}
	if p := w.s.ListPage(t, w.member, "/v1/groups/"+w.group+"/expenses", "expenses", "limit=6"); len(p.IDs) != 6 || p.Next == "" {
		t.Errorf("limit 6 of 7: %d items, cursor %q", len(p.IDs), p.Next)
	}
}

// A cursor holds a position, not an offset: an expense created (newer) or removed between
// two page fetches neither repeats nor skips what comes after the cursor.
func TestListExpenses_AnExpenseAddedBetweenPagesChangesNothingAfterTheCursor(t *testing.T) {
	w := newWorld(t)
	for range 6 {
		w.newExpense(t, w.member, goodBody)
	}
	before := w.expenseOrder(t)
	first := w.s.ListPage(t, w.member, "/v1/groups/"+w.group+"/expenses", "expenses", "limit=2")
	if !slices.Equal(first.IDs, before[:2]) || first.Next == "" {
		t.Fatalf("first page = %v (cursor %q), want %v", first.IDs, first.Next, before[:2])
	}

	added, _ := w.newExpense(t, w.member, goodBody) // newer than everything: it sorts first
	rest := flat(w.s.ListAll(t, w.member, "/v1/groups/"+w.group+"/expenses", "expenses", 0))
	if rest[0] != added {
		t.Fatalf("setup: the new expense is not first in a fresh list")
	}

	var after []string
	cursor := first.Next
	for cursor != "" {
		p := w.s.ListPage(t, w.member, "/v1/groups/"+w.group+"/expenses", "expenses", "limit=2&cursor="+cursor)
		if p.Status != 200 {
			t.Fatalf("page = %d: %s", p.Status, p.Body)
		}
		after = append(after, p.IDs...)
		cursor = p.Next
	}
	if !slices.Equal(after, before[2:]) {
		t.Errorf("after the cursor: %v, want exactly the rest of the original list %v (the new expense %s must not appear or shift anything)", after, before[2:], added)
	}
}

// A cursor needs the same access as the first page: a non-member gets the same 404 with or
// without one, and a bad limit or cursor is a 400 for a member.
func TestListExpenses_CursorsAndAccess(t *testing.T) {
	w := newWorld(t)
	for range 3 {
		w.newExpense(t, w.member, goodBody)
	}
	url := "/v1/groups/" + w.group + "/expenses"
	cursor := w.s.ListPage(t, w.member, url, "expenses", "limit=1").Next
	for _, q := range []string{"", "limit=1", "limit=1&cursor=" + cursor} {
		if p := w.s.ListPage(t, w.outsider, url, "expenses", q); p.Status != http.StatusNotFound {
			t.Errorf("an outsider with %q = %d, want 404", q, p.Status)
		}
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "cursor=nonsense", "cursor="} {
		if p := w.s.ListPage(t, w.member, url, "expenses", q); p.Status != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", q, p.Status)
		}
	}
	// A cursor from this group used on another group the caller is in shows only that
	// group's expenses.
	other := w.s.CreateGroup(t, w.outsider, "other")
	otherID := w.s.CreateExpense(t, w.outsider, other, goodBody)
	p := w.s.ListPage(t, w.outsider, "/v1/groups/"+other+"/expenses", "expenses", "cursor="+cursor)
	for _, id := range p.IDs {
		if id != otherID {
			t.Errorf("a cursor from another group made %s appear in this one", id)
		}
	}
}
