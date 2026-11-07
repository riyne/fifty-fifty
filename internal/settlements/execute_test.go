package settlements_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/payclient"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// ---- rig: a settlement in the database, a mock provider, and a way to run jobs ----

type rig struct {
	*world
	mock   *mockpay.Server
	client *payclient.Client
	kind   string // a kind of this test's own: the shared queue holds other tests' jobs
}

func newRig(t *testing.T, cfg mockpay.Config) *rig {
	t.Helper()
	if cfg.TimeoutHold == 0 {
		cfg.TimeoutHold = 30 * time.Millisecond
	}
	m, err := mockpay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(m.Handler())
	t.Cleanup(func() { ts.Close(); m.Close() })
	// No keep-alive, so Go's transparent retry of a request that carries an
	// Idempotency-Key cannot add provider calls the test did not plan.
	hc := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	r := &rig{
		world:  newWorld(t),
		mock:   m,
		client: &payclient.Client{BaseURL: ts.URL, HTTP: hc},
		kind:   "test.settle." + uuid.NewString(),
	}
	// The handlers enqueue jobs of the real settlement kinds (the reconcile job, the
	// watchdog's execute job). Nothing in these tests runs them, so remove them with
	// the test instead of leaving them for a real worker to find.
	t.Cleanup(func() {
		_, _ = r.owner.Exec(ctx, `DELETE FROM jobs WHERE kind IN ('settlement.execute', 'settlement.reconcile')
		                         AND payload->>'settlement_id' IN (SELECT id::text FROM settlements WHERE group_id = $1)`, r.group)
		_, _ = r.owner.Exec(ctx, `DELETE FROM jobs WHERE kind = $1`, r.kind)
	})
	return r
}

func (r *rig) enqueue(t *testing.T, id uuid.UUID, maxAttempts int) uuid.UUID {
	t.Helper()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	job, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: r.kind, Payload: settlements.ExecutePayload{SettlementID: id}, MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return job
}

// run starts a fast worker for the rig's kind with handler h. It returns a function that
// stops the worker and waits for it (also run at cleanup). A test that needs two handlers
// on the rig's kind must stop the first before starting the second: two workers on one
// kind would split the jobs between the wrong handlers.
func (r *rig) run(t *testing.T, h worker.Handler) (stop func()) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(runCtx, worker.Config{
			Pool: r.pool, Handlers: map[string]worker.Handler{r.kind: h},
			Concurrency: 4, PollInterval: 10 * time.Millisecond, Lease: 500 * time.Millisecond,
			ReapInterval: time.Hour, ShutdownGrace: 2 * time.Second, // the reaper is global: these tests do not need it and must not touch other packages' jobs
			Backoff: queue.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

type jobInfo struct {
	state, lastError string
	attempts         int
}

func (r *rig) job(t *testing.T, id uuid.UUID) jobInfo {
	t.Helper()
	var j jobInfo
	var le *string
	if err := r.owner.QueryRow(ctx, `SELECT state, attempts, last_error FROM jobs WHERE id = $1`, id).Scan(&j.state, &j.attempts, &le); err != nil {
		t.Fatal(err)
	}
	if le != nil {
		j.lastError = *le
	}
	return j
}

// waitJob waits until the job is in one of the states.
func (r *rig) waitJob(t *testing.T, id uuid.UUID, states ...string) jobInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j := r.job(t, id)
		for _, s := range states {
			if j.state == s {
				return j
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %v; now %+v", id, states, r.job(t, id))
	return jobInfo{}
}

// reconcileJob returns the payload and whether it is scheduled for the future, for the
// settlement's reconcile job; found is false when there is none.
func (r *rig) reconcileJobs(t *testing.T, id uuid.UUID) (n int, payload string, future bool) {
	t.Helper()
	rows, err := r.owner.Query(ctx,
		`SELECT payload::text, run_at > now() FROM jobs WHERE kind = $1 AND payload->>'settlement_id' = $2`, settlements.KindReconcile, id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		n++
		if err := rows.Scan(&payload, &future); err != nil {
			t.Fatal(err)
		}
	}
	return n, payload, future
}

func (r *rig) executeHandler(p settlements.Provider) worker.Handler {
	return settlements.ExecuteHandler(r.pool, p)
}

// countingProvider counts the calls that reach the provider.
type countingProvider struct {
	inner settlements.Provider
	calls atomic.Int32
}

func (c *countingProvider) Create(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error) {
	c.calls.Add(1)
	return c.inner.Create(ctx, key, r)
}

// ---- outcomes through a real worker ----

func TestExecute_SuccessPostsOnceAndKeysTheCallBySettlementID(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.settlement(t, 700)
	job := r.enqueue(t, id, 0)
	r.run(t, r.executeHandler(r.client))
	j := r.waitJob(t, job, "succeeded", "dead")
	if j.state != "succeeded" || j.attempts != 1 {
		t.Fatalf("job = %+v", j)
	}

	if row := r.row(t, id); row.status != "succeeded" || row.ledgerTx == nil || !row.resolved {
		t.Errorf("settlement = %+v", row)
	}
	if got := r.balances(t); len(got) != 2 || got[r.from] != 700 || got[r.to] != -700 {
		t.Errorf("balances = %v", got)
	}
	if r.postings(t, id) != 1 {
		t.Errorf("postings = %d, want 1", r.postings(t, id))
	}
	pays := r.mock.Payments()
	if len(pays) != 1 {
		t.Fatalf("provider holds %d payments", len(pays))
	}
	p := pays[0]
	if p.IdempotencyKey != id.String() || p.Reference != id.String() || p.From != r.from.String() || p.To != r.to.String() ||
		p.AmountMinor != 700 || p.Currency != "CAD" || p.Status != "succeeded" {
		t.Errorf("provider payment = %+v: the key and reference must be the settlement id", p.Payment)
	}
}

func TestExecute_DeclineFailsTheSettlementWithoutPosting(t *testing.T) {
	r := newRig(t, mockpay.Config{RateDecline: 1})
	id := r.settlement(t, 700)
	job := r.enqueue(t, id, 0)
	r.run(t, r.executeHandler(r.client))
	if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" || j.attempts != 1 {
		t.Fatalf("job = %+v: a decline is an answer, not a job failure", j)
	}
	row := r.row(t, id)
	if row.status != "failed" || row.failureCode == nil || *row.failureCode != mockpay.FailureCode || row.ledgerTx != nil {
		t.Errorf("settlement = %+v", row)
	}
	if r.postings(t, id) != 0 || len(r.balances(t)) != 0 {
		t.Error("a declined settlement posted to the ledger")
	}
	if st := r.mock.Stats(); st.Charges != 0 || st.Declined != 1 {
		t.Errorf("provider stats = %+v", st)
	}
}

// Every way of not knowing becomes `unknown` plus a reconcile job, once, with no
// resend, and no posting. Whether the provider took the payment differs by case; the
// settlement cannot tell, which is the point.
func TestExecute_AmbiguousResultsBecomeUnknownAndAreReconciled(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cfg          mockpay.Config
		wantPayments int // what the provider really holds
	}{
		{"timeout before the provider acted", mockpay.Config{RateTimeoutBefore: 1}, 0},
		{"5xx before the provider acted", mockpay.Config{Rate5xxBefore: 1}, 0},
		{"timeout after the payment was created", mockpay.Config{RateTimeoutAfter: 1}, 1},
		{"5xx after the payment was created", mockpay.Config{Rate5xxAfter: 1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.cfg)
			id := r.settlement(t, 700)
			job := r.enqueue(t, id, 0)
			calls := &countingProvider{inner: r.client}
			r.run(t, r.executeHandler(calls))
			j := r.waitJob(t, job, "succeeded", "dead")
			if j.state != "succeeded" || j.attempts != 1 {
				t.Fatalf("job = %+v: an ambiguous answer must finish the job, not retry the send", j)
			}
			if calls.calls.Load() != 1 {
				t.Errorf("provider called %d times, want 1 (execute never resends)", calls.calls.Load())
			}
			if row := r.row(t, id); row.status != "unknown" || row.ledgerTx != nil || row.resolved {
				t.Errorf("settlement = %+v, want unknown", row)
			}
			if r.postings(t, id) != 0 {
				t.Error("an unknown settlement posted to the ledger")
			}
			if got := len(r.mock.Payments()); got != tc.wantPayments {
				t.Errorf("provider holds %d payments, want %d", got, tc.wantPayments)
			}
			n, payload, future := r.reconcileJobs(t, id)
			if n != 1 || !future || !strings.Contains(payload, `"check": 1`) {
				t.Errorf("reconcile jobs: n=%d future=%v payload=%s, want one job, in the future, check 1", n, future, payload)
			}
			if r.audits(t, "settlement.unknown") != 1 {
				t.Error("no settlement.unknown audit event")
			}
		})
	}
}

// A crash between the provider call and the commit: the attempt is lost, the next one
// calls again with the same key, the provider returns the payment it already has, and
// the ledger is posted exactly once.
func TestExecute_RetryAfterAFailureCallsAgainWithoutChargingTwice(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.settlement(t, 700)
	job := r.enqueue(t, id, 0)

	var first atomic.Bool
	crashy := providerFunc(func(ctx context.Context, key string, req payclient.Request) (payclient.Payment, error) {
		p, err := r.client.Create(ctx, key, req)
		if err == nil && first.CompareAndSwap(false, true) {
			return payclient.Payment{}, errors.New("simulated crash after the provider took the payment")
		}
		return p, err
	})
	r.run(t, r.executeHandler(crashy))
	j := r.waitJob(t, job, "succeeded", "dead")
	if j.state != "succeeded" || j.attempts != 2 {
		t.Fatalf("job = %+v, want succeeded on attempt 2", j)
	}
	st := r.mock.Stats()
	if st.CreateCalls != 2 || st.Payments != 1 || st.Charges != 1 || st.Replays != 1 {
		t.Errorf("provider stats = %+v, want 2 calls, 1 payment, 1 charge", st)
	}
	if r.postings(t, id) != 1 || r.row(t, id).status != "succeeded" {
		t.Error("the settlement was not posted exactly once")
	}
	if got := r.balances(t); got[r.from] != 700 || got[r.to] != -700 {
		t.Errorf("balances = %v", got)
	}
}

type providerFunc func(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error)

func (f providerFunc) Create(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error) {
	return f(ctx, key, r)
}

// A rejected request is our bug: the job retries, then is dead-lettered where an
// operator sees it; the settlement is not declared failed and nothing is posted.
func TestExecute_RejectedRequestIsAJobFailureNotAPaymentOutcome(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.settlement(t, 700)
	job := r.enqueue(t, id, 2)
	rejecting := providerFunc(func(context.Context, string, payclient.Request) (payclient.Payment, error) {
		return payclient.Payment{}, fmt.Errorf("%w: status 422", payclient.ErrRejected)
	})
	r.run(t, r.executeHandler(rejecting))
	j := r.waitJob(t, job, "dead", "succeeded")
	if j.state != "dead" || j.attempts != 2 || !strings.Contains(j.lastError, "rejected") {
		t.Fatalf("job = %+v, want dead after 2 attempts with the reason", j)
	}
	if row := r.row(t, id); row.status != "pending" {
		t.Errorf("settlement = %+v, want pending (an operator decides)", row)
	}
	if n, _, _ := r.reconcileJobs(t, id); n != 0 || r.postings(t, id) != 0 {
		t.Error("a rejected request scheduled reconciliation or posted")
	}
}

// A settlement that is no longer pending is left alone, with no provider call.
func TestExecute_LeavesResolvedSettlementsAlone(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			r := newRig(t, mockpay.Config{})
			id := r.settlement(t, 700)
			switch state {
			case "succeeded":
				r.resolve(t, id, settlements.Success())
			case "failed":
				r.resolve(t, id, settlements.Failure("declined"))
			case "unknown":
				r.resolve(t, id, settlements.AmbiguousResult())
			}
			before := r.postings(t, id)
			job := r.enqueue(t, id, 0)
			calls := &countingProvider{inner: r.client}
			r.run(t, r.executeHandler(calls))
			if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" {
				t.Fatalf("job = %+v", j)
			}
			if calls.calls.Load() != 0 {
				t.Errorf("the provider was called %d times for a %s settlement", calls.calls.Load(), state)
			}
			if got := r.row(t, id).status; got != state {
				t.Errorf("status = %s, want %s", got, state)
			}
			if r.postings(t, id) != before {
				t.Error("postings changed")
			}
			if n, _, _ := r.reconcileJobs(t, id); n != 0 {
				t.Error("a reconcile job was scheduled")
			}
		})
	}
}

func TestExecute_BadJobsAreErrors(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	h := r.executeHandler(r.client)
	for name, payload := range map[string]string{
		"not json":           `nope`,
		"no settlement id":   `{}`,
		"nil settlement id":  `{"settlement_id":"00000000-0000-0000-0000-000000000000"}`,
		"unknown settlement": fmt.Sprintf(`{"settlement_id":"%s"}`, uuid.New()),
	} {
		commit, err := h(ctx, queue.Claimed{Payload: []byte(payload)})
		if err == nil || commit != nil {
			t.Errorf("%s: commit=%v err=%v, want an error and no commit", name, commit != nil, err)
		}
	}
	if st := r.mock.Stats(); st.CreateCalls != 0 {
		t.Errorf("the provider was called %d times for bad jobs", st.CreateCalls)
	}
}

// Our own context ending (lease lost, shutdown) says nothing about the payment: it is
// an error for the worker to handle, never an `unknown` settlement.
func TestExecute_CancellationIsNotAnAmbiguousOutcome(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.settlement(t, 700)
	blocked := make(chan struct{})
	// The real client, against a server that never answers, with the caller's context canceled.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body) // until the body is read the server cannot notice a disconnect
		close(blocked)
		<-req.Context().Done()
	}))
	defer slow.Close()
	c := &payclient.Client{BaseURL: slow.URL, HTTP: &http.Client{Timeout: 10 * time.Second}}

	cctx, cancel := context.WithCancel(ctx)
	go func() { <-blocked; cancel() }()
	commit, err := r.executeHandler(c)(cctx, queue.Claimed{Payload: []byte(fmt.Sprintf(`{"settlement_id":"%s"}`, id))})
	if err == nil || commit != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("commit=%v err=%v, want a context.Canceled error and no commit", commit != nil, err)
	}
	if r.row(t, id).status != "pending" {
		t.Error("a canceled call changed the settlement")
	}
}

// ---- interleavings, driven by hand ----

func claim(t *testing.T, r *rig, lease time.Duration) queue.Claimed {
	t.Helper()
	cs, err := queue.ClaimJobs(ctx, r.pool, queue.ClaimParams{Kinds: []string{r.kind}, Limit: 1, Lease: lease})
	if err != nil || len(cs) != 1 {
		t.Fatalf("claim: %v, %d jobs", err, len(cs))
	}
	return cs[0]
}

// A worker whose lease was taken over runs the provider call and then tries to commit.
// The commit rolls back as a whole: it posts nothing and schedules nothing. The new
// holder's attempt then posts, once.
func TestExecute_AStaleWorkerCannotPost(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.settlement(t, 700)
	job := r.enqueue(t, id, 0)
	h := r.executeHandler(r.client)

	stale := claim(t, r, 100*time.Millisecond)
	staleCommit, err := h(ctx, stale)
	if err != nil || staleCommit == nil {
		t.Fatalf("stale worker's phase one: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // its lease runs out
	fresh := claim(t, r, 10*time.Second)
	if fresh.ID != job || fresh.LeaseToken == stale.LeaseToken {
		t.Fatal("the job was not taken over with a new fencing token")
	}

	if err := queue.CompleteWith(ctx, r.pool, stale, staleCommit); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("stale commit: err = %v, want ErrLeaseLost", err)
	}
	if r.postings(t, id) != 0 || r.row(t, id).status != "pending" {
		t.Fatal("a worker that lost its lease changed the settlement")
	}

	freshCommit, err := h(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteWith(ctx, r.pool, fresh, freshCommit); err != nil {
		t.Fatal(err)
	}
	if r.postings(t, id) != 1 || r.row(t, id).status != "succeeded" {
		t.Error("the new holder did not post exactly once")
	}
	if st := r.mock.Stats(); st.CreateCalls != 2 || st.Payments != 1 || st.Charges != 1 {
		t.Errorf("provider stats = %+v: the second attempt must replay the first's payment", st)
	}
}

// A webhook (or anything else) resolves the settlement while the call is in flight and
// the answer was ambiguous: the commit finds it already resolved, changes nothing, and
// schedules no reconcile.
func TestExecute_AnAmbiguousAnswerLosesToAnEarlierResolution(t *testing.T) {
	r := newRig(t, mockpay.Config{Rate5xxAfter: 1})
	id := r.settlement(t, 700)
	r.enqueue(t, id, 0)
	h := r.executeHandler(r.client)

	c := claim(t, r, 10*time.Second)
	commit, err := h(ctx, c)
	if err != nil || commit == nil {
		t.Fatalf("phase one: %v", err)
	}
	r.resolve(t, id, settlements.Success()) // the webhook gets there first

	if err := queue.CompleteWith(ctx, r.pool, c, commit); err != nil {
		t.Fatal(err)
	}
	if r.row(t, id).status != "succeeded" || r.postings(t, id) != 1 {
		t.Error("the settlement is not succeeded with one posting")
	}
	if n, _, _ := r.reconcileJobs(t, id); n != 0 {
		t.Errorf("%d reconcile jobs were scheduled for a settlement that was already resolved", n)
	}
	if r.audits(t, "settlement.unknown") != 0 {
		t.Error("an unknown event was recorded after the settlement was resolved")
	}
}
