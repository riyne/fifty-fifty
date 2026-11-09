package stressrig

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/auth/authtest"
	"github.com/riyne/fifty-fifty/internal/balances"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/expenses"
	"github.com/riyne/fifty-fifty/internal/groups"
	"github.com/riyne/fifty-fifty/internal/harness/dbrole"
	"github.com/riyne/fifty-fifty/internal/httpapi"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

// ClientID is the audience the rig's verifier requires.
const ClientID = "stress-client"

// API is the real api (the same middleware, verifier, handlers and queries as
// cmd/api) served in this process, with tokens minted by an in-process issuer that the
// unmodified verifier checks. It is the same arrangement as internal/harness/teststack but does
// not need a *testing.T, so a benchmark program can use it. Nothing bypasses
// authentication. It is not cmd/api's wiring: if a handler is added there it must be
// added here (the BOLA suite and teststack have the same obligation).
type API struct {
	URL string

	srv     *httptest.Server
	issuer  *authtest.Issuer
	rigT    *rigT
	closeFn []func()
}

// StartAPI serves the api over pool. webhookSecret, if not empty, mounts the payment
// webhook at settlements.WebhookPath with that signing secret. Join attempts are not
// rate limited (a stress run joins many users in a burst).
func StartAPI(ctx context.Context, pool *pgxpool.Pool, webhookSecret string) (*API, error) {
	a := &API{rigT: &rigT{}}
	if err := a.guard(func(t authtest.T) {
		a.issuer = authtest.NewIssuer(t)
	}); err != nil {
		return nil, err
	}
	verifier, err := auth.NewOIDCVerifier(ctx, a.issuer.URL, "", ClientID)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("verifier: %w", err)
	}
	// The second pool, as cmd/api builds it: provisioning and the webhook run as fifty_system.
	systemPool, err := dbrole.Pool(ctx, pool, dbrole.System, 4)
	if err != nil {
		a.Close()
		return nil, err
	}
	a.closeFn = append(a.closeFn, systemPool.Close)
	// Uploads are mounted as in cmd/api, over a private temporary directory that Close
	// removes; the stress workload does not use them.
	receiptDir, err := os.MkdirTemp("", "stressrig-receipts-")
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("receipt dir: %w", err)
	}
	a.closeFn = append(a.closeFn, func() { _ = os.RemoveAll(receiptDir) })
	store, err := objstore.NewFS(receiptDir)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("receipt store: %w", err)
	}
	receiptHandler := receiptapi.NewHandler(pool, store, ratelimit.New(rate.Inf, 1_000_000), receiptapi.Config{})
	v1 := httpapi.NewV1(
		auth.Middleware(verifier, db.New(systemPool)),
		groups.NewHandler(pool, ratelimit.New(rate.Inf, 1_000_000)),
		expenses.NewHandler(pool),
		balances.NewHandler(pool),
		settlements.NewHandler(pool),
		receiptHandler,
	)
	var webhooks http.Handler
	if webhookSecret != "" {
		wh, err := settlements.NewWebhookHandler(systemPool, []byte(webhookSecret))
		if err != nil {
			a.Close()
			return nil, fmt.Errorf("webhook handler: %w", err)
		}
		webhooks = wh
	}
	a.srv = httptest.NewServer(httpapi.New(pool, v1, webhooks))
	a.URL = a.srv.URL
	return a, nil
}

// Token mints a valid token for a brand-new user. The user row is created by the api on
// first use, as in production.
func (a *API) Token(name string) (string, error) {
	var tok string
	err := a.guard(func(t authtest.T) {
		tok = a.issuer.Mint(t, authtest.Token{
			Audience: ClientID,
			Subject:  name + "-" + uuid.NewString(),
			Email:    name + "@example.com",
			Name:     name,
		})
	})
	return tok, err
}

// Close stops the api and the issuer.
func (a *API) Close() {
	if a.srv != nil {
		a.srv.Close()
	}
	for _, fn := range a.closeFn {
		fn()
	}
	a.closeFn = nil
	a.rigT.run()
}

// guard runs fn with a T whose Fatalf panics, and turns that panic into an error.
func (a *API) guard(fn func(authtest.T)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	fn(a.rigT)
	return nil
}

// rigT is the authtest.T of a program that is not a test.
type rigT struct{ cleanups []func() }

func (*rigT) Helper()                           {}
func (*rigT) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }
func (t *rigT) Cleanup(fn func())               { t.cleanups = append(t.cleanups, fn) }
func (t *rigT) run() {
	for i := len(t.cleanups) - 1; i >= 0; i-- {
		t.cleanups[i]()
	}
	t.cleanups = nil
}
