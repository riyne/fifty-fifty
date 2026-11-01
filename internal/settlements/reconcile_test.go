package settlements_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/payclient"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

func TestNextReconcileDelay(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 300 * time.Second, 300 * time.Second}
	for done, w := range want {
		if got := settlements.NextReconcileDelay(done); got != w {
			t.Errorf("NextReconcileDelay(%d) = %v, want %v", done, got, w)
		}
	}
	for _, n := range []int{-3, 40, 1_000_000} {
		if d := settlements.NextReconcileDelay(n); d < settlements.FirstReconcileDelay || d > settlements.MaxReconcileDelay {
			t.Errorf("NextReconcileDelay(%d) = %v, outside [first, max]", n, d)
		}
	}
	// The whole schedule: about 35 minutes from the first check to the last.
	total := settlements.FirstReconcileDelay
	for done := 1; done < settlements.MaxReconcileChecks; done++ {
		total += settlements.NextReconcileDelay(done)
	}
	if total < 30*time.Minute || total > 40*time.Minute {
		t.Errorf("the schedule spans %v; the docs say about 35 minutes", total)
	}
}

// ---- reconcile helpers ----

func (r *rig) enqueueReconcile(t *testing.T, id uuid.UUID, check, maxAttempts int) uuid.UUID {
	t.Helper()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	job, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: r.kind, Payload: settlements.ReconcilePayload{SettlementID: id, Check: check}, MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return job
}

// unknownAt makes a settlement `unknown`, and, if holds is not "", makes the provider hold a
// payment for it ("succeeded" or "failed"), as it would after a lost response.
func (r *rig) unknownSettlement(t *testing.T, amount int64, holds string) uuid.UUID {
	t.Helper()
	id := r.settlement(t, amount)
	r.resolve(t, id, settlements.AmbiguousResult())
	if holds != "" {
		p, err := r.client.Create(ctx, id.String(), payclient.Request{
			Reference: id.String(), From: r.from.String(), To: r.to.String(), AmountMinor: amount, Currency: "CAD"})
		if err != nil || p.Status != holds {
			t.Fatalf("arranging the provider's payment: %+v, %v (want status %s)", p, err, holds)
		}
	}
	return id
}

type nextCheck struct {
	check   int
	inSecs  float64
	payload string
}

// nextChecks returns the real-kind reconcile jobs for the settlement.
func (r *rig) nextChecks(t *testing.T, id uuid.UUID) []nextCheck {
	t.Helper()
	rows, err := r.pool.Query(ctx,
		`SELECT (payload->>'check')::int, extract(epoch FROM run_at - now())::float8, payload::text
		 FROM jobs WHERE kind = $1 AND payload->>'settlement_id' = $2 ORDER BY 1`, settlements.KindReconcile, id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []nextCheck
	for rows.Next() {
		var n nextCheck
		if err := rows.Scan(&n.check, &n.inSecs, &n.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// ---- outcomes ----

func TestReconcile_FindsASuccessfulPaymentAndPostsOnce(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.unknownSettlement(t, 700, "succeeded")
	job := r.enqueueReconcile(t, id, 1, 0)
	r.run(t, settlements.ReconcileHandler(r.pool, r.client))
	if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" || j.attempts != 1 {
		t.Fatalf("job = %+v", j)
	}
	if row := r.row(t, id); row.status != "succeeded" || row.ledgerTx == nil {
		t.Errorf("settlement = %+v", row)
	}
	if r.postings(t, id) != 1 {
		t.Errorf("postings = %d", r.postings(t, id))
	}
	if got := r.balances(t); got[r.from] != 700 || got[r.to] != -700 {
		t.Errorf("balances = %v", got)
	}
	// It only read: the one create is the test's own arrangement.
	if st := r.mock.Stats(); st.CreateCalls != 1 || st.Reads != 1 {
		t.Errorf("provider stats = %+v, want 1 create (the setup) and 1 read", st)
	}
	if len(r.nextChecks(t, id)) != 0 {
		t.Error("a next check was scheduled after an answer")
	}
}

func TestReconcile_FindsADeclineAndFailsWithoutPosting(t *testing.T) {
	r := newRig(t, mockpay.Config{RateDecline: 1})
	id := r.unknownSettlement(t, 700, "failed")
	job := r.enqueueReconcile(t, id, 1, 0)
	r.run(t, settlements.ReconcileHandler(r.pool, r.client))
	if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	if row := r.row(t, id); row.status != "failed" || row.failureCode == nil || *row.failureCode != mockpay.FailureCode {
		t.Errorf("settlement = %+v", row)
	}
	if r.postings(t, id) != 0 {
		t.Error("a declined settlement posted")
	}
}

// No answer is not an answer: a failed read, or a missing payment whose re-send also fails,
// schedules the next check, with the delay for how many checks have
// been made, and leave the settlement unknown.
func TestReconcile_NoAnswerSchedulesTheNextCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  mockpay.Config
	}{
		{"the read fails", mockpay.Config{RateReadError: 1}},
		{"the provider has nothing and the re-send fails too", mockpay.Config{Rate5xxBefore: 1}},
	} {
		for _, check := range []int{1, 2, 5, settlements.MaxReconcileChecks - 1} {
			t.Run(fmt.Sprintf("%s, check %d", tc.name, check), func(t *testing.T) {
				r := newRig(t, tc.cfg)
				id := r.unknownSettlement(t, 700, "")
				job := r.enqueueReconcile(t, id, check, 0)
				r.run(t, settlements.ReconcileHandler(r.pool, r.client))
				if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" || j.attempts != 1 {
					t.Fatalf("job = %+v", j)
				}
				if row := r.row(t, id); row.status != "unknown" || r.postings(t, id) != 0 {
					t.Errorf("settlement = %+v, postings %d", row, r.postings(t, id))
				}
				next := r.nextChecks(t, id)
				if len(next) != 1 || next[0].check != check+1 {
					t.Fatalf("next checks = %+v, want exactly check %d", next, check+1)
				}
				want := settlements.NextReconcileDelay(check).Seconds()
				if next[0].inSecs < want-3 || next[0].inSecs > want+1 {
					t.Errorf("next check is %.1f s away, want about %.0f s", next[0].inSecs, want)
				}
			})
		}
	}
}

// After the last check the job fails instead of scheduling another: it retries (each
// retry reads again), then dead-letters with the reason. The settlement stays unknown;
// the dead job is the operator's signal, and requeueing it reads once more.
func TestReconcile_GivesUpLoudlyAndCanBeRequeued(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.unknownSettlement(t, 700, "")
	gw := &toggled{inner: r.client}
	gw.down.Store(true)
	job := r.enqueueReconcile(t, id, settlements.MaxReconcileChecks, 2)
	r.run(t, settlements.ReconcileHandler(r.pool, gw))

	j := r.waitJob(t, job, "dead", "succeeded")
	if j.state != "dead" || j.attempts != 2 || !strings.Contains(j.lastError, "gave up") || !strings.Contains(j.lastError, id.String()) {
		t.Fatalf("job = %+v, want dead after 2 attempts with the reason", j)
	}
	if r.row(t, id).status != "unknown" || len(r.nextChecks(t, id)) != 0 {
		t.Error("the settlement moved or another check was scheduled")
	}
	if gw.gets.Load() != 2 {
		t.Errorf("asked %d times, want 2 (every attempt asks again)", gw.gets.Load())
	}

	// The provider comes back; the operator requeues the dead job.
	gw.down.Store(false)
	if ids, err := queue.Requeue(ctx, r.pool, queue.RequeueParams{IDs: []uuid.UUID{job}}); err != nil || len(ids) != 1 {
		t.Fatalf("requeue: %v %v", ids, err)
	}
	if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" {
		t.Fatalf("requeued job = %+v", j)
	}
	if r.row(t, id).status != "succeeded" || r.postings(t, id) != 1 {
		t.Error("the requeued reconcile did not resolve the settlement exactly once")
	}
}

// The provider has nothing for the key (the original request never reached it): reconcile
// sends the same request again with the same key, and records what comes back.
func TestReconcile_ResendsWhenTheProviderHasNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        mockpay.Config
		wantStatus string
		wantPosted int
	}{
		{"the payment goes through", mockpay.Config{}, "succeeded", 1},
		{"the payment is declined", mockpay.Config{RateDecline: 1}, "failed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.cfg)
			id := r.unknownSettlement(t, 700, "")
			job := r.enqueueReconcile(t, id, 1, 0)
			r.run(t, settlements.ReconcileHandler(r.pool, r.client))
			if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" || j.attempts != 1 {
				t.Fatalf("job = %+v", j)
			}
			if r.row(t, id).status != tc.wantStatus || r.postings(t, id) != tc.wantPosted {
				t.Errorf("settlement %q with %d postings, want %q and %d", r.row(t, id).status, r.postings(t, id), tc.wantStatus, tc.wantPosted)
			}
			st := r.mock.Stats()
			if st.Reads != 1 || st.CreateCalls != 1 || st.Payments != 1 {
				t.Errorf("provider stats = %+v, want 1 read then 1 create", st)
			}
			p := r.mock.Payments()[0]
			if p.IdempotencyKey != id.String() || p.Reference != id.String() || p.From != r.from.String() || p.To != r.to.String() || p.AmountMinor != 700 {
				t.Errorf("the re-send differs from execute's request: %+v", p.Payment)
			}
			if len(r.nextChecks(t, id)) != 0 {
				t.Error("a next check was scheduled after an answer")
			}
		})
	}
}

// The whole story the re-send exists for: execute's request never reaches the provider,
// the settlement goes unknown, reconcile finds nothing and sends it again.
func TestReconcile_RecoversARequestThatNeverReachedTheProvider(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.settlement(t, 700)
	execJob := r.enqueue(t, id, 0)
	lost := providerFunc(func(context.Context, string, payclient.Request) (payclient.Payment, error) {
		return payclient.Payment{}, fmt.Errorf("%w: timeout before the provider acted", payclient.ErrAmbiguous)
	})
	stopExecute := r.run(t, r.executeHandler(lost))
	r.waitJob(t, execJob, "succeeded", "dead")
	stopExecute() // the next worker on this kind runs a different handler
	if r.row(t, id).status != "unknown" || len(r.mock.Payments()) != 0 {
		t.Fatalf("setup: settlement %q, provider holds %d payments", r.row(t, id).status, len(r.mock.Payments()))
	}

	// The reconcile job execute scheduled, run now under this test's kind.
	n := r.nextChecks(t, id)
	if len(n) != 1 || n[0].check != 1 {
		t.Fatalf("execute scheduled %+v", n)
	}
	rec := r.enqueueReconcile(t, id, n[0].check, 0)
	r.run(t, settlements.ReconcileHandler(r.pool, r.client))
	if j := r.waitJob(t, rec, "succeeded", "dead"); j.state != "succeeded" {
		t.Fatalf("reconcile job = %+v", j)
	}
	if r.row(t, id).status != "succeeded" || r.postings(t, id) != 1 {
		t.Error("the settlement was not posted exactly once")
	}
	if st := r.mock.Stats(); st.Payments != 1 || st.Charges != 1 {
		t.Errorf("provider stats = %+v", st)
	}
}

// The reason the re-send is safe: the read says "nothing", but the original request lands
// at the provider before the re-send does. The re-send gets that payment back; the
// provider has one payment and took one charge.
func TestReconcile_AResendCannotDoubleCharge(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.unknownSettlement(t, 700, "succeeded") // the original request has landed
	stale := gateway{
		get: func(context.Context, string) (payclient.Payment, error) {
			return payclient.Payment{}, payclient.ErrNotFound
		},
		create: r.client.Create,
	}
	job := r.enqueueReconcile(t, id, 1, 0)
	r.run(t, settlements.ReconcileHandler(r.pool, stale))
	if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	st := r.mock.Stats()
	if st.CreateCalls != 2 || st.Replays != 1 || st.Payments != 1 || st.Charges != 1 {
		t.Errorf("provider stats = %+v, want 2 create calls, 1 replay, 1 payment, 1 charge", st)
	}
	if r.row(t, id).status != "succeeded" || r.postings(t, id) != 1 {
		t.Error("the settlement was not posted exactly once")
	}
}

func TestReconcile_LeavesSettlementsThatAreNotUnknownAlone(t *testing.T) {
	for _, state := range []string{"pending", "succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			r := newRig(t, mockpay.Config{})
			id := r.settlement(t, 700)
			switch state {
			case "succeeded":
				r.resolve(t, id, settlements.Success())
			case "failed":
				r.resolve(t, id, settlements.Failure("declined"))
			}
			before := r.postings(t, id)
			job := r.enqueueReconcile(t, id, 1, 0)
			r.run(t, settlements.ReconcileHandler(r.pool, r.client))
			if j := r.waitJob(t, job, "succeeded", "dead"); j.state != "succeeded" {
				t.Fatalf("job = %+v", j)
			}
			if st := r.mock.Stats(); st.Reads != 0 || st.CreateCalls != 0 {
				t.Errorf("the provider was called (%d reads, %d creates) for a %s settlement", st.Reads, st.CreateCalls, state)
			}
			if r.row(t, id).status != state || r.postings(t, id) != before || len(r.nextChecks(t, id)) != 0 {
				t.Error("something changed")
			}
		})
	}
}

// A rejected read (a 4xx) is our bug, not "no answer": it fails the attempt, and does not
// quietly schedule checks forever.
func TestReconcile_ARejectedReadIsAJobFailure(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.unknownSettlement(t, 700, "")
	job := r.enqueueReconcile(t, id, 1, 2)
	r.run(t, settlements.ReconcileHandler(r.pool, gateway{get: func(context.Context, string) (payclient.Payment, error) {
		return payclient.Payment{}, fmt.Errorf("%w: status 400", payclient.ErrRejected)
	}}))
	if j := r.waitJob(t, job, "dead", "succeeded"); j.state != "dead" || !strings.Contains(j.lastError, "rejected") {
		t.Fatalf("job = %+v", j)
	}
	if r.row(t, id).status != "unknown" || len(r.nextChecks(t, id)) != 0 {
		t.Error("a rejected read changed the settlement or scheduled a check")
	}
}

// gateway builds a Gateway from functions; a nil one answers "ambiguous".
type gateway struct {
	get    func(ctx context.Context, key string) (payclient.Payment, error)
	create func(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error)
}

func (g gateway) Get(ctx context.Context, key string) (payclient.Payment, error) {
	if g.get == nil {
		return payclient.Payment{}, fmt.Errorf("%w: down", payclient.ErrAmbiguous)
	}
	return g.get(ctx, key)
}

func (g gateway) Create(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error) {
	if g.create == nil {
		return payclient.Payment{}, fmt.Errorf("%w: down", payclient.ErrAmbiguous)
	}
	return g.create(ctx, key, r)
}

// toggled passes calls to the real client unless down is set, when every call is ambiguous.
type toggled struct {
	inner settlements.Gateway
	down  atomic.Bool
	gets  atomic.Int32
}

func (g *toggled) Get(ctx context.Context, key string) (payclient.Payment, error) {
	g.gets.Add(1)
	if g.down.Load() {
		return payclient.Payment{}, fmt.Errorf("%w: down", payclient.ErrAmbiguous)
	}
	return g.inner.Get(ctx, key)
}

func (g *toggled) Create(ctx context.Context, key string, r payclient.Request) (payclient.Payment, error) {
	if g.down.Load() {
		return payclient.Payment{}, fmt.Errorf("%w: down", payclient.ErrAmbiguous)
	}
	return g.inner.Create(ctx, key, r)
}

func TestReconcile_BadJobsAndCancellation(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	h := settlements.ReconcileHandler(r.pool, r.client)
	for name, payload := range map[string]string{
		"not json":           `nope`,
		"no settlement id":   `{"check":1}`,
		"check zero":         fmt.Sprintf(`{"settlement_id":"%s","check":0}`, uuid.New()),
		"no check":           fmt.Sprintf(`{"settlement_id":"%s"}`, uuid.New()),
		"unknown settlement": fmt.Sprintf(`{"settlement_id":"%s","check":1}`, uuid.New()),
	} {
		if commit, err := h(ctx, queue.Claimed{Payload: []byte(payload)}); err == nil || commit != nil {
			t.Errorf("%s: commit=%v err=%v, want an error and no commit", name, commit != nil, err)
		}
	}

	// Our own context ending is an error for the worker, never "no answer": it must not
	// burn a check or schedule the next one.
	id := r.unknownSettlement(t, 700, "")
	blocked := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(blocked)
		<-req.Context().Done()
	}))
	defer slow.Close()
	c := &payclient.Client{BaseURL: slow.URL, HTTP: &http.Client{Timeout: 10 * time.Second}}
	cctx, cancel := context.WithCancel(ctx)
	go func() { <-blocked; cancel() }()
	commit, err := settlements.ReconcileHandler(r.pool, c)(cctx, queue.Claimed{Payload: []byte(fmt.Sprintf(`{"settlement_id":"%s","check":1}`, id))})
	if err == nil || commit != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled read: commit=%v err=%v, want context.Canceled and no commit", commit != nil, err)
	}
}

// The settlement is resolved by a webhook between the read and the commit: the commit
// finds it no longer unknown under the row lock and schedules nothing.
func TestReconcile_NoNextCheckForASettlementResolvedMeanwhile(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.unknownSettlement(t, 700, "")
	r.enqueueReconcile(t, id, 1, 0)
	h := settlements.ReconcileHandler(r.pool, gateway{}) // the provider cannot be reached: no answer

	c := claim(t, r, 10*time.Second)
	commit, err := h(ctx, c)
	if err != nil || commit == nil {
		t.Fatalf("phase one: %v", err)
	}
	r.resolve(t, id, settlements.Success()) // the webhook

	if err := queue.CompleteWith(ctx, r.pool, c, commit); err != nil {
		t.Fatal(err)
	}
	if r.row(t, id).status != "succeeded" || r.postings(t, id) != 1 {
		t.Error("the webhook's resolution was disturbed")
	}
	if n := r.nextChecks(t, id); len(n) != 0 {
		t.Errorf("%d checks were scheduled for a settlement that was already resolved", len(n))
	}
}
