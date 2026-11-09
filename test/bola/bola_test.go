// Package bola is the broken-object-level-authorization suite.
//
// For every authenticated operation in api/openapi.yaml it asks: when group A's
// ids are used by someone who is not allowed to use them, does the api refuse,
// and without leaking that the id exists or changing anything?
//
// Actors, relative to group A:
//
//	anon     no token
//	outsider authenticated, owns a different group B, not a member of A
//	member   joined A through a join code; not the owner
//	owner    created A
//
// TestEveryOperationIsCovered fails when an operation is added to the OpenAPI
// spec without a row here (or an explicit, reasoned exemption), so new
// endpoints cannot silently skip authorization testing.
package bola_test

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/riyne/fifty-fifty/internal/harness/teststack"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

type actor string

const (
	anon     actor = "anon"
	outsider actor = "outsider"
	member   actor = "member"
	owner    actor = "owner"
)

// actorOrder runs the denied actors first so an owner's successful mutation
// cannot mask a denied actor's earlier success.
var actorOrder = []actor{anon, outsider, member, owner}

type fixture struct {
	s                 *teststack.Stack
	tokens            map[actor]string
	groupA            string // owned by `owner`, `member` belongs, `outsider` does not
	groupB            string // owned by `outsider`
	codeA             string // a join code id in A
	codeB             string // a join code id in B
	expA              string // an expense in A, created by `owner`
	expB              string // an expense in B, created by `outsider`
	itemA             string // first item of expA
	itemB             string // first item of expB
	ownerID, memberID string // user ids, for bodies that name members
	outsiderID        string
	settlementA       string // a pending settlement in A, from `member` to `owner`
	settlementB       string // a pending settlement in B (see ensureSettlementB)
	indebted          bool   // ensureDebt has run
}

const expenseBody = `{"description":"dinner","items":[{"description":"pizza","price_minor":1500},{"description":"beer","price_minor":800}],"tax_minor":300,"tip_minor":200}`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := teststack.New(t)
	f := &fixture{s: s, tokens: map[actor]string{
		anon:     "",
		outsider: s.Token(t, "outsider"),
		member:   s.Token(t, "member"),
		owner:    s.Token(t, "owner"),
	}}
	f.groupA = s.CreateGroup(t, f.tokens[owner], "Group A")
	f.groupB = s.CreateGroup(t, f.tokens[outsider], "Group B")
	// The settlement scenarios enqueue execute jobs nothing here runs; do not leave them
	// in the shared database for a real worker to find.
	t.Cleanup(func() {
		s.Exec(t, `DELETE FROM jobs WHERE kind IN ('settlement.execute', 'settlement.reconcile')
		           AND payload->>'settlement_id' IN (SELECT id::text FROM settlements WHERE group_id IN ($1, $2))`, f.groupA, f.groupB)
	})
	var joinCode string
	f.codeA, joinCode = s.CreateJoinCode(t, f.tokens[owner], f.groupA, 0, 0)
	f.codeB, _ = s.CreateJoinCode(t, f.tokens[outsider], f.groupB, 0, 0)
	if r := s.Join(t, f.tokens[member], joinCode); r.Status != http.StatusCreated {
		t.Fatalf("fixture: member join = %d: %s", r.Status, r.Body)
	}
	f.expA = s.CreateExpense(t, f.tokens[owner], f.groupA, expenseBody)
	f.expB = s.CreateExpense(t, f.tokens[outsider], f.groupB, expenseBody)
	f.ownerID, f.memberID = s.UserID(t, f.tokens[owner]).String(), s.UserID(t, f.tokens[member]).String()
	f.itemA = f.firstItem(t, owner, f.groupA, f.expA)
	f.itemB = f.firstItem(t, outsider, f.groupB, f.expB)
	f.outsiderID = s.UserID(t, f.tokens[outsider]).String()
	f.settlementA = f.insertSettlement(t, f.groupA, f.memberID, f.ownerID)
	return f
}

// insertSettlement writes a pending settlement directly (no ledger effect, no job):
// the read scenarios only need a real settlement id that belongs to a group.
func (f *fixture) insertSettlement(t *testing.T, group, from, to string) string {
	t.Helper()
	var id string
	if err := f.s.Pool.QueryRow(context.Background(),
		`INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 100) RETURNING id`,
		group, from, to).Scan(&id); err != nil {
		t.Fatalf("fixture: settlement: %v", err)
	}
	return id
}

// ensureSettlementB creates, once, a settlement in group B. B has one member, so it
// adds a second who exists only in B. Lazy because other tests assert B's exact
// membership.
func (f *fixture) ensureSettlementB(t *testing.T) string {
	t.Helper()
	if f.settlementB != "" {
		return f.settlementB
	}
	var bOnly string
	if err := f.s.Pool.QueryRow(context.Background(),
		`INSERT INTO users (issuer, subject) VALUES ('bola', $1) RETURNING id`, uuid.NewString()).Scan(&bOnly); err != nil {
		t.Fatalf("fixture: user: %v", err)
	}
	if _, err := f.s.Pool.Exec(context.Background(),
		`INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, f.groupB, bOnly); err != nil {
		t.Fatalf("fixture: membership: %v", err)
	}
	f.settlementB = f.insertSettlement(t, f.groupB, bOnly, f.outsiderID)
	return f.settlementB
}

// ensureDebt makes `member` owe money to `owner` in group A (one finalized expense),
// once, so a settlement from member to owner is allowed. Done lazily so fixtures
// that need an empty ledger are unaffected.
func (f *fixture) ensureDebt(t *testing.T) {
	t.Helper()
	if !f.indebted {
		f.finalizeWithMemberClaim(t, owner, f.groupA, expenseBody)
		f.indebted = true
	}
}

// finalizeWithMemberClaim has `payer` create an expense in the group, the fixture
// `member` claim its first item, and the payer finalize (the payer takes the
// rest). It returns what the member owes.
func (f *fixture) finalizeWithMemberClaim(t *testing.T, payer actor, group, body string) int64 {
	t.Helper()
	exp := f.s.CreateExpense(t, f.tokens[payer], group, body)
	item := f.firstItem(t, payer, group, exp)
	if r := f.s.Do(t, f.tokens[member], "PUT", claimPath(group, exp, item), ""); r.Status != 200 {
		t.Fatalf("member claim = %d: %s", r.Status, r.Body)
	}
	payerID := f.s.UserID(t, f.tokens[payer]).String()
	r := f.s.Do(t, f.tokens[payer], "POST", finalizePath(group, exp), `{"split_rest_among":["`+payerID+`"]}`, "Idempotency-Key", uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	var fin struct {
		Shares []struct {
			MemberID   string `json:"member_id"`
			TotalMinor int64  `json:"total_minor"`
		} `json:"shares"`
	}
	r.JSON(t, &fin)
	for _, s := range fin.Shares {
		if s.MemberID == f.memberID {
			return s.TotalMinor
		}
	}
	t.Fatal("member has no share in the finalize response")
	return 0
}

// finalizedBy has `payer` create an expense in the group and finalize it (taking
// every item themselves), and returns its id.
func (f *fixture) finalizedBy(t *testing.T, payer actor, group string) string {
	t.Helper()
	exp := f.s.CreateExpense(t, f.tokens[payer], group, expenseBody)
	payerID := f.s.UserID(t, f.tokens[payer]).String()
	r := f.s.Do(t, f.tokens[payer], "POST", finalizePath(group, exp), `{"split_rest_among":["`+payerID+`"]}`, "Idempotency-Key", uuid.NewString())
	if r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}
	return exp
}

// firstItem returns the id of an expense's first item, read through the API.
func (f *fixture) firstItem(t *testing.T, a actor, group, expense string) string {
	t.Helper()
	var e struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	f.s.Do(t, f.tokens[a], "GET", "/v1/groups/"+group+"/expenses/"+expense, "").JSON(t, &e)
	if len(e.Items) == 0 {
		t.Fatal("fixture expense has no items")
	}
	return e.Items[0].ID
}

// state summarizes everything a denied request must not change, for both groups.
func (f *fixture) state(t *testing.T) string {
	t.Helper()
	q := func(sql string) int { return f.s.QueryInt(t, sql, f.groupA, f.groupB) }
	return fmt.Sprintf("codes=%d revoked=%d uses=%d members=%d audit=%d expenses=%d items=%d claims=%d finalized=%d ledger_tx=%d ledger_entries=%d settlements=%d settlement_jobs=%d versions=%d item_cents=%d tax_tip=%d receipts=%d quota=%d",
		q(`SELECT count(*) FROM join_codes WHERE group_id IN ($1, $2)`),
		q(`SELECT count(revoked_at) FROM join_codes WHERE group_id IN ($1, $2)`),
		q(`SELECT coalesce(sum(uses), 0) FROM join_codes WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM memberships WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM audit_events WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM expenses WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM expense_items i JOIN expenses e ON e.id = i.expense_id WHERE e.group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM claims WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM expenses WHERE status <> 'claiming' AND group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM ledger_transactions WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM ledger_entries WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM settlements WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM jobs j JOIN settlements s ON s.id::text = j.payload->>'settlement_id' WHERE j.kind = 'settlement.execute' AND s.group_id IN ($1, $2)`),
		q(`SELECT coalesce(sum(version), 0) FROM expenses WHERE group_id IN ($1, $2)`),
		q(`SELECT coalesce(sum(price_minor), 0) FROM expense_items i JOIN expenses e ON e.id = i.expense_id WHERE e.group_id IN ($1, $2)`),
		q(`SELECT coalesce(sum(tax_minor + tip_minor), 0) FROM expenses WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM receipts WHERE group_id IN ($1, $2)`),
		q(`SELECT coalesce(sum(used), 0) FROM receipt_quota WHERE user_id IN (SELECT user_id FROM memberships WHERE group_id IN ($1, $2))`),
	)
}

// uploadContentType and uploadBody are a valid receipt upload: one multipart file
// part holding a rendered synthetic receipt, with a fixed boundary so the content type
// can be a constant.
const uploadContentType = "multipart/form-data; boundary=bolaboundary"

var (
	uploadOnce sync.Once
	uploadForm string
)

func uploadBody(*fixture) string {
	uploadOnce.Do(func() {
		img, err := synth.Generate(1).Render(synth.RenderOptions{})
		if err != nil {
			panic(err)
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.SetBoundary("bolaboundary"); err != nil {
			panic(err)
		}
		w, _ := mw.CreateFormFile("file", "receipt.png")
		w.Write(img)
		mw.Close()
		uploadForm = buf.String()
	})
	return uploadForm
}

// scenario is one request shape tried as each actor.
type scenario struct {
	name   string
	opID   string // "METHOD /path/template", as written in api/openapi.yaml
	method string
	path   func(f *fixture) string
	body   string
	// bodyFn, if set, builds the body from the fixture (e.g. to name a member's id).
	bodyFn func(f *fixture) string
	// bodyFor, if set, builds the body for one actor (e.g. a settlement that names
	// somebody other than the sender).
	bodyFor func(f *fixture, a actor) string
	// idempotent adds a fresh Idempotency-Key header to every request.
	idempotent bool
	// contentType, if set, is sent as the Content-Type header (default: none).
	contentType string
	// prepare runs before each actor's request (e.g. to mint a fresh code to revoke).
	prepare func(t *testing.T, f *fixture) string // returns the path to use; overrides path if non-nil
	want    map[actor]int
}

func scenarios() []scenario {
	groupPath := func(suffix string) func(f *fixture) string {
		return func(f *fixture) string { return "/v1/groups/" + f.groupA + suffix }
	}
	return []scenario{
		{
			name: "read group", opID: "GET /v1/groups/{id}", method: "GET", path: groupPath(""),
			// Any member may read; only non-members are refused, and with 404, not 403.
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			name: "create join code", opID: "POST /v1/groups/{id}/join-codes", method: "POST", path: groupPath("/join-codes"), body: `{}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 201},
		},
		{
			name: "list join codes", opID: "GET /v1/groups/{id}/join-codes", method: "GET", path: groupPath("/join-codes"),
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 200},
		},
		{
			name: "revoke join code", opID: "DELETE /v1/groups/{id}/join-codes/{codeId}", method: "DELETE",
			prepare: func(t *testing.T, f *fixture) string {
				id, _ := f.s.CreateJoinCode(t, f.tokens[owner], f.groupA, 0, 0)
				return "/v1/groups/" + f.groupA + "/join-codes/" + id
			},
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 204},
		},
		{
			// B's owner reaches for A's code through B's path: the code id exists
			// but belongs to another group, so it must match nothing.
			name: "revoke A's code through B's path", opID: "DELETE /v1/groups/{id}/join-codes/{codeId}", method: "DELETE",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupB + "/join-codes/" + f.codeA },
			want: map[actor]int{anon: 401, outsider: 404 /* owner of B */, member: 404 /* not in B */, owner: 404 /* not in B */},
		},
		{
			// A's owner authorizes on A but names B's code id.
			name: "revoke B's code through A's path", opID: "DELETE /v1/groups/{id}/join-codes/{codeId}", method: "DELETE",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupA + "/join-codes/" + f.codeB },
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 404},
		},
		{
			// Any member may create (the creator becomes the payer); a non-member is
			// refused with 404 before the body or key is even looked at.
			name: "create expense", opID: "POST /v1/groups/{id}/expenses", method: "POST", path: groupPath("/expenses"),
			body: expenseBody, idempotent: true,
			want: map[actor]int{anon: 401, outsider: 404, member: 201, owner: 201},
		},
		{
			// Any member may upload a receipt (the uploader becomes the payer of the new
			// expense); a non-member is refused with 404 before the body or key is looked at.
			name: "upload receipt", opID: "POST /v1/groups/{id}/receipts", method: "POST", path: groupPath("/receipts"),
			bodyFn: uploadBody, contentType: uploadContentType, idempotent: true,
			want: map[actor]int{anon: 401, outsider: 404, member: 202, owner: 202},
		},
		{
			name: "read balances", opID: "GET /v1/groups/{id}/balances", method: "GET", path: groupPath("/balances"),
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			name: "list expenses", opID: "GET /v1/groups/{id}/expenses", method: "GET", path: groupPath("/expenses"),
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			name: "read expense", opID: "GET /v1/groups/{id}/expenses/{expenseId}", method: "GET",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupA + "/expenses/" + f.expA },
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			// Everyone is authorized on A, but the id is B's expense: it must match
			// nothing, even though it exists.
			name: "read B's expense through A's path", opID: "GET /v1/groups/{id}/expenses/{expenseId}", method: "GET",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupA + "/expenses/" + f.expB },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// B's owner is authorized on B but names A's expense.
			name: "read A's expense through B's path", opID: "GET /v1/groups/{id}/expenses/{expenseId}", method: "GET",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupB + "/expenses/" + f.expA },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// Any member may claim; a non-member is refused with 404.
			name: "claim item", opID: "PUT /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", method: "PUT",
			path: func(f *fixture) string { return claimPath(f.groupA, f.expA, f.itemA) },
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			// Authorized on A and on A's expense, but the item belongs to B's expense.
			name: "claim B's item through A's expense", opID: "PUT /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", method: "PUT",
			path: func(f *fixture) string { return claimPath(f.groupA, f.expA, f.itemB) },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// B's expense and item are real and consistent with each other, but not with A's path.
			name: "claim B's item through A's path", opID: "PUT /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", method: "PUT",
			path: func(f *fixture) string { return claimPath(f.groupA, f.expB, f.itemB) },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// B's owner is authorized on B but names A's expense and item.
			name: "claim A's item through B's path", opID: "PUT /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", method: "PUT",
			path: func(f *fixture) string { return claimPath(f.groupB, f.expA, f.itemA) },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			name: "unclaim item", opID: "DELETE /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", method: "DELETE",
			path: func(f *fixture) string { return claimPath(f.groupA, f.expA, f.itemA) },
			want: map[actor]int{anon: 401, outsider: 404, member: 204, owner: 204},
		},
		{
			name: "unclaim B's item through A's path", opID: "DELETE /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", method: "DELETE",
			path: func(f *fixture) string { return claimPath(f.groupA, f.expB, f.itemB) },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// Only the payer may finalize. Each actor gets a fresh expense (created by
			// `owner`, so owner is the payer); a plain member is refused with 403.
			name: "finalize as payer", opID: "POST /v1/groups/{id}/expenses/{expenseId}/finalize", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return finalizePath(f.groupA, f.s.CreateExpense(t, f.tokens[owner], f.groupA, expenseBody))
			},
			bodyFn: func(f *fixture) string { return `{"split_rest_among":["` + f.ownerID + `"]}` },
			want:   map[actor]int{anon: 401, outsider: 404, member: 403, owner: 200},
		},
		{
			// Being the group owner confers nothing: here a plain member is the payer
			// and the owner is the one refused.
			name: "finalize as non-payer owner", opID: "POST /v1/groups/{id}/expenses/{expenseId}/finalize", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return finalizePath(f.groupA, f.s.CreateExpense(t, f.tokens[member], f.groupA, expenseBody))
			},
			bodyFn: func(f *fixture) string { return `{"split_rest_among":["` + f.memberID + `"]}` },
			want:   map[actor]int{anon: 401, outsider: 404, member: 200, owner: 403},
		},
		{
			// Voiding needs a finalized expense, so each actor gets a fresh one. Here a plain
			// member is the payer; the group owner may also void it.
			name: "void as payer or owner", opID: "POST /v1/groups/{id}/expenses/{expenseId}/void", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return voidPath(f.groupA, f.finalizedBy(t, member, f.groupA))
			},
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			// The owner is the payer, so a plain member is neither payer nor owner.
			name: "void as a member who is neither", opID: "POST /v1/groups/{id}/expenses/{expenseId}/void", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return voidPath(f.groupA, f.finalizedBy(t, owner, f.groupA))
			},
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 200},
		},
		{
			name: "void B's expense through A's path", opID: "POST /v1/groups/{id}/expenses/{expenseId}/void", method: "POST", idempotent: true,
			path: func(f *fixture) string { return voidPath(f.groupA, f.expB) },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			name: "void A's expense through B's path", opID: "POST /v1/groups/{id}/expenses/{expenseId}/void", method: "POST", idempotent: true,
			path: func(f *fixture) string { return voidPath(f.groupB, f.expA) },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// Only the payer may edit; being the group owner confers nothing. Each actor gets a
			// fresh expense so every request is at version 1.
			name: "edit as payer", opID: "PATCH /v1/groups/{id}/expenses/{expenseId}", method: "PATCH", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return expensePath(f.groupA, f.s.CreateExpense(t, f.tokens[owner], f.groupA, expenseBody))
			},
			body: editBody,
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 200},
		},
		{
			name: "edit as non-payer owner", opID: "PATCH /v1/groups/{id}/expenses/{expenseId}", method: "PATCH", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return expensePath(f.groupA, f.s.CreateExpense(t, f.tokens[member], f.groupA, expenseBody))
			},
			body: editBody,
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 403},
		},
		{
			name: "edit B's expense through A's path", opID: "PATCH /v1/groups/{id}/expenses/{expenseId}", method: "PATCH", idempotent: true,
			path: func(f *fixture) string { return expensePath(f.groupA, f.expB) },
			body: editBody,
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			name: "edit A's expense through B's path", opID: "PATCH /v1/groups/{id}/expenses/{expenseId}", method: "PATCH", idempotent: true,
			path: func(f *fixture) string { return expensePath(f.groupB, f.expA) },
			body: editBody,
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// Confirm needs a needs_review expense, so each actor gets a fresh one.
			name: "confirm as payer", opID: "POST /v1/groups/{id}/expenses/{expenseId}/confirm", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return confirmPath(f.groupA, f.needsReviewBy(t, owner))
			},
			body: `{"version":1}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 403, owner: 200},
		},
		{
			name: "confirm as non-payer owner", opID: "POST /v1/groups/{id}/expenses/{expenseId}/confirm", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				return confirmPath(f.groupA, f.needsReviewBy(t, member))
			},
			body: `{"version":1}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 403},
		},
		{
			name: "confirm B's expense through A's path", opID: "POST /v1/groups/{id}/expenses/{expenseId}/confirm", method: "POST", idempotent: true,
			path: func(f *fixture) string { return confirmPath(f.groupA, f.expB) },
			body: `{"version":1}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			name: "confirm A's expense through B's path", opID: "POST /v1/groups/{id}/expenses/{expenseId}/confirm", method: "POST", idempotent: true,
			path: func(f *fixture) string { return confirmPath(f.groupB, f.expA) },
			body: `{"version":1}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			name: "finalize B's expense through A's path", opID: "POST /v1/groups/{id}/expenses/{expenseId}/finalize", method: "POST", idempotent: true,
			path: func(f *fixture) string { return finalizePath(f.groupA, f.expB) },
			body: `{}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			name: "finalize A's expense through B's path", opID: "POST /v1/groups/{id}/expenses/{expenseId}/finalize", method: "POST", idempotent: true,
			path: func(f *fixture) string { return finalizePath(f.groupB, f.expA) },
			body: `{}`,
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// The caller is always the sender. `member` owes `owner` (ensureDebt), so member
			// paying owner is allowed; owner paying member is authorized but refused by the
			// business rule (the owner is owed money, so cannot send any): 409, not 403.
			name: "create settlement", opID: "POST /v1/groups/{id}/settlements", method: "POST", idempotent: true,
			prepare: func(t *testing.T, f *fixture) string {
				f.ensureDebt(t)
				return "/v1/groups/" + f.groupA + "/settlements"
			},
			bodyFor: func(f *fixture, a actor) string {
				to := f.ownerID
				if a == owner {
					to = f.memberID
				}
				return `{"to_member_id":"` + to + `","amount_minor":1}`
			},
			want: map[actor]int{anon: 401, outsider: 404, member: 202, owner: 409},
		},
		{
			// Paying a user who is not in the group is refused for everyone who is in it.
			name: "create settlement to a user outside the group", opID: "POST /v1/groups/{id}/settlements", method: "POST", idempotent: true,
			path:   groupPath("/settlements"),
			bodyFn: func(f *fixture) string { return `{"to_member_id":"` + f.outsiderID + `","amount_minor":1}` },
			want:   map[actor]int{anon: 401, outsider: 404, member: 400, owner: 400},
		},
		{
			name: "list settlements", opID: "GET /v1/groups/{id}/settlements", method: "GET", path: groupPath("/settlements"),
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			name: "read settlement", opID: "GET /v1/groups/{id}/settlements/{settlementId}", method: "GET",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupA + "/settlements/" + f.settlementA },
			want: map[actor]int{anon: 401, outsider: 404, member: 200, owner: 200},
		},
		{
			// Authorized on A, but the id is B's settlement: it must match nothing.
			name: "read B's settlement through A's path", opID: "GET /v1/groups/{id}/settlements/{settlementId}", method: "GET",
			prepare: func(t *testing.T, f *fixture) string {
				return "/v1/groups/" + f.groupA + "/settlements/" + f.ensureSettlementB(t)
			},
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
		{
			// B's owner is authorized on B but names A's settlement.
			name: "read A's settlement through B's path", opID: "GET /v1/groups/{id}/settlements/{settlementId}", method: "GET",
			path: func(f *fixture) string { return "/v1/groups/" + f.groupB + "/settlements/" + f.settlementA },
			want: map[actor]int{anon: 401, outsider: 404, member: 404, owner: 404},
		},
	}
}

const editBody = `{"version":1,"tip_minor":1}`

func expensePath(group, expense string) string {
	return "/v1/groups/" + group + "/expenses/" + expense
}

func confirmPath(group, expense string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/confirm"
}

// needsReviewBy has `payer` create an expense in group A and puts it in needs_review
// (in production the receipt parse job does this; the test sets the status directly).
func (f *fixture) needsReviewBy(t *testing.T, payer actor) string {
	t.Helper()
	id := f.s.CreateExpense(t, f.tokens[payer], f.groupA, expenseBody)
	f.s.Exec(t, `UPDATE expenses SET status = 'needs_review' WHERE id = $1`, id)
	return id
}

func voidPath(group, expense string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/void"
}

func finalizePath(group, expense string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/finalize"
}

func claimPath(group, expense, item string) string {
	return "/v1/groups/" + group + "/expenses/" + expense + "/items/" + item + "/claim"
}

// exempt lists authenticated operations with no per-object authorization to
// test, each with the reason. They still must be authenticated; see
// TestExemptOperationsRequireAuthentication.
var exempt = map[string]string{
	"GET /v1/me":      "takes no id and returns only the caller's own record",
	"GET /v1/groups":  "takes no id; results are scoped to the caller (see TestListIsolation)",
	"POST /v1/groups": "takes no id; creates a resource owned by the caller",
	"POST /v1/join":   "authorization is possession of the join code, not an object id (see internal/groups tests)",
}

func TestMatrix(t *testing.T) {
	f := newFixture(t)
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			for _, a := range actorOrder {
				want := sc.want[a]
				if want == 0 {
					continue
				}
				path := ""
				if sc.prepare != nil {
					path = sc.prepare(t, f)
				} else {
					path = sc.path(f)
				}

				before := f.state(t)
				body := sc.body
				if sc.bodyFn != nil {
					body = sc.bodyFn(f)
				}
				if sc.bodyFor != nil {
					body = sc.bodyFor(f, a)
				}
				var headers []string
				if sc.idempotent {
					headers = []string{"Idempotency-Key", uuid.NewString()}
				}
				if sc.contentType != "" {
					headers = append(headers, "Content-Type", sc.contentType)
				}
				r := f.s.Do(t, f.tokens[a], sc.method, path, body, headers...)
				if r.Status != want {
					t.Errorf("%s as %s: %s %s = %d, want %d: %s", sc.name, a, sc.method, path, r.Status, want, r.Body)
					continue
				}
				if r.Status >= 400 {
					if after := f.state(t); after != before {
						t.Errorf("%s as %s was refused (%d) but changed state:\n before: %s\n after:  %s", sc.name, a, r.Status, before, after)
					}
				}
			}
		})
	}
}

// A 404 for "exists but not yours" must be byte-identical to a 404 for "does
// not exist", or ids can be probed.
func TestNotFoundDoesNotLeakExistence(t *testing.T) {
	f := newFixture(t)
	for _, sc := range scenarios() {
		if sc.prepare != nil || sc.want[outsider] != 404 || !strings.Contains(sc.path(f), f.groupA) {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			real := f.s.Do(t, f.tokens[outsider], sc.method, sc.path(f), sc.body)
			ghost := f.s.Do(t, f.tokens[outsider], sc.method, strings.ReplaceAll(sc.path(f), f.groupA, uuid.NewString()), sc.body)
			if real.Status != ghost.Status || !bytes.Equal(real.Body, ghost.Body) {
				t.Fatalf("existing-but-foreign vs nonexistent differ:\n%d %s\n%d %s", real.Status, real.Body, ghost.Status, ghost.Body)
			}
		})
	}
}

// Using a real expense id that belongs to a group the caller cannot reach must
// look exactly like using an id that does not exist, for the same caller on the
// same path.
func TestForeignExpenseIdIsIndistinguishableFromAGhost(t *testing.T) {
	f := newFixture(t)
	for _, a := range []actor{outsider, member, owner} {
		path := func(id string) string { return "/v1/groups/" + f.groupA + "/expenses/" + id }
		foreign := f.s.Do(t, f.tokens[a], "GET", path(f.expB), "")
		ghost := f.s.Do(t, f.tokens[a], "GET", path(uuid.NewString()), "")
		if foreign.Status != 404 || foreign.Status != ghost.Status || !bytes.Equal(foreign.Body, ghost.Body) {
			t.Errorf("%s: foreign expense id vs nonexistent differ:\n%d %s\n%d %s", a, foreign.Status, foreign.Body, ghost.Status, ghost.Body)
		}
	}
	// And the same for a non-member who names a real expense of a real group:
	// identical to a ghost group and ghost expense.
	real := f.s.Do(t, f.tokens[outsider], "GET", "/v1/groups/"+f.groupA+"/expenses/"+f.expA, "")
	ghost := f.s.Do(t, f.tokens[outsider], "GET", "/v1/groups/"+uuid.NewString()+"/expenses/"+uuid.NewString(), "")
	if real.Status != 404 || real.Status != ghost.Status || !bytes.Equal(real.Body, ghost.Body) {
		t.Errorf("non-member on a real expense vs all-ghost ids differ:\n%d %s\n%d %s", real.Status, real.Body, ghost.Status, ghost.Body)
	}
}

// An expense list must only ever contain the group's own expenses.
func TestExpenseListingIsScopedToItsGroup(t *testing.T) {
	f := newFixture(t)
	r := f.s.Do(t, f.tokens[owner], "GET", "/v1/groups/"+f.groupA+"/expenses", "")
	if r.Status != 200 {
		t.Fatalf("list = %d", r.Status)
	}
	if !strings.Contains(string(r.Body), f.expA) {
		t.Errorf("A's expense list is missing A's expense: %s", r.Body)
	}
	if strings.Contains(string(r.Body), f.expB) {
		t.Errorf("A's expense list contains B's expense id: %s", r.Body)
	}
}

// The sharpest BOLA case: a user who IS a member of both groups. Membership
// proves nothing here; only scoping by the group in the path separates them.
func TestMemberOfTwoGroupsCannotCrossThem(t *testing.T) {
	f := newFixture(t)
	_, codeB := f.s.CreateJoinCode(t, f.tokens[outsider], f.groupB, 0, 0)
	if r := f.s.Join(t, f.tokens[member], codeB); r.Status != http.StatusCreated {
		t.Fatalf("join B = %d: %s", r.Status, r.Body)
	}
	get := func(group, expense string) int {
		return f.s.Do(t, f.tokens[member], "GET", "/v1/groups/"+group+"/expenses/"+expense, "").Status
	}
	if got := get(f.groupA, f.expA); got != 200 {
		t.Errorf("A's expense via A = %d, want 200", got)
	}
	if got := get(f.groupB, f.expB); got != 200 {
		t.Errorf("B's expense via B = %d, want 200", got)
	}
	if got := get(f.groupA, f.expB); got != 404 {
		t.Errorf("B's expense via A's path = %d, want 404", got)
	}
	if got := get(f.groupB, f.expA); got != 404 {
		t.Errorf("A's expense via B's path = %d, want 404", got)
	}
	// Claiming has the same property: the member may claim in either group, but
	// only through the path of the group that owns the expense and item.
	claim := func(group, expense, item string) int {
		return f.s.Do(t, f.tokens[member], "PUT", claimPath(group, expense, item), "").Status
	}
	if got := claim(f.groupB, f.expB, f.itemB); got != 200 {
		t.Errorf("claim B's item via B = %d, want 200", got)
	}
	if got := claim(f.groupA, f.expB, f.itemB); got != 404 {
		t.Errorf("claim B's item via A's path = %d, want 404", got)
	}
	if got := claim(f.groupB, f.expA, f.itemA); got != 404 {
		t.Errorf("claim A's item via B's path = %d, want 404", got)
	}
	if n := f.s.QueryInt(t, `SELECT count(*) FROM claims WHERE expense_id = $1`, f.expA); n != 0 {
		t.Errorf("a cross-group claim created %d claims on A's expense", n)
	}

	// Money: finalize one expense in each group so this member owes something in
	// both, then check each group's balances show only that group's ledger.
	owedInA := f.finalizeWithMemberClaim(t, owner, f.groupA, expenseBody)
	owedInB := f.finalizeWithMemberClaim(t, outsider, f.groupB, strings.Replace(expenseBody, `"price_minor":1500`, `"price_minor":2500`, 1))
	if owedInA == 0 || owedInB == 0 || owedInA == owedInB {
		t.Fatalf("test needs two different nonzero amounts, got %d and %d", owedInA, owedInB)
	}
	type balances struct {
		Balances []struct {
			MemberID     string `json:"member_id"`
			BalanceMinor int64  `json:"balance_minor"`
		} `json:"balances"`
		Transfers []struct {
			From, To string
			Amount   int64 `json:"amount_minor"`
		} `json:"suggested_transfers"`
	}
	read := func(group string) (mine int64, ids []string) {
		var b balances
		r := f.s.Do(t, f.tokens[member], "GET", "/v1/groups/"+group+"/balances", "")
		if r.Status != 200 {
			t.Fatalf("balances of %s = %d: %s", group, r.Status, r.Body)
		}
		r.JSON(t, &b)
		for _, x := range b.Balances {
			ids = append(ids, x.MemberID)
			if x.MemberID == f.memberID {
				mine = x.BalanceMinor
			}
		}
		return mine, ids
	}
	mineA, idsA := read(f.groupA)
	mineB, idsB := read(f.groupB)
	if mineA != -owedInA || mineB != -owedInB {
		t.Errorf("member's balances = %d in A and %d in B, want %d and %d (each group shows only its own ledger)", mineA, mineB, -owedInA, -owedInB)
	}
	outsiderID := f.s.UserID(t, f.tokens[outsider]).String()
	slices.Sort(idsA)
	slices.Sort(idsB)
	wantA := []string{f.ownerID, f.memberID}
	wantB := []string{outsiderID, f.memberID}
	slices.Sort(wantA)
	slices.Sort(wantB)
	if !slices.Equal(idsA, wantA) || !slices.Equal(idsB, wantB) {
		t.Errorf("each group must list exactly its own members:\n A: %v want %v\n B: %v want %v", idsA, wantA, idsB, wantB)
	}
	for group, other := range map[string]string{f.groupA: f.expB, f.groupB: f.expA} {
		r := f.s.Do(t, f.tokens[member], "GET", "/v1/groups/"+group+"/expenses", "")
		if r.Status != 200 || strings.Contains(string(r.Body), other) {
			t.Errorf("list of %s = %d and mentions the other group's expense %s: %s", group, r.Status, other, r.Body)
		}
	}
}

func TestListIsolation(t *testing.T) {
	f := newFixture(t)
	type listed struct {
		Groups []struct{ ID string } `json:"groups"`
	}
	ids := func(a actor) []string {
		var l listed
		f.s.Do(t, f.tokens[a], "GET", "/v1/groups", "").JSON(t, &l)
		var out []string
		for _, g := range l.Groups {
			out = append(out, g.ID)
		}
		return out
	}
	contains := func(xs []string, x string) bool {
		for _, v := range xs {
			if v == x {
				return true
			}
		}
		return false
	}
	if got := ids(outsider); contains(got, f.groupA) || !contains(got, f.groupB) {
		t.Errorf("outsider list = %v, want B but never A", got)
	}
	if got := ids(member); !contains(got, f.groupA) || contains(got, f.groupB) {
		t.Errorf("member list = %v, want A but never B", got)
	}
	if got := ids(owner); !contains(got, f.groupA) || contains(got, f.groupB) {
		t.Errorf("owner list = %v, want A but never B", got)
	}
}

// A member of A must not see B's join codes through any listing either.
func TestJoinCodeListingIsScopedToItsGroup(t *testing.T) {
	f := newFixture(t)
	r := f.s.Do(t, f.tokens[owner], "GET", "/v1/groups/"+f.groupA+"/join-codes", "")
	if r.Status != 200 {
		t.Fatalf("list = %d", r.Status)
	}
	if strings.Contains(string(r.Body), f.codeB) {
		t.Fatalf("A's code list contains B's code id: %s", r.Body)
	}
}

func TestExemptOperationsRequireAuthentication(t *testing.T) {
	f := newFixture(t)
	for op := range exempt {
		method, path, _ := strings.Cut(op, " ")
		if r := f.s.Do(t, "", method, path, `{}`); r.Status != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", op, r.Status)
		}
	}
}

// TestEveryOperationIsCovered cross-checks this suite against the OpenAPI spec.
func TestEveryOperationIsCovered(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Security []any                           `yaml:"security"`
		Paths    map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Security) == 0 {
		t.Fatal("spec has no global security requirement; this guard assumes authentication is the default")
	}

	authenticated := map[string]bool{}
	public := map[string]bool{}
	for path, item := range doc.Paths {
		for method, node := range item {
			switch method {
			case "get", "post", "put", "patch", "delete":
			default:
				continue // "parameters" and friends
			}
			var op struct {
				Security *[]any `yaml:"security"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", method, path, err)
			}
			if op.Security != nil && len(*op.Security) == 0 {
				public[strings.ToUpper(method)+" "+path] = true
				continue // explicitly public
			}
			authenticated[strings.ToUpper(method)+" "+path] = true
		}
	}

	covered := map[string]bool{}
	for _, sc := range scenarios() {
		covered[sc.opID] = true
	}
	var problems []string
	for op := range authenticated {
		if !covered[op] && exempt[op] == "" {
			problems = append(problems, "authenticated operation has no BOLA scenario or exemption: "+op)
		}
		if covered[op] && exempt[op] != "" {
			problems = append(problems, "operation is both covered and exempt: "+op)
		}
	}
	for op := range covered {
		if !authenticated[op] {
			problems = append(problems, "scenario names an operation that is not an authenticated route in the spec: "+op)
		}
	}
	for op := range exempt {
		if !authenticated[op] {
			problems = append(problems, "exemption names an operation that is not an authenticated route in the spec: "+op)
		}
	}
	// A route without user authentication is a decision, not a default: the list is
	// fixed here, and adding one means changing it on purpose. (The webhook is tested
	// in internal/settlements: it authenticates by HMAC signature instead.)
	allowedPublic := map[string]string{
		"GET /healthz":               "health check",
		"GET /readyz":                "readiness check",
		"POST /v1/webhooks/payments": "payment-provider webhook, authenticated by signature",
	}
	for op := range public {
		if allowedPublic[op] == "" {
			problems = append(problems, "route without user authentication is not on the allowed list: "+op)
		}
	}
	for op := range allowedPublic {
		if !public[op] {
			problems = append(problems, "allowed public route is missing from the spec or is not marked public: "+op)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// A user who is a member of both groups reads settlements only through the path of
// the group that owns them, and each group's list holds only its own.
func TestSettlementsAreScopedToTheirGroup(t *testing.T) {
	f := newFixture(t)
	sB := f.ensureSettlementB(t)
	_, codeB := f.s.CreateJoinCode(t, f.tokens[outsider], f.groupB, 0, 0)
	if r := f.s.Join(t, f.tokens[member], codeB); r.Status != http.StatusCreated {
		t.Fatalf("join B = %d: %s", r.Status, r.Body)
	}
	read := func(group, settlement string) int {
		return f.s.Do(t, f.tokens[member], "GET", "/v1/groups/"+group+"/settlements/"+settlement, "").Status
	}
	for name, tc := range map[string]struct {
		group, settlement string
		want              int
	}{
		"A's settlement via A": {f.groupA, f.settlementA, 200},
		"B's settlement via B": {f.groupB, sB, 200},
		"B's settlement via A": {f.groupA, sB, 404},
		"A's settlement via B": {f.groupB, f.settlementA, 404},
	} {
		if got := read(tc.group, tc.settlement); got != tc.want {
			t.Errorf("%s = %d, want %d", name, got, tc.want)
		}
	}
	list := func(group string) string {
		r := f.s.Do(t, f.tokens[member], "GET", "/v1/groups/"+group+"/settlements", "")
		if r.Status != 200 {
			t.Fatalf("list %s = %d", group, r.Status)
		}
		return string(r.Body)
	}
	if a := list(f.groupA); !strings.Contains(a, f.settlementA) || strings.Contains(a, sB) {
		t.Errorf("A's list is wrong: %s", a)
	}
	if b := list(f.groupB); !strings.Contains(b, sB) || strings.Contains(b, f.settlementA) {
		t.Errorf("B's list is wrong: %s", b)
	}
}
