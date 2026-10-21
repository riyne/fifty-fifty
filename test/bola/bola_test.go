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
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/riyne/fifty-fifty/internal/teststack"
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
	s      *teststack.Stack
	tokens map[actor]string
	groupA string // owned by `owner`, `member` belongs, `outsider` does not
	groupB string // owned by `outsider`
	codeA  string // a join code id in A
	codeB  string // a join code id in B
}

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
	var joinCode string
	f.codeA, joinCode = s.CreateJoinCode(t, f.tokens[owner], f.groupA, 0, 0)
	f.codeB, _ = s.CreateJoinCode(t, f.tokens[outsider], f.groupB, 0, 0)
	if r := s.Join(t, f.tokens[member], joinCode); r.Status != http.StatusCreated {
		t.Fatalf("fixture: member join = %d: %s", r.Status, r.Body)
	}
	return f
}

// state summarizes everything a denied request must not change, for both groups.
func (f *fixture) state(t *testing.T) string {
	t.Helper()
	q := func(sql string) int { return f.s.QueryInt(t, sql, f.groupA, f.groupB) }
	return fmt.Sprintf("codes=%d revoked=%d uses=%d members=%d audit=%d",
		q(`SELECT count(*) FROM join_codes WHERE group_id IN ($1, $2)`),
		q(`SELECT count(revoked_at) FROM join_codes WHERE group_id IN ($1, $2)`),
		q(`SELECT coalesce(sum(uses), 0) FROM join_codes WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM memberships WHERE group_id IN ($1, $2)`),
		q(`SELECT count(*) FROM audit_events WHERE group_id IN ($1, $2)`),
	)
}

// scenario is one request shape tried as each actor.
type scenario struct {
	name   string
	opID   string // "METHOD /path/template", as written in api/openapi.yaml
	method string
	path   func(f *fixture) string
	body   string
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
	}
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
				r := f.s.Do(t, f.tokens[a], sc.method, path, sc.body)
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
				continue // explicitly public (health checks)
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
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
