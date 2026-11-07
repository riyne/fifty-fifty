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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/auth/authtest"
	"github.com/riyne/fifty-fifty/internal/balances"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbrole"
	"github.com/riyne/fifty-fifty/internal/expenses"
	"github.com/riyne/fifty-fifty/internal/groups"
	"github.com/riyne/fifty-fifty/internal/httpapi"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// ClientID is the audience the stack's verifier requires.
const ClientID = "test-client"

type Stack struct {
	Server *httptest.Server
	Issuer *authtest.Issuer
	Pool   *pgxpool.Pool
	// ReceiptDir is the directory of the stack's receipt object store (a fresh
	// temporary directory, removed with the test).
	ReceiptDir string
}

type config struct {
	joinRate      rate.Limit
	joinBurst     int
	webhookSecret string
	receipt       receiptapi.Config
	receiptStore  objstore.Store
	readTimeout   time.Duration
	uploadRate    rate.Limit
	uploadBurst   int
}

type Option func(*config)

// WithJoinLimit overrides the join-attempt rate limit (default: effectively unlimited).
func WithJoinLimit(r rate.Limit, burst int) Option {
	return func(c *config) { c.joinRate, c.joinBurst = r, burst }
}

// WithReceiptConfig overrides the receipt upload settings (daily quota, how many
// uploads may decode at once, image limits).
func WithReceiptConfig(c receiptapi.Config) Option {
	return func(cfg *config) { cfg.receipt = c }
}

// WithReceiptStore replaces the receipt object store (default: a temporary directory),
// for tests that need it to block or fail. Stack.ReceiptDir is then empty.
func WithReceiptStore(s objstore.Store) Option {
	return func(c *config) { c.receiptStore = s }
}

// WithServerReadTimeout sets the http.Server ReadTimeout of the stack (default: none), to
// test routes that extend their own read deadline, as cmd/api's 15 s timeout requires
// of the upload route.
func WithServerReadTimeout(d time.Duration) Option {
	return func(c *config) { c.readTimeout = d }
}

// WithUploadLimit overrides the per-user upload rate limit (default: effectively unlimited).
func WithUploadLimit(r rate.Limit, burst int) Option {
	return func(c *config) { c.uploadRate, c.uploadBurst = r, burst }
}

// WithWebhookSecret enables the payment webhook endpoint with this signing secret
// (without it the route is not mounted, as in a deployment with no secret).
func WithWebhookSecret(secret string) Option {
	return func(c *config) { c.webhookSecret = secret }
}

// New starts the stack on the shared test database. The test is skipped if
// TEST_DATABASE_URL is unset.
func New(t testing.TB, opts ...Option) *Stack {
	t.Helper()
	return NewOn(t, testutil.Pool(t), opts...)
}

// NewOn starts the stack on pool, for tests that bring their own database (the
// end-to-end tests use a scratch one).
func NewOn(t testing.TB, pool *pgxpool.Pool, opts ...Option) *Stack {
	t.Helper()
	cfg := config{joinRate: rate.Inf, joinBurst: 1_000_000, uploadRate: rate.Inf, uploadBurst: 1_000_000}
	for _, o := range opts {
		o(&cfg)
	}
	// The api's second pool, as cmd/api builds it: first-login provisioning and the
	// payment webhook run as fifty_system, the handlers as the pool's own role.
	systemPool := systemPoolFor(t, pool)
	iss := authtest.NewIssuer(t)
	verifier, err := auth.NewOIDCVerifier(context.Background(), iss.URL, "", ClientID)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	var receiptDir string
	var store objstore.Store = cfg.receiptStore
	if store == nil {
		receiptDir = filepath.Join(t.TempDir(), "receipts") // created 0700 by NewFS
		fsStore, err := objstore.NewFS(receiptDir)
		if err != nil {
			t.Fatalf("receipt store: %v", err)
		}
		store = fsStore
	}
	v1 := httpapi.NewV1(
		auth.Middleware(verifier, db.New(systemPool)),
		groups.NewHandler(pool, ratelimit.New(cfg.joinRate, cfg.joinBurst)),
		expenses.NewHandler(pool),
		balances.NewHandler(pool),
		settlements.NewHandler(pool),
		receiptapi.NewHandler(pool, store, ratelimit.New(cfg.uploadRate, cfg.uploadBurst), cfg.receipt),
	)
	var webhooks http.Handler
	if cfg.webhookSecret != "" {
		wh, err := settlements.NewWebhookHandler(systemPool, []byte(cfg.webhookSecret))
		if err != nil {
			t.Fatalf("webhook handler: %v", err)
		}
		webhooks = wh
	}
	srv := httptest.NewUnstartedServer(httpapi.New(pool, v1, webhooks))
	srv.Config.ReadTimeout = cfg.readTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	// An upload enqueues a receipt.parse job, which a real worker pointed at this database
	// would try to run. Receipts and expenses stay (the database is shared and never
	// cleaned), but the jobs this stack's users caused go with the test.
	issuerURL := iss.URL
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE kind = 'receipt.parse' AND payload->>'receipt_id' IN
			(SELECT r.id::text FROM receipts r JOIN users u ON u.id = r.uploader_id WHERE u.issuer = $1)`, issuerURL)
	})
	return &Stack{Server: srv, Issuer: iss, Pool: pool, ReceiptDir: receiptDir}
}

var (
	systemPoolsMu sync.Mutex
	systemPools   = map[string]*pgxpool.Pool{}
)

// systemPoolFor returns the fifty_system pool for pool's database, one per database for
// the whole test process. Tests start many stacks (one test starts a dozen), the shared
// development database allows 100 connections in all, and a pool per stack that is only
// closed when its test ends used them up. It is never closed: the process exit does it,
// and dropping a scratch database ends its connections.
func systemPoolFor(t testing.TB, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	systemPoolsMu.Lock()
	defer systemPoolsMu.Unlock()
	key := pool.Config().ConnString()
	if p, ok := systemPools[key]; ok {
		return p
	}
	p, err := dbrole.Pool(context.Background(), pool, dbrole.System, 4)
	if err != nil {
		t.Fatal(err)
	}
	systemPools[key] = p
	return p
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

// CreateExpense creates an expense in groupID as token's user from a JSON body
// and returns its id. It fails the test on any non-201 response.
func (s *Stack) CreateExpense(t testing.TB, token, groupID, body string) string {
	t.Helper()
	r := s.Do(t, token, "POST", "/v1/groups/"+groupID+"/expenses", body, "Idempotency-Key", uuid.NewString())
	if r.Status != http.StatusCreated {
		t.Fatalf("create expense = %d: %s", r.Status, r.Body)
	}
	var e struct {
		ID string `json:"id"`
	}
	r.JSON(t, &e)
	return e.ID
}
