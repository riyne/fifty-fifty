package httpx

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Pagination for the list endpoints. A list is newest first, ordered by
// (created_at DESC, id), and a page is found by keyset: the cursor is the last row of the
// previous page, and the query asks for the rows after it. A cursor is therefore stable
// while rows are added or removed (an offset would skip or repeat rows), and costs the
// same on every page.
//
// The cursor is opaque to a client and is NOT signed. That is safe because it only says
// where to start: every list query still applies its own membership scope, so a forged
// or borrowed cursor can only move the position inside rows the caller may already see.

const (
	// DefaultPageSize is what a list returns when the client does not say, and
	// MaxPageSize is the most it will return: the same 100 these lists always returned,
	// so a client that sends neither parameter sees the first page it always saw.
	DefaultPageSize = 100
	MaxPageSize     = 100
)

// ErrBadPage is a limit or cursor the client sent that is not valid. Its message is safe
// to show the client.
var ErrBadPage = errors.New("invalid limit or cursor")

// Cursor is the position after which the next page starts.
type Cursor struct {
	At time.Time
	ID uuid.UUID
}

type cursorJSON struct {
	T string `json:"t"`
	I string `json:"i"`
}

// String is the opaque form a client hands back.
func (c Cursor) String() string {
	b, _ := json.Marshal(cursorJSON{T: c.At.UTC().Format(time.RFC3339Nano), I: c.ID.String()})
	return base64.RawURLEncoding.EncodeToString(b)
}

// ParseCursor reads what String wrote. Anything else is ErrBadPage.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadPage
	}
	var j cursorJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return Cursor{}, ErrBadPage
	}
	at, err1 := time.Parse(time.RFC3339Nano, j.T)
	id, err2 := uuid.Parse(j.I)
	if err1 != nil || err2 != nil {
		return Cursor{}, ErrBadPage
	}
	return Cursor{At: at, ID: id}, nil
}

// Page is what a client asked for.
type Page struct {
	Limit int     // 1 to MaxPageSize
	After *Cursor // nil for the first page
}

// ParsePage reads the `limit` and `cursor` query parameters. A limit that is not an
// integer from 1 to MaxPageSize, or a cursor this server did not make, is ErrBadPage
// (the handler answers 400 invalid_request).
func ParsePage(r *http.Request) (Page, error) {
	p := Page{Limit: DefaultPageSize}
	q := r.URL.Query()
	if v, ok := q["limit"]; ok {
		if len(v) != 1 {
			return Page{}, ErrBadPage
		}
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 1 || n > MaxPageSize {
			return Page{}, ErrBadPage
		}
		p.Limit = n
	}
	if v, ok := q["cursor"]; ok {
		if len(v) != 1 {
			return Page{}, ErrBadPage
		}
		c, err := ParseCursor(v[0])
		if err != nil {
			return Page{}, err
		}
		p.After = &c
	}
	return p, nil
}

// QueryArgs are the values the list queries take for "after this row" and "how many to
// fetch". The fetch size is one more than the page, so that a full result means another
// page exists without a second query.
func (p Page) QueryArgs() (afterID pgtype.UUID, afterAt *time.Time, fetch int32) {
	if p.After != nil {
		at := p.After.At
		afterID, afterAt = pgtype.UUID{Bytes: p.After.ID, Valid: true}, &at
	}
	return afterID, afterAt, int32(p.Limit + 1)
}

// Trim cuts the extra row a list query fetched. It returns the rows of this page and the
// cursor for the next one ("" when this is the last page). key gives a row's created_at
// and id.
func Trim[T any](rows []T, limit int, key func(T) (time.Time, uuid.UUID)) ([]T, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	rows = rows[:limit]
	at, id := key(rows[limit-1])
	return rows, Cursor{At: at, ID: id}.String()
}

// WriteBadPage answers a bad limit or cursor.
func WriteBadPage(w http.ResponseWriter) {
	WriteError(w, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 100, and cursor must be one returned by this endpoint")
}
