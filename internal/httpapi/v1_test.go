package httpapi_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/auth/authtest"
	"github.com/riyne/fifty-fifty/internal/balances"
	"github.com/riyne/fifty-fifty/internal/expenses"
	"github.com/riyne/fifty-fifty/internal/groups"
	"github.com/riyne/fifty-fifty/internal/harness/teststack"
	"github.com/riyne/fifty-fifty/internal/httpapi"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

// Integration tests for authentication and the basic group endpoints. See
// internal/harness/teststack for what "integration" means here.

// countUsers counts users recorded under this stack's issuer URL. The URL is a
// random local port, and the test database is shared and never cleaned, so a port
// reused from an earlier run already has rows under it: tests must compare
// against a baseline taken after the stack starts, never against an absolute number.
func countUsers(t *testing.T, s *teststack.Stack) int {
	t.Helper()
	return s.QueryInt(t, `SELECT count(*) FROM users WHERE issuer = $1`, s.Issuer.URL)
}

func TestV1_RequiresValidToken(t *testing.T) {
	s := teststack.New(t)
	usersBefore := countUsers(t, s)
	other := authtest.NewIssuer(t)
	bad := map[string]string{
		"no token":         "",
		"garbage":          "nope",
		"expired":          s.Issuer.Mint(t, authtest.Token{Audience: teststack.ClientID, TTL: -time.Hour}),
		"wrong audience":   s.Issuer.Mint(t, authtest.Token{Audience: "other-client"}),
		"unknown issuer":   other.Mint(t, authtest.Token{Audience: teststack.ClientID}),
		"forged signature": other.Mint(t, authtest.Token{Audience: teststack.ClientID, Issuer: s.Issuer.URL}),
	}
	// Every /v1 route, including ones that do not exist, answers 401 before anything else.
	paths := []struct{ method, path string }{
		{"GET", "/v1/me"}, {"GET", "/v1/groups"}, {"POST", "/v1/groups"}, {"POST", "/v1/join"},
		{"GET", "/v1/groups/00000000-0000-0000-0000-000000000000"}, {"GET", "/v1/no-such-route"},
		{"POST", "/v1/groups/00000000-0000-0000-0000-000000000000/receipts"},
	}
	for name, tok := range bad {
		for _, p := range paths {
			if r := s.Do(t, tok, p.method, p.path, `{}`); r.Status != http.StatusUnauthorized {
				t.Errorf("%s: %s %s = %d, want 401", name, p.method, p.path, r.Status)
			}
		}
	}
	if n := countUsers(t, s) - usersBefore; n != 0 {
		t.Fatalf("rejected tokens created %d users, want 0", n)
	}
}

func TestV1_UnauthenticatedHasChallengeHeader(t *testing.T) {
	s := teststack.New(t)
	r := s.Do(t, "", "GET", "/v1/me", "")
	if r.Status != 401 || r.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("got %d / %q", r.Status, r.Header.Get("WWW-Authenticate"))
	}
}

func TestMe_ProvisionsOnFirstTokenAndIsStable(t *testing.T) {
	s := teststack.New(t)
	tok := s.Token(t, "alice")

	r := s.Do(t, tok, "GET", "/v1/me", "")
	if r.Status != 200 {
		t.Fatalf("first /v1/me = %d: %s", r.Status, r.Body)
	}
	var first map[string]string
	r.JSON(t, &first)
	if first["email"] != "alice@example.com" || first["display_name"] != "alice" {
		t.Fatalf("unexpected profile: %v", first)
	}

	var second map[string]string
	s.Do(t, tok, "GET", "/v1/me", "").JSON(t, &second)
	if second["id"] != first["id"] {
		t.Fatalf("user id changed between requests: %s vs %s", first["id"], second["id"])
	}
}

// Many simultaneous first requests for a brand-new identity must converge on
// one user row (this is what EnsureUser's ON CONFLICT is for).
func TestMe_ConcurrentFirstRequestsCreateOneUser(t *testing.T) {
	s := teststack.New(t)
	usersBefore := countUsers(t, s)
	tok := s.Token(t, "racer")

	const n = 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.Do(t, tok, "GET", "/v1/me", "")
			if r.Status != 200 {
				t.Errorf("request %d = %d: %s", i, r.Status, r.Body)
				return
			}
			var me map[string]string
			r.JSON(t, &me)
			ids[i] = me["id"]
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent first requests produced different users: %v", ids)
		}
	}
	if got := countUsers(t, s) - usersBefore; got != 1 {
		t.Fatalf("concurrent first requests created %d users, want 1", got)
	}
}

type group struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Role     string `json:"role"`
	Members  []struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	} `json:"members"`
}

func TestCreateGroup_Validation(t *testing.T) {
	s := teststack.New(t)
	tok := s.Token(t, "alice")
	long := string(bytes.Repeat([]byte("x"), 101))

	tests := []struct{ name, body string }{
		{"empty name", `{"name":"","currency":"CAD"}`},
		{"blank name", `{"name":"   ","currency":"CAD"}`},
		{"name too long", `{"name":"` + long + `","currency":"CAD"}`},
		{"bad currency", `{"name":"Trip","currency":"EUR"}`},
		{"lowercase currency", `{"name":"Trip","currency":"cad"}`},
		{"missing currency", `{"name":"Trip"}`},
		{"unknown field", `{"name":"Trip","currency":"CAD","role":"admin"}`},
		{"trailing data", `{"name":"Trip","currency":"CAD"} {}`},
		{"not json", `name=Trip`},
		{"empty body", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := s.Do(t, tok, "POST", "/v1/groups", tt.body); r.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Status, r.Body)
			}
		})
	}
}

func TestGroups_CreateListGet(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")

	r := s.Do(t, alice, "POST", "/v1/groups", `{"name":"  Whistler trip ","currency":"CAD"}`)
	if r.Status != http.StatusCreated {
		t.Fatalf("create = %d: %s", r.Status, r.Body)
	}
	var created group
	r.JSON(t, &created)
	if created.Name != "Whistler trip" || created.Currency != "CAD" || created.Role != "owner" {
		t.Fatalf("created = %+v", created)
	}

	var listed struct{ Groups []group }
	s.Do(t, alice, "GET", "/v1/groups", "").JSON(t, &listed)
	if len(listed.Groups) != 1 || listed.Groups[0].ID != created.ID {
		t.Fatalf("list = %+v, want just %s", listed.Groups, created.ID)
	}

	r = s.Do(t, alice, "GET", "/v1/groups/"+created.ID, "")
	if r.Status != 200 {
		t.Fatalf("get = %d: %s", r.Status, r.Body)
	}
	var detail group
	r.JSON(t, &detail)
	if len(detail.Members) != 1 || detail.Members[0].Role != "owner" || detail.Members[0].DisplayName != "alice" {
		t.Fatalf("members = %+v", detail.Members)
	}
}

// A non-member must not be able to tell "exists but not yours" from "does not
// exist": the two responses are byte-identical. The full endpoint-by-actor
// matrix lives in test/bola; this is the focused check for the read path.
func TestGetGroup_NonMemberIsIndistinguishableFromMissing(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	mallory := s.Token(t, "mallory")
	id := s.CreateGroup(t, alice, "Private")

	forbidden := s.Do(t, mallory, "GET", "/v1/groups/"+id, "")
	if forbidden.Status != http.StatusNotFound {
		t.Fatalf("non-member get = %d, want 404", forbidden.Status)
	}
	missing := s.Do(t, mallory, "GET", "/v1/groups/00000000-0000-4000-8000-000000000000", "")
	if !bytes.Equal(forbidden.Body, missing.Body) {
		t.Fatalf("non-member and missing responses differ:\n%s\n%s", forbidden.Body, missing.Body)
	}
	if r := s.Do(t, mallory, "GET", "/v1/groups/not-a-uuid", ""); r.Status != http.StatusNotFound {
		t.Fatalf("malformed id = %d, want 404", r.Status)
	}

	var listed struct{ Groups []group }
	s.Do(t, mallory, "GET", "/v1/groups", "").JSON(t, &listed)
	if len(listed.Groups) != 0 {
		t.Fatalf("mallory sees groups: %+v", listed.Groups)
	}
}

// Without a receipt handler (no receipt directory configured) the upload route does not
// exist: 404, never an upload that goes nowhere. With one, the route is mounted.
func TestNewV1_ReceiptRouteExistsOnlyWithAHandler(t *testing.T) {
	pass := func(h http.Handler) http.Handler { return h }
	req := func(v1 http.Handler) int {
		w := httptest.NewRecorder()
		v1.ServeHTTP(w, httptest.NewRequest("POST", "/v1/groups/00000000-0000-0000-0000-000000000000/receipts", nil))
		return w.Code
	}
	without := httpapi.NewV1(pass, groups.NewHandler(nil, nil), expenses.NewHandler(nil), balances.NewHandler(nil), settlements.NewHandler(nil), nil)
	if got := req(without); got != http.StatusNotFound {
		t.Errorf("without a receipt handler: %d, want 404", got)
	}
	store, err := objstore.NewFS(filepath.Join(t.TempDir(), "r"))
	if err != nil {
		t.Fatal(err)
	}
	with := httpapi.NewV1(pass, groups.NewHandler(nil, nil), expenses.NewHandler(nil), balances.NewHandler(nil), settlements.NewHandler(nil),
		receiptapi.NewHandler(nil, store, ratelimit.New(rate.Inf, 1), receiptapi.Config{}))
	// With a handler the route is mounted: the request reaches it. It has no database
	// here, so reaching it may panic (recovered); only "no such route" is a failure.
	mounted := func() (m bool) {
		defer func() {
			if recover() != nil {
				m = true
			}
		}()
		got := req(with)
		return got != http.StatusNotFound && got != http.StatusMethodNotAllowed
	}()
	if !mounted {
		t.Error("with a receipt handler the route is not mounted")
	}
}
