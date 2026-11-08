package teststack

import (
	"encoding/json"
	"net/url"
	"testing"
)

// ListPage is one response of a paginated list endpoint (M10.15).
type ListPage struct {
	Status int
	IDs    []string // the "id" of each item, in the order returned
	Next   string   // next_cursor, "" on the last page
	Body   []byte
}

// ListPage fetches one page of the list at path. key is the JSON field that holds the
// items ("groups", "expenses", "join_codes", "settlements"); query is the raw query
// string to add ("limit=2", "limit=2&cursor=...", or "").
func (s *Stack) ListPage(t testing.TB, token, path, key, query string) ListPage {
	t.Helper()
	if query != "" {
		path += "?" + query
	}
	r := s.Do(t, token, "GET", path, "")
	p := ListPage{Status: r.Status, Body: r.Body}
	if r.Status != 200 {
		return p
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("list %s: not a JSON object: %v: %s", path, err, r.Body)
	}
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body[key], &items); err != nil {
		t.Fatalf("list %s: %q is not an array of items: %v: %s", path, key, err, r.Body)
	}
	for _, it := range items {
		p.IDs = append(p.IDs, it.ID)
	}
	if raw, ok := body["next_cursor"]; ok {
		if err := json.Unmarshal(raw, &p.Next); err != nil {
			t.Fatalf("list %s: next_cursor is not a string: %s", path, raw)
		}
	}
	return p
}

// ListAll follows next_cursor from the first page until there is none, asking for limit
// items at a time (0: the server's default), and returns every page. It fails the test
// on a non-200 page or on more than 1000 pages (a cursor that never ends).
func (s *Stack) ListAll(t testing.TB, token, path, key string, limit int) []ListPage {
	t.Helper()
	var pages []ListPage
	cursor := ""
	for range 1000 {
		q := url.Values{}
		if limit > 0 {
			q.Set("limit", itoa(limit))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		p := s.ListPage(t, token, path, key, q.Encode())
		if p.Status != 200 {
			t.Fatalf("list %s page %d = %d: %s", path, len(pages)+1, p.Status, p.Body)
		}
		pages = append(pages, p)
		if p.Next == "" {
			return pages
		}
		cursor = p.Next
	}
	t.Fatalf("list %s: still more pages after 1000", path)
	return nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
