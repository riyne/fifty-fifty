package groups_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/httpx"

	"github.com/riyne/fifty-fifty/internal/teststack"
)

func idsOf(pages []teststack.ListPage) []string {
	var out []string
	for _, p := range pages {
		out = append(out, p.IDs...)
	}
	return out
}

// Before M10.15 the 101st group a user belonged to was silently not returned. Now the
// first page is the same 100 and says there is more; the next page has the rest.
func TestListGroups_NothingIsSilentlyDropped(t *testing.T) {
	s := teststack.New(t)
	tok := s.Token(t, "many")
	var created []string
	for range 105 {
		created = append(created, s.CreateGroup(t, tok, "g"))
	}

	pages := s.ListAll(t, tok, "/v1/groups", "groups", 0)
	if len(pages) != 2 || len(pages[0].IDs) != 100 || len(pages[1].IDs) != 5 || pages[1].Next != "" {
		t.Fatalf("got %d pages of %v, want one of 100 (with a next_cursor) and one of 5", len(pages), func() (n []int) {
			for _, p := range pages {
				n = append(n, len(p.IDs))
			}
			return
		}())
	}
	all := idsOf(pages)
	// Newest first: the reverse of the order they were created in.
	slices.Reverse(created)
	if !slices.Equal(all, created) {
		t.Errorf("the pages together are not all 105 groups, newest first")
	}
}

func TestListGroups_SmallPagesAndTheExactBoundary(t *testing.T) {
	s := teststack.New(t)
	tok := s.Token(t, "boundary")
	for range 4 {
		s.CreateGroup(t, tok, "g")
	}
	// Exactly a page: no next_cursor (no empty last page to fetch).
	if p := s.ListPage(t, tok, "/v1/groups", "groups", "limit=4"); len(p.IDs) != 4 || p.Next != "" {
		t.Errorf("limit 4 of 4: %d items, cursor %q, want 4 and none", len(p.IDs), p.Next)
	}
	// One more than a page: a cursor, then a last page of one.
	pages := s.ListAll(t, tok, "/v1/groups", "groups", 3)
	if len(pages) != 2 || len(pages[0].IDs) != 3 || len(pages[1].IDs) != 1 {
		t.Errorf("limit 3 of 4 gave %d pages", len(pages))
	}
	pages = s.ListAll(t, tok, "/v1/groups", "groups", 1)
	if len(pages) != 4 {
		t.Errorf("limit 1 of 4 gave %d pages, want 4", len(pages))
	}
	// A user with no groups: an empty list, no cursor.
	if p := s.ListPage(t, s.Token(t, "none"), "/v1/groups", "groups", ""); p.Status != 200 || len(p.IDs) != 0 || p.Next != "" {
		t.Errorf("no groups: %d %v %q", p.Status, p.IDs, p.Next)
	}
}

func TestListGroups_BadLimitOrCursorIs400(t *testing.T) {
	s := teststack.New(t)
	tok := s.Token(t, "bad")
	s.CreateGroup(t, tok, "g")
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "limit=-3", "cursor=nonsense", "limit=1&limit=2"} {
		p := s.ListPage(t, tok, "/v1/groups", "groups", q)
		if p.Status != http.StatusBadRequest || !strings.Contains(string(p.Body), `"code":"invalid_request"`) {
			t.Errorf("%q = %d %s, want 400 invalid_request", q, p.Status, p.Body)
		}
	}
}

// A cursor says where to start, not what to show: a user never sees a group they are not
// in, whatever cursor they send, including one made from someone else's list.
func TestListGroups_ACursorCannotShowSomeoneElsesGroups(t *testing.T) {
	s := teststack.New(t)
	alice, mallory := s.Token(t, "alice"), s.Token(t, "mallory")
	var mine []string
	for range 3 {
		mine = append(mine, s.CreateGroup(t, alice, "alice's"))
	}
	theirs := s.CreateGroup(t, mallory, "mallory's")

	cursor := s.ListPage(t, alice, "/v1/groups", "groups", "limit=1").Next
	if cursor == "" {
		t.Fatal("no cursor from alice's list")
	}
	p := s.ListPage(t, mallory, "/v1/groups", "groups", "cursor="+cursor)
	for _, id := range p.IDs {
		if slices.Contains(mine, id) {
			t.Fatalf("mallory was shown alice's group %s", id)
		}
	}
	// And a cursor older than everything shows nothing, not other people's rows.
	if p := s.ListPage(t, mallory, "/v1/groups", "groups", "cursor="+oldestCursor()); len(p.IDs) != 0 {
		t.Errorf("a cursor before every row returned %v (mallory's own group is %s)", p.IDs, theirs)
	}
}

// ---- join codes ----

func TestListJoinCodes_PagesWithTies(t *testing.T) {
	s := teststack.New(t)
	owner := s.Token(t, "owner")
	g := s.CreateGroup(t, owner, "codes")
	var created []string
	for range 5 {
		id, _ := s.CreateJoinCode(t, owner, g, 0, 0)
		created = append(created, id)
	}
	// Three codes with the very same created_at: only the id breaks the tie, and a page
	// boundary may fall between them.
	s.Exec(t, `UPDATE join_codes SET created_at = '2026-01-02T03:04:05.123456Z' WHERE id = ANY($1::uuid[])`, created[:3])

	var want []string
	rows, err := s.Pool.Query(t.Context(), `SELECT id::text FROM join_codes WHERE group_id = $1 ORDER BY created_at DESC, id`, g)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	rows.Close()

	for _, limit := range []int{1, 2, 3, 4, 5, 100} {
		got := idsOf(s.ListAll(t, owner, "/v1/groups/"+g+"/join-codes", "join_codes", limit))
		if !slices.Equal(got, want) {
			t.Errorf("limit %d: pages together = %v, want %v", limit, got, want)
		}
	}

	// Only the owner may list, with or without a cursor.
	member := s.Token(t, "member")
	_, code := s.CreateJoinCode(t, owner, g, 0, 0)
	if r := s.Join(t, member, code); r.Status != http.StatusCreated {
		t.Fatalf("join = %d", r.Status)
	}
	cursor := s.ListPage(t, owner, "/v1/groups/"+g+"/join-codes", "join_codes", "limit=1").Next
	if p := s.ListPage(t, member, "/v1/groups/"+g+"/join-codes", "join_codes", "limit=1&cursor="+cursor); p.Status != http.StatusForbidden {
		t.Errorf("a member listing join codes with a cursor = %d, want 403", p.Status)
	}
	if p := s.ListPage(t, owner, "/v1/groups/"+g+"/join-codes", "join_codes", "limit=0"); p.Status != http.StatusBadRequest {
		t.Errorf("limit=0 = %d, want 400", p.Status)
	}
}

// oldestCursor is a cursor for a position before any row.
func oldestCursor() string {
	return httpx.Cursor{At: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), ID: uuid.Nil}.String()
}
