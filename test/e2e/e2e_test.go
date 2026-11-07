// Package e2e runs settlement end to end with real processes: the real worker binary
// (several, one of them killed with SIGKILL in some scenarios), the real mock payment
// provider binary, and the real API handlers in-process, over a scratch database that
// the test creates and drops. Settlement is simulated; the provider is a mock.
//
// Each scenario creates settlements through the API, lets the workers, the provider's
// webhooks and reconciliation resolve them, and then asks settlerig.Verify whether the
// ledger agrees with the provider's own books. The scenarios assert the invariants
// (exactly one posting per succeeded settlement, no double charge, nothing charged but
// recorded as failed), not timings.
//
// These tests take minutes and start processes, so they run only when E2E=1:
//
//	make test-e2e
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/settlerig"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

const webhookSecret = "whsec_e2e"

var (
	bins    settlerig.Bins
	ctx     = context.Background()
	enabled = os.Getenv("E2E") == "1"
)

func TestMain(m *testing.M) {
	if !enabled {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "fifty-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bins, err = settlerig.Build(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// scenario describes one run.
type scenario struct {
	name        string
	settlements int
	workers     int
	lease       time.Duration
	mock        []string // mockpay flags (the webhook url and secret are added)
	webhooks    bool
	killOne     bool          // SIGKILL the first worker once the provider has been called
	timeout     time.Duration // how long to wait for every settlement to be final
	check       func(t *testing.T, r *result)
}

type result struct {
	report settlerig.Report
	stats  mockpay.Stats
	ids    []uuid.UUID
	db     *settlerig.Scratch
}

func (r *result) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.db.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func providerStats(t *testing.T, base string) mockpay.Stats {
	t.Helper()
	resp, err := http.Get(base + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s mockpay.Stats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func run(t *testing.T, sc scenario) {
	t.Helper()
	ownerURL, appURL := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	db, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Drop(context.Background()); err != nil {
			t.Errorf("drop %s: %v", db.Name, err)
		}
	})

	stack := teststack.NewOn(t, db.Pool, db.Owner, teststack.WithWebhookSecret(webhookSecret))
	flags := append([]string{"--seed", "1", "--timeout-hold", "300ms"}, sc.mock...)
	if sc.webhooks {
		flags = append(flags, "--webhook-url", stack.Server.URL+settlements.WebhookPath)
	}
	mock, mockURL, err := settlerig.StartMockpay(bins.Mockpay, webhookSecret, flags...)
	if err != nil {
		t.Fatal(err)
	}
	var procs []*settlerig.Proc
	var procMu sync.Mutex
	t.Cleanup(func() {
		procMu.Lock()
		defer procMu.Unlock()
		for _, p := range procs {
			p.Stop()
		}
		mock.Stop()
		if t.Failed() {
			t.Logf("mockpay log tail:\n%s", tail(mock.Log(), 1500))
			for _, p := range procs {
				t.Logf("%s log tail:\n%s", p.Name, tail(p.Log(), 1500))
			}
		}
	})
	startWorker := func(name string) *settlerig.Proc {
		p, err := settlerig.StartWorker(settlerig.WorkerSpec{
			Bin: bins.Worker, DatabaseURL: db.WorkerURL, PaymentsURL: mockURL, Name: name,
			Concurrency: 4, Lease: sc.lease, Poll: 100 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		procMu.Lock()
		procs = append(procs, p)
		procMu.Unlock()
		return p
	}

	// Workers start first when a worker will be killed, so the victim is the only one
	// holding jobs; otherwise they start together before the settlements exist.
	first := startWorker("worker-1")
	if !sc.killOne {
		for i := 2; i <= sc.workers; i++ {
			startWorker(fmt.Sprintf("worker-%d", i))
		}
	}

	ids := createSettlements(t, stack, sc.settlements)

	if sc.killOne {
		waitFor(t, 20*time.Second, "the provider to be called", func() bool { return providerStats(t, mockURL).CreateCalls > 0 })
		for i := 2; i <= sc.workers; i++ {
			startWorker(fmt.Sprintf("worker-%d", i))
		}
		var running, claimedTotal int
		_ = db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'running'), count(*) FILTER (WHERE attempts > 0) FROM jobs WHERE kind = 'settlement.execute'`).Scan(&running, &claimedTotal)
		t.Logf("killing %s: provider stats %+v; jobs running %d, ever claimed %d", first.Name, providerStats(t, mockURL), running, claimedTotal)
		if err := first.Kill(); err != nil {
			t.Fatal(err)
		}
	}

	left, err := settlerig.WaitResolved(ctx, db.Pool, ids, sc.timeout)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // duplicate and late webhooks still arriving
	provider, err := settlerig.FetchProvider(ctx, mockURL)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := settlerig.Verify(ctx, db.Pool, provider, ids)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %v, %d postings, provider took %d charges, %d unresolved", sc.name, rep.ByStatus, rep.Postings, rep.ProviderCharges, left)

	for _, a := range rep.Anomalies {
		t.Errorf("anomaly: %s", a)
	}
	if left != 0 || rep.Unresolved() != 0 {
		t.Errorf("%d settlements were not final within %v", left, sc.timeout)
	}
	if got := rep.ByStatus["succeeded"]; rep.ProviderCharges != got || rep.Postings != got {
		t.Errorf("succeeded %d, ledger postings %d, provider charges %d: they must all be equal", got, rep.Postings, rep.ProviderCharges)
	}
	if sc.check != nil {
		sc.check(t, &result{report: rep, stats: providerStats(t, mockURL), ids: ids, db: db})
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// createSettlements makes one payer who is owed by n debtors (one expense, each debtor
// claims one item, the payer finalizes) and has each debtor settle 700 of their 1000
// through the real endpoint, all at once. It returns the settlement ids.
func createSettlements(t *testing.T, s *teststack.Stack, n int) []uuid.UUID {
	t.Helper()
	payer := s.Token(t, "payer")
	payerID := s.UserID(t, payer)
	group := s.CreateGroup(t, payer, "e2e")
	_, code := s.CreateJoinCode(t, payer, group, n, 0)

	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = s.Token(t, fmt.Sprintf("debtor%d", i))
		if r := s.Join(t, tokens[i], code); r.Status != http.StatusCreated {
			t.Fatalf("join %d = %d: %s", i, r.Status, r.Body)
		}
	}
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"description":"item %d","price_minor":1000}`, i)
	}
	exp := s.CreateExpense(t, payer, group, `{"description":"e2e","items":[`+strings.Join(items, ",")+`]}`)
	var detail struct {
		Items []struct {
			ID       string `json:"id"`
			Position int    `json:"position"`
		} `json:"items"`
	}
	s.Do(t, payer, "GET", "/v1/groups/"+group+"/expenses/"+exp, "").JSON(t, &detail)
	for _, it := range detail.Items {
		path := "/v1/groups/" + group + "/expenses/" + exp + "/items/" + it.ID + "/claim"
		if r := s.Do(t, tokens[it.Position], "PUT", path, ""); r.Status != 200 {
			t.Fatalf("claim = %d: %s", r.Status, r.Body)
		}
	}
	if r := s.Do(t, payer, "POST", "/v1/groups/"+group+"/expenses/"+exp+"/finalize", `{}`, "Idempotency-Key", uuid.NewString()); r.Status != 200 {
		t.Fatalf("finalize = %d: %s", r.Status, r.Body)
	}

	ids := make([]uuid.UUID, n)
	var wg sync.WaitGroup
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"to_member_id":"%s","amount_minor":700}`, payerID)
			r := s.Do(t, tokens[i], "POST", "/v1/groups/"+group+"/settlements", body, "Idempotency-Key", uuid.NewString())
			if r.Status != http.StatusAccepted {
				t.Errorf("create settlement %d = %d: %s", i, r.Status, r.Body)
				return
			}
			var st struct {
				ID uuid.UUID `json:"id"`
			}
			r.JSON(t, &st)
			ids[i] = st.ID
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	return ids
}

func requireEnabled(t *testing.T) {
	t.Helper()
	if !enabled {
		t.Skip("set E2E=1 (make test-e2e): these start processes and take minutes")
	}
}

func TestE2E(t *testing.T) {
	requireEnabled(t)
	fast := []string{"--latency-median", "60ms", "--latency-sigma", "0.3"}
	with := func(extra ...string) []string { return append(append([]string{}, fast...), extra...) }

	for _, sc := range []scenario{
		{
			name: "healthy: no faults", settlements: 20, workers: 2, lease: 5 * time.Second, mock: fast, webhooks: true, timeout: 60 * time.Second,
			check: func(t *testing.T, r *result) {
				if r.report.ByStatus["succeeded"] != 20 {
					t.Errorf("statuses %v, want all 20 succeeded", r.report.ByStatus)
				}
				if n := r.count(t, `SELECT count(*) FROM jobs WHERE kind = 'settlement.reconcile'`); n != 0 {
					t.Errorf("%d reconcile jobs in a run with no faults", n)
				}
				if r.stats.WebhookNon2xxOrFail != 0 {
					t.Errorf("provider stats %+v: the api refused or failed a webhook", r.stats)
				}
			},
		},
		{
			// The payment is created and the response is lost, every time. Only the
			// provider's signed webhooks (duplicated, delayed, reordered) tell the api.
			name: "lost responses, healed by webhooks", settlements: 20, workers: 2, lease: 5 * time.Second, webhooks: true, timeout: 60 * time.Second,
			mock: with("--rate-5xx-after", "1", "--webhook-duplicate", "0.5", "--webhook-delay-max", "150ms", "--webhook-out-of-order", "0.5"),
			check: func(t *testing.T, r *result) {
				if r.report.ByStatus["succeeded"] != 20 {
					t.Errorf("statuses %v, want all 20 succeeded", r.report.ByStatus)
				}
			},
		},
		{
			// Same loss, and the webhooks never come: reconciliation reads the provider.
			name: "lost responses, no webhooks: reconciled", settlements: 10, workers: 2, lease: 5 * time.Second, timeout: 90 * time.Second,
			mock: with("--rate-timeout-after", "1"),
			check: func(t *testing.T, r *result) {
				if r.report.ByStatus["succeeded"] != 10 {
					t.Errorf("statuses %v, want all 10 succeeded", r.report.ByStatus)
				}
				if n := r.count(t, `SELECT count(*) FROM audit_events WHERE kind = 'settlement.unknown'`); n != 10 {
					t.Errorf("%d settlement.unknown events, want 10 (every response was lost)", n)
				}
			},
		},
		{
			// Some requests never reach the provider. Reconcile finds nothing and sends
			// the same request again, until it gets through.
			name: "requests that never arrive: re-sent", settlements: 10, workers: 2, lease: 5 * time.Second, timeout: 180 * time.Second,
			mock: with("--rate-timeout-before", "0.2", "--rate-5xx-before", "0.2"),
			check: func(t *testing.T, r *result) {
				if r.report.ByStatus["succeeded"] != 10 {
					t.Errorf("statuses %v, want all 10 succeeded", r.report.ByStatus)
				}
				if r.stats.CreateCalls <= 10 {
					t.Errorf("%d create calls for 10 settlements: nothing was re-sent, so the faults did not fire", r.stats.CreateCalls)
				}
			},
		},
		{
			name: "declines", settlements: 20, workers: 2, lease: 5 * time.Second, webhooks: true, timeout: 60 * time.Second,
			mock: with("--rate-decline", "0.5"),
			check: func(t *testing.T, r *result) {
				if r.report.ByStatus["failed"] == 0 || r.report.ByStatus["succeeded"] == 0 {
					t.Errorf("statuses %v: expected both outcomes at a 50%% decline rate", r.report.ByStatus)
				}
			},
		},
		{
			// kill -9 the worker while its jobs are waiting on the provider. The others
			// take the jobs over when the lease runs out and the provider returns the
			// payments it already made. No webhooks here, on purpose: the provider sends
			// them when it creates a payment, before its slow response, so with them on the
			// api resolves the victim's settlements first and the orphaned jobs find nothing
			// to do (correct, but then the kill proves nothing about re-execution).
			name: "worker killed mid-call", settlements: 12, workers: 2, lease: 3 * time.Second, killOne: true, timeout: 60 * time.Second,
			mock: []string{"--latency-median", "1500ms", "--latency-sigma", "0"},
			check: func(t *testing.T, r *result) {
				if r.report.ByStatus["succeeded"] != 12 {
					t.Errorf("statuses %v, want all 12 succeeded", r.report.ByStatus)
				}
				if r.stats.Replays == 0 {
					t.Errorf("provider stats %+v: no job was re-run after the kill, so the kill hit nothing", r.stats)
				}
				if n := r.count(t, `SELECT count(*) FROM jobs WHERE kind = 'settlement.execute' AND attempts >= 2`); n == 0 {
					t.Error("no execute job needed a second attempt")
				}
			},
		},
		{
			// Everything at once, and a worker killed in the middle.
			name: "everything at once", settlements: 30, workers: 3, lease: 3 * time.Second, killOne: true, webhooks: true, timeout: 240 * time.Second,
			mock: []string{"--latency-median", "400ms", "--latency-sigma", "0.4",
				"--rate-decline", "0.1", "--rate-timeout-before", "0.05", "--rate-5xx-before", "0.05", "--rate-timeout-after", "0.1", "--rate-5xx-after", "0.1",
				"--rate-read-error", "0.1", "--webhook-drop", "0.2", "--webhook-duplicate", "0.3", "--webhook-delay-max", "300ms", "--webhook-out-of-order", "0.3"},
		},
	} {
		t.Run(sc.name, func(t *testing.T) { run(t, sc) })
	}
}

// The benchmark runner (bench/settlement) uses settlerig.RunLoad. A small run in the
// test suite keeps it working: no faults, nothing may be left unresolved or anomalous.
func TestLoadRunnerSmoke(t *testing.T) {
	requireEnabled(t)
	ownerURL, appURL := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	res, err := settlerig.RunLoad(ctx, settlerig.LoadParams{
		OwnerURL: ownerURL, AppURL: appURL, Bins: bins,
		Mix:         settlerig.Mix{Name: "smoke", Mock: []string{"--latency-median", "50ms", "--rate-5xx-after", "0.2"}, Webhooks: true},
		Settlements: 20, Rate: 10, Workers: 2, Concurrency: 4, Lease: 5 * time.Second, Poll: 200 * time.Millisecond,
		TimeoutHold: time.Second, Seed: 1, WebhookSecret: webhookSecret, ResolveTimeout: 2 * time.Minute, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unresolved != 0 || len(res.Anomalies) != 0 || res.DeadJobs != 0 || res.ByStatus["succeeded"] != 20 ||
		res.Postings != 20 || res.Charges != 20 || res.EndToEnd.N != 20 || res.DatabaseJobsRows != 0 {
		t.Errorf("result = %+v", res)
	}
}
