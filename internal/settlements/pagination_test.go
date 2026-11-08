package settlements_test

import (
	"net/http"
	"slices"
	"testing"
)

// The settlement list pages like the others: every page size returns every settlement
// once, newest first.
func TestRead_ListPages(t *testing.T) {
	e := newEnv(t, "owner", "alice")
	e.owe(t, "owner", "alice", 3000)
	var created []string
	for _, amt := range []int64{100, 200, 300, 400, 500} {
		var s settlement
		e.settle(t, "alice", "owner", amt).JSON(t, &s)
		created = append(created, s.ID.String())
	}
	slices.Reverse(created) // newest first

	url := "/v1/groups/" + e.group + "/settlements"
	for _, limit := range []int{1, 2, 4, 5, 6, 0} {
		var got []string
		for _, p := range e.s.ListAll(t, e.tok["owner"], url, "settlements", limit) {
			got = append(got, p.IDs...)
		}
		if !slices.Equal(got, created) {
			t.Errorf("limit %d: %v, want %v", limit, got, created)
		}
	}
	if p := e.s.ListPage(t, e.tok["owner"], url, "settlements", "limit=5"); len(p.IDs) != 5 || p.Next != "" {
		t.Errorf("limit 5 of 5: %d items, cursor %q, want 5 and none", len(p.IDs), p.Next)
	}

	// Access: an outsider is refused with or without a cursor; a bad limit is a 400.
	out := e.s.Token(t, "outsider")
	cursor := e.s.ListPage(t, e.tok["owner"], url, "settlements", "limit=1").Next
	for _, q := range []string{"", "limit=1&cursor=" + cursor} {
		if p := e.s.ListPage(t, out, url, "settlements", q); p.Status != http.StatusNotFound {
			t.Errorf("an outsider with %q = %d, want 404", q, p.Status)
		}
	}
	if p := e.s.ListPage(t, e.tok["owner"], url, "settlements", "limit=0"); p.Status != http.StatusBadRequest {
		t.Errorf("limit=0 = %d, want 400", p.Status)
	}
}
