// Package teststack runs the whole api in-process for integration tests: the
// real middleware, verifier, handlers and queries over a real Postgres, with
// tokens minted by the fake issuer in authtest. Nothing in it bypasses auth.
package teststack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/auth/authtest"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/groups"
	"github.com/riyne/fifty-fifty/internal/httpapi"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// ClientID is the audience the stack's verifier requires.
const ClientID = "test-client"

type Stack struct {
	Server *httptest.Server
	Issuer *authtest.Issuer
	Pool   *pgxpool.Pool
}

type config struct {
	joinRate  rate.Limit
	joinBurst int
}

type Option func(*config)

// WithJoinLimit overrides the join-attempt rate limit (default: effectively unlimited).
func WithJoinLimit(r rate.Limit, burst int) Option {
	return func(c *config) { c.joinRate, c.joinBurst = r, burst }
}

// New starts the stack. The test is skipped if TEST_DATABASE_URL is unset.
func New(t testing.TB, opts ...Option) *Stack {
	t.Helper()
	cfg := config{joinRate: rate.Inf, joinBurst: 1_000_000}
	for _, o := range opts {
		o(&cfg)
	}
	pool := testutil.Pool(t)
	iss := authtest.NewIssuer(t)
	verifier, err := auth.NewOIDCVerifier(context.Background(), iss.URL, "", ClientID)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	v1 := httpapi.NewV1(
		auth.Middleware(verifier, db.New(pool)),
		groups.NewHandler(pool, ratelimit.New(cfg.joinRate, cfg.joinBurst)),
	)
	srv := httptest.NewServer(httpapi.New(pool, v1))
	t.Cleanup(srv.Close)
	return &Stack{Server: srv, Issuer: iss, Pool: pool}
}

// Token mints a valid token for a brand-new user named name.
func (s *Stack) Token(t testing.TB, name string) string {
	t.Helper()
	return s.Issuer.Mint(t, authtest.Token{
		Audience: ClientID,
		Subject:  testutil.UniqueSubject(name),
		Email:    name + "@example.com",
		Name:     name,
	})
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into v, failing the test if it is not valid JSON.
func (r Response) JSON(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
}

// Do sends a request. token may be empty (unauthenticated). headers are
// alternating name, value pairs.
func (s *Stack) Do(t testing.TB, token, method, path, body string, headers ...string) Response {
	t.Helper()
	req, err := http.NewRequest(method, s.Server.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// UserID returns the local user id behind a token (provisioning it if needed).
func (s *Stack) UserID(t testing.TB, token string) uuid.UUID {
	t.Helper()
	var me struct {
		ID uuid.UUID `json:"id"`
	}
	r := s.Do(t, token, "GET", "/v1/me", "")
	if r.Status != 200 {
		t.Fatalf("/v1/me = %d: %s", r.Status, r.Body)
	}
	r.JSON(t, &me)
	return me.ID
}

// CreateGroup creates a group as token's user and returns its id.
func (s *Stack) CreateGroup(t testing.TB, token, name string) string {
	t.Helper()
	r := s.Do(t, token, "POST", "/v1/groups", `{"name":"`+name+`","currency":"CAD"}`)
	if r.Status != http.StatusCreated {
		t.Fatalf("create group = %d: %s", r.Status, r.Body)
	}
	var g struct {
		ID string `json:"id"`
	}
	r.JSON(t, &g)
	return g.ID
}

// CreateJoinCode creates a code in groupID as token's user (must be owner).
// maxUses <= 0 and ttlHours <= 0 use the server defaults.
func (s *Stack) CreateJoinCode(t testing.TB, token, groupID string, maxUses, ttlHours int) (codeID, plaintext string) {
	t.Helper()
	body := map[string]int{}
	if maxUses > 0 {
		body["max_uses"] = maxUses
	}
	if ttlHours > 0 {
		body["expires_in_hours"] = ttlHours
	}
	b, _ := json.Marshal(body)
	r := s.Do(t, token, "POST", "/v1/groups/"+groupID+"/join-codes", string(b))
	if r.Status != http.StatusCreated {
		t.Fatalf("create join code = %d: %s", r.Status, r.Body)
	}
	var c struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	r.JSON(t, &c)
	return c.ID, c.Code
}

// Join redeems code as token's user with a fresh idempotency key.
func (s *Stack) Join(t testing.TB, token, code string) Response {
	t.Helper()
	return s.JoinWithKey(t, token, code, uuid.NewString())
}

func (s *Stack) JoinWithKey(t testing.TB, token, code, key string) Response {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"code": code})
	return s.Do(t, token, "POST", "/v1/join", string(b), "Idempotency-Key", key)
}

// Exec runs a statement directly against the database, for arranging state a
// test cannot reach through the API (e.g. backdating an expiry).
func (s *Stack) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// QueryInt runs a query returning a single integer.
func (s *Stack) QueryInt(t testing.TB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
