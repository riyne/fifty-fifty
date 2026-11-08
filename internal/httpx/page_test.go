package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/httpx"
)

func TestCursor_RoundTripKeepsMicrosecondsAndTheID(t *testing.T) {
	// Postgres timestamptz has microsecond precision; a cursor that rounded it would
	// compare unequal to the row it came from and repeat or skip that row.
	at := time.Date(2026, 11, 8, 20, 29, 18, 123456000, time.UTC)
	id := uuid.New()
	got, err := httpx.ParseCursor(httpx.Cursor{At: at, ID: id}.String())
	if err != nil || !got.At.Equal(at) || got.ID != id {
		t.Fatalf("round trip = %+v, %v, want %v %v", got, err, at, id)
	}
	// A time in another zone comes back as the same instant.
	zone := time.FixedZone("x", -7*3600)
	got, err = httpx.ParseCursor(httpx.Cursor{At: at.In(zone), ID: id}.String())
	if err != nil || !got.At.Equal(at) {
		t.Fatalf("zoned round trip = %+v, %v", got, err)
	}
}

func TestParseCursor_RefusesWhatThisServerDidNotMake(t *testing.T) {
	good := httpx.Cursor{At: time.Now(), ID: uuid.New()}.String()
	for name, in := range map[string]string{
		"empty":            "",
		"not base64":       "%%%",
		"base64 not json":  "bm90IGpzb24",
		"json wrong shape": "eyJ0IjoxfQ",
		"bad time":         "eyJ0Ijoibm9wZSIsImkiOiIwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDAifQ",
		"truncated":        good[:len(good)-4],
	} {
		if _, err := httpx.ParseCursor(in); err == nil {
			t.Errorf("%s: %q was accepted", name, in)
		}
	}
}

func TestParsePage(t *testing.T) {
	cur := httpx.Cursor{At: time.Now(), ID: uuid.New()}.String()
	for _, tc := range []struct {
		query    string
		limit    int
		hasAfter bool
		bad      bool
	}{
		{"", 100, false, false},
		{"limit=1", 1, false, false},
		{"limit=100", 100, false, false},
		{"limit=7&cursor=" + cur, 7, true, false},
		{"cursor=" + cur, 100, true, false},
		{"limit=0", 0, false, true},
		{"limit=101", 0, false, true},
		{"limit=-1", 0, false, true},
		{"limit=abc", 0, false, true},
		{"limit=", 0, false, true},
		{"limit=2&limit=3", 0, false, true},
		{"cursor=garbage", 0, false, true},
		{"cursor=", 0, false, true},
		{"cursor=" + cur + "&cursor=" + cur, 0, false, true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil)
		p, err := httpx.ParsePage(r)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: accepted as %+v, want an error", tc.query, p)
			}
			continue
		}
		if err != nil || p.Limit != tc.limit || (p.After != nil) != tc.hasAfter {
			t.Errorf("%q: %+v, %v, want limit %d, cursor %v", tc.query, p, err, tc.limit, tc.hasAfter)
		}
	}
}

type row struct {
	at time.Time
	id uuid.UUID
}

func key(r row) (time.Time, uuid.UUID) { return r.at, r.id }

// A list query fetches one row more than the page. Fewer or exactly the page is the last
// page; one more means there is a next page, whose cursor is the last row KEPT (not the
// extra one).
func TestTrim(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(n int) []row {
		out := make([]row, n)
		for i := range out {
			out[i] = row{at: base.Add(-time.Duration(i) * time.Second), id: uuid.New()}
		}
		return out
	}
	for _, n := range []int{0, 1, 2, 3} {
		rows := mk(n)
		got, next := httpx.Trim(rows, 3, key)
		if len(got) != n || next != "" {
			t.Errorf("%d rows, limit 3: kept %d, cursor %q, want all and no cursor", n, len(got), next)
		}
	}
	rows := mk(4)
	got, next := httpx.Trim(rows, 3, key)
	if len(got) != 3 || next == "" {
		t.Fatalf("4 rows, limit 3: kept %d, cursor %q", len(got), next)
	}
	c, err := httpx.ParseCursor(next)
	if err != nil || c.ID != rows[2].id || !c.At.Equal(rows[2].at) {
		t.Errorf("the cursor is %+v (%v), want the last kept row %v, not the extra one", c, err, rows[2].id)
	}
}

func TestPageQueryArgs_FetchOneMore(t *testing.T) {
	id, at, fetch := httpx.Page{Limit: 5}.QueryArgs()
	if id.Valid || at != nil || fetch != 6 {
		t.Errorf("first page: %v %v %d, want no position and 6", id, at, fetch)
	}
	c := httpx.Cursor{At: time.Now(), ID: uuid.New()}
	id, at, fetch = httpx.Page{Limit: 100, After: &c}.QueryArgs()
	if !id.Valid || id.Bytes != c.ID || at == nil || !at.Equal(c.At) || fetch != 101 {
		t.Errorf("later page: %v %v %d", id, at, fetch)
	}
}
