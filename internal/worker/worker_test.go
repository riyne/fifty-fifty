package worker_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
	"github.com/riyne/fifty-fifty/internal/worker"
)

var ctx = context.Background()

func newKind(prefix string) string { return "test." + prefix + "." + uuid.NewString() }

// fastConfig is a worker that notices everything within tens of milliseconds, so
// tests wait on conditions rather than sleeping for production-sized intervals.
func fastConfig(pool *pgxpool.Pool, kind string, h worker.Handler) worker.Config {
	return worker.Config{
		Pool:             pool,
		Handlers:         map[string]worker.Handler{kind: h},
		Concurrency:      4,
		PollInterval:     10 * time.Millisecond,
		Lease:            300 * time.Millisecond,
		ReapInterval:     50 * time.Millisecond,
		PeriodicInterval: 50 * time.Millisecond,
		ShutdownGrace:    2 * time.Second,
		Backoff:          queue.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// start runs a worker and returns a function that stops it and waits for Run to
// return. The stop function is also registered as cleanup.
func start(t *testing.T, cfg worker.Config) (stop func()) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx, cfg) }()
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
				t.Error("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func enqueue(t *testing.T, pool *pgxpool.Pool, j queue.NewJob) uuid.UUID {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id, err := queue.Enqueue(ctx, tx, j)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

type jobRow struct {
	State     string
	Attempts  int
	LastError string
	DueNow    bool
	Token     *uuid.UUID
}

func getJob(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) jobRow {
	t.Helper()
	var r jobRow
	var tok *uuid.UUID
	var le *string
	err := pool.QueryRow(ctx, `SELECT state, attempts, last_error, run_at <= now(), lease_token FROM jobs WHERE id = $1`, id).
		Scan(&r.State, &r.Attempts, &le, &r.DueNow, &tok)
	if err != nil {
		t.Fatal(err)
	}
	r.Token = tok
	if le != nil {
		r.LastError = *le
	}
	return r
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, want string) {
	t.Helper()
	waitFor(t, "job to be "+want, 10*time.Second, func() bool { return getJob(t, pool, id).State == want })
}

// effects stands in for a handler's database writes: audit_events rows of a kind
// unique to the test (the app role may insert there), one per call.
type effects struct {
	pool *pgxpool.Pool
	kind string
	user uuid.UUID
	grp  uuid.UUID
}

func newEffects(t *testing.T, pool *pgxpool.Pool) *effects {
	t.Helper()
	u, err := db.New(pool).EnsureUser(ctx, db.EnsureUserParams{Issuer: "https://worker-test.example", Subject: testutil.UniqueSubject("w")})
	if err != nil {
		t.Fatal(err)
	}
	g, err := db.New(pool).CreateGroup(ctx, db.CreateGroupParams{Name: "w", Currency: "CAD", CreatedBy: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	return &effects{pool: pool, kind: "test.effect." + uuid.NewString(), user: u.ID, grp: g.ID}
}

// commit returns a Commit that records the job id in an audit row.
func (e *effects) commit(job uuid.UUID) worker.Commit {
	return func(ctx context.Context, tx pgx.Tx) error {
		return db.New(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{
			ActorUserID: e.user, GroupID: e.grp, Kind: e.kind, Data: []byte(`{"job":"` + job.String() + `"}`),
		})
	}
}

func (e *effects) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE kind = $1`, e.kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *effects) distinctJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(DISTINCT data->>'job') FROM audit_events WHERE kind = $1`, e.kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- running jobs ----

func TestWorker_RunsJobsAndCommitsTheirEffects(t *testing.T) {
	pool := testutil.Pool(t)
	fx, kind := newEffects(t, pool), newKind("run")
	start(t, fastConfig(pool, kind, func(_ context.Context, j queue.Claimed) (worker.Commit, error) {
		return fx.commit(j.ID), nil
	}))

	var ids []uuid.UUID
	for range 5 {
		ids = append(ids, enqueue(t, pool, queue.NewJob{Kind: kind}))
	}
	for _, id := range ids {
		waitState(t, pool, id, queue.StateSucceeded)
	}
	if fx.count(t) != 5 || fx.distinctJobs(t) != 5 {
		t.Errorf("%d effects for %d distinct jobs, want 5 and 5", fx.count(t), fx.distinctJobs(t))
	}
}

func TestWorker_OnlyClaimsKindsItHasHandlersFor(t *testing.T) {
	pool := testutil.Pool(t)
	mine, theirs := newKind("mine"), newKind("theirs")
	start(t, fastConfig(pool, mine, func(context.Context, queue.Claimed) (worker.Commit, error) { return nil, nil }))

	other := enqueue(t, pool, queue.NewJob{Kind: theirs})
	own := enqueue(t, pool, queue.NewJob{Kind: mine})
	waitState(t, pool, own, queue.StateSucceeded) // the worker has certainly polled by now
	if row := getJob(t, pool, other); row.State != queue.StateQueued || row.Attempts != 0 {
		t.Errorf("a job of a kind with no handler was touched: %+v", row)
	}
}

func TestWorker_FailingHandlerIsRetriedThenDeadLettered(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("fail")
	var calls atomic.Int32
	start(t, fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) {
		calls.Add(1)
		return nil, errors.New("provider said no")
	}))

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	waitState(t, pool, id, queue.StateDead)
	row := getJob(t, pool, id)
	if row.Attempts != 3 || calls.Load() != 3 || row.LastError != "provider said no" {
		t.Errorf("attempts %d, handler calls %d, last_error %q; want 3, 3, the handler's error", row.Attempts, calls.Load(), row.LastError)
	}
}

func TestWorker_FailedCommitDiscardsEffectsAndRetries(t *testing.T) {
	pool := testutil.Pool(t)
	fx, kind := newEffects(t, pool), newKind("badcommit")
	var calls atomic.Int32
	start(t, fastConfig(pool, kind, func(_ context.Context, j queue.Claimed) (worker.Commit, error) {
		first := calls.Add(1) == 1
		return func(ctx context.Context, tx pgx.Tx) error {
			if err := fx.commit(j.ID)(ctx, tx); err != nil {
				return err
			}
			if first {
				return errors.New("write failed halfway") // after the effect was written
			}
			return nil
		}, nil
	}))

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	waitState(t, pool, id, queue.StateSucceeded)
	if fx.count(t) != 1 || getJob(t, pool, id).Attempts != 2 {
		t.Errorf("%d effects after a rolled-back first attempt and a good second, attempts %d; want 1 and 2", fx.count(t), getJob(t, pool, id).Attempts)
	}
}

func TestWorker_PanicIsAFailureAndTheWorkerSurvives(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("panic")
	var calls atomic.Int32
	start(t, fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return nil, nil
	}))

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	waitState(t, pool, id, queue.StateSucceeded)
	if row := getJob(t, pool, id); row.Attempts != 2 || row.LastError != "handler panicked: boom" {
		t.Errorf("row = %+v, want attempt 2 and the panic recorded", row)
	}
	// And it keeps working.
	next := enqueue(t, pool, queue.NewJob{Kind: kind})
	waitState(t, pool, next, queue.StateSucceeded)
}

func TestWorker_ConcurrencyIsBounded(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("conc")
	var now, peak atomic.Int32
	cfg := fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) {
		n := now.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(80 * time.Millisecond)
		now.Add(-1)
		return nil, nil
	})
	cfg.Concurrency = 2
	start(t, cfg)

	var ids []uuid.UUID
	for range 6 {
		ids = append(ids, enqueue(t, pool, queue.NewJob{Kind: kind}))
	}
	for _, id := range ids {
		waitState(t, pool, id, queue.StateSucceeded)
	}
	if peak.Load() != 2 {
		t.Errorf("peak concurrency %d, want exactly the limit of 2 (a backlog of 6 should reach it)", peak.Load())
	}
}

// ---- leases ----

// The lease is 300 ms and the job takes 1.2 s, with a second worker polling the
// whole time. Only the heartbeat stops the second worker taking it over.
func TestWorker_HeartbeatKeepsALongJobAlive(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("long")
	var calls atomic.Int32
	h := func(ctx context.Context, _ queue.Claimed) (worker.Commit, error) {
		calls.Add(1)
		select {
		case <-time.After(1200 * time.Millisecond):
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	start(t, fastConfig(pool, kind, h))
	start(t, fastConfig(pool, kind, h))

	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	waitState(t, pool, id, queue.StateSucceeded)
	if row := getJob(t, pool, id); row.Attempts != 1 || calls.Load() != 1 {
		t.Errorf("attempts %d, handler calls %d; want 1 and 1 (the lease was kept alive)", row.Attempts, calls.Load())
	}
}

// Another worker takes the job over while this one is still working on it. The
// next heartbeat finds out, the handler's context is canceled, and the worker
// reports nothing: the new holder's attempt is the one that counts.
func TestWorker_LostLeaseCancelsTheHandlerAndReportsNothing(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("lost")
	started, cause := make(chan struct{}), make(chan error, 1)
	cfg := fastConfig(pool, kind, func(ctx context.Context, _ queue.Claimed) (worker.Commit, error) {
		close(started)
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return nil, ctx.Err()
	})
	cfg.Lease = 3 * time.Second // heartbeats every second: slow enough to take the job over in between
	start(t, cfg)

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	<-started

	// Play worker B: expire the lease and claim it. A heartbeat landing between the
	// two would revive the lease, so retry until B gets it.
	var b []queue.Claimed
	waitFor(t, "B to take the job over", 5*time.Second, func() bool {
		if _, err := pool.Exec(ctx, `UPDATE jobs SET run_at = now() - interval '1 second', lease_expires_at = now() - interval '1 second' WHERE id = $1 AND state = 'running'`, id); err != nil {
			t.Fatal(err)
		}
		var err error
		b, err = queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return len(b) == 1
	})

	select {
	case c := <-cause:
		if !errors.Is(c, queue.ErrLeaseLost) {
			t.Errorf("handler context ended with %v, want ErrLeaseLost", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was never told it lost the lease")
	}
	time.Sleep(100 * time.Millisecond) // let the worker finish reacting
	row := getJob(t, pool, id)
	if row.State != queue.StateRunning || row.Attempts != 2 || row.LastError != "" || row.Token == nil || *row.Token != b[0].LeaseToken {
		t.Errorf("row = %+v; the stale worker must not have changed B's attempt", row)
	}
}

func TestWorker_ReapsJobsThatDiedOnTheirLastAttempt(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("reap")
	start(t, fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) { return nil, nil }))

	// What a worker that claimed the job and was then killed leaves behind: running,
	// lease expired, no attempts left. The job is created far in the future and
	// moved into that state in one statement, so the test's worker never runs it.
	dead := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1, RunAt: time.Now().Add(time.Hour)})
	if _, err := pool.Exec(ctx, `UPDATE jobs SET state = 'running', attempts = 1, lease_token = gen_random_uuid(),
		lease_expires_at = now() - interval '1 second', run_at = now() - interval '1 second' WHERE id = $1`, dead); err != nil {
		t.Fatal(err)
	}
	waitState(t, pool, dead, queue.StateDead)
	if row := getJob(t, pool, dead); row.LastError != "lease expired on attempt 1 of 1" {
		t.Errorf("last_error = %q", row.LastError)
	}
}

// ---- shutdown ----

func TestWorker_ShutdownLetsRunningJobsFinishAndTakesNoNewOnes(t *testing.T) {
	pool := testutil.Pool(t)
	fx, kind := newEffects(t, pool), newKind("graceful")
	started := make(chan struct{})
	var once sync.Once
	cfg := fastConfig(pool, kind, func(_ context.Context, j queue.Claimed) (worker.Commit, error) {
		once.Do(func() { close(started) })
		time.Sleep(400 * time.Millisecond)
		return fx.commit(j.ID), nil
	})
	stop := start(t, cfg)

	running := enqueue(t, pool, queue.NewJob{Kind: kind})
	<-started
	go stop() // SIGTERM arrives mid-job
	time.Sleep(50 * time.Millisecond)
	late := enqueue(t, pool, queue.NewJob{Kind: kind}) // arrives after shutdown began

	stop() // waits for Run to return
	if row := getJob(t, pool, running); row.State != queue.StateSucceeded || fx.count(t) != 1 {
		t.Errorf("in-flight job: %+v, effects %d; it should have been allowed to finish", row, fx.count(t))
	}
	time.Sleep(100 * time.Millisecond)
	if row := getJob(t, pool, late); row.State != queue.StateQueued || row.Attempts != 0 {
		t.Errorf("a job arriving after shutdown began was claimed: %+v", row)
	}
}

func TestWorker_ShutdownPastTheGracePeriodHandsTheJobBack(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("grace")
	started, sawCancel := make(chan struct{}), make(chan struct{})
	cfg := fastConfig(pool, kind, func(ctx context.Context, _ queue.Claimed) (worker.Commit, error) {
		close(started)
		<-ctx.Done()
		close(sawCancel)
		return nil, ctx.Err()
	})
	cfg.ShutdownGrace = 150 * time.Millisecond
	stop := start(t, cfg)

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	<-started
	begin := time.Now()
	stop()
	if took := time.Since(begin); took > 3*time.Second {
		t.Errorf("shutdown took %v with a 150ms grace period", took)
	}
	select {
	case <-sawCancel:
	default:
		t.Error("the handler was never canceled")
	}
	row := getJob(t, pool, id)
	if row.State != queue.StateQueued || !row.DueNow || row.Attempts != 1 || row.Token != nil ||
		row.LastError != "worker shut down mid-job: context canceled" {
		t.Errorf("row = %+v; want queued, due now, no lease, shutdown recorded", row)
	}
	// Another worker can take it straight away.
	got, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
	if err != nil || len(got) != 1 || got[0].Attempt != 2 {
		t.Errorf("claim after handback: %+v, %v", got, err)
	}
}

// ---- several workers ----

// Two workers, 150 jobs: every job's effect is committed exactly once and each job
// is handled once (no crashes here, so a second call would mean a double claim).
func TestWorker_TwoWorkersHandleEachJobOnce(t *testing.T) {
	pool := testutil.Pool(t)
	fx, kind := newEffects(t, pool), newKind("two")
	var mu sync.Mutex
	handled := map[uuid.UUID]int{}
	h := func(_ context.Context, j queue.Claimed) (worker.Commit, error) {
		mu.Lock()
		handled[j.ID]++
		mu.Unlock()
		return fx.commit(j.ID), nil
	}
	start(t, fastConfig(pool, kind, h))
	start(t, fastConfig(pool, kind, h))

	const n = 150
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for range n {
		id, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "all jobs to succeed", 20*time.Second, func() bool {
		var done int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1 AND state = 'succeeded'`, kind).Scan(&done); err != nil {
			t.Fatal(err)
		}
		return done == n
	})
	mu.Lock()
	defer mu.Unlock()
	for _, id := range ids {
		if handled[id] != 1 {
			t.Errorf("job %v handled %d times", id, handled[id])
		}
	}
	if fx.count(t) != n || fx.distinctJobs(t) != n {
		t.Errorf("%d effects for %d distinct jobs, want %d of each", fx.count(t), fx.distinctJobs(t), n)
	}
}

// Both workers try to schedule the periodic job at start-up and every 50 ms after:
// still exactly one job for the period, run once.
func TestWorker_SchedulesAPeriodicJobOncePerPeriod(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("periodic")
	var calls atomic.Int32
	h := func(context.Context, queue.Claimed) (worker.Commit, error) { calls.Add(1); return nil, nil }
	cfg := fastConfig(pool, kind, h)
	cfg.Periodic = []queue.PeriodicJob{{Kind: kind, Every: time.Hour}}
	start(t, cfg)
	start(t, cfg)

	waitFor(t, "the periodic job to run", 10*time.Second, func() bool { return calls.Load() >= 1 })
	time.Sleep(300 * time.Millisecond) // several more scheduling rounds
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || calls.Load() != 1 {
		t.Errorf("%d jobs and %d runs for one period, want 1 and 1", n, calls.Load())
	}
}

func TestRun_RejectsBadConfig(t *testing.T) {
	pool := testutil.Pool(t)
	ok := func() worker.Config {
		return fastConfig(pool, "k", func(context.Context, queue.Claimed) (worker.Commit, error) { return nil, nil })
	}
	cases := map[string]func(c *worker.Config){
		"no pool":                  func(c *worker.Config) { c.Pool = nil },
		"no handlers":              func(c *worker.Config) { c.Handlers = nil },
		"nil handler":              func(c *worker.Config) { c.Handlers["k"] = nil },
		"negative concurrency":     func(c *worker.Config) { c.Concurrency = -1 },
		"concurrency over a batch": func(c *worker.Config) { c.Concurrency = queue.MaxClaimBatch + 1 },
		"negative poll":            func(c *worker.Config) { c.PollInterval = -time.Second },
		"lease too short":          func(c *worker.Config) { c.Lease = time.Millisecond },
		"periodic without handler": func(c *worker.Config) {
			c.Periodic = []queue.PeriodicJob{{Kind: "other", Every: time.Hour}}
		},
	}
	for name, mutate := range cases {
		c := ok()
		mutate(&c)
		if err := worker.Run(ctx, c); err == nil {
			t.Errorf("%s: Run accepted the config", name)
		}
	}
}

// The real cleanup handler, scheduled by the real scheduler, run by the real loop.
func TestWorker_IdempotencyCleanupRunsEndToEnd(t *testing.T) {
	pool := testutil.Pool(t)
	u, err := db.New(pool).EnsureUser(ctx, db.EnsureUserParams{Issuer: "https://worker-test.example", Subject: testutil.UniqueSubject("e2e")})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(in string) string {
		k := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
			VALUES ($1, $2, '\x01', 200, '\x7b7d', now() + $3::interval)`, u.ID, k, in); err != nil {
			t.Fatal(err)
		}
		return k
	}
	exists := func(k string) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND key = $2`, u.ID, k).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	old, fresh := insert("-2 hours"), insert("2 hours")

	// Enqueue the job directly: the schedule's own hourly period may already have
	// been used by an earlier run, and what is under test is the handler and the loop.
	id := enqueue(t, pool, queue.NewJob{Kind: worker.KindIdempotencyCleanup})
	cfg := fastConfig(pool, worker.KindIdempotencyCleanup, worker.IdempotencyCleanup(pool))
	start(t, cfg)
	waitState(t, pool, id, queue.StateSucceeded)

	if exists(old) || !exists(fresh) {
		t.Errorf("expired key present=%v, live key present=%v; want false and true", exists(old), exists(fresh))
	}
}

// maxRand always draws the top of the range, so the delay is exactly the ceiling.
type maxRand struct{}

func (maxRand) Int64N(n int64) int64 { return n - 1 }

// A failed attempt waits the backoff policy's delay, measured on the database
// clock: with a one-hour ceiling and the top draw, the retry is an hour away and
// the worker does not retry in the meantime.
func TestWorker_RetryWaitsTheBackoffDelay(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("backoff")
	var calls atomic.Int32
	cfg := fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) {
		calls.Add(1)
		return nil, errors.New("429")
	})
	cfg.Backoff = queue.Backoff{Base: time.Hour, Max: time.Hour}
	cfg.Rand = maxRand{}
	start(t, cfg)

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	waitFor(t, "the first attempt to fail", 10*time.Second, func() bool { return getJob(t, pool, id).LastError == "429" })
	time.Sleep(200 * time.Millisecond) // many poll intervals: a worker ignoring the delay would have retried
	var in time.Duration
	if err := pool.QueryRow(ctx, `SELECT run_at - now() FROM jobs WHERE id = $1`, id).Scan(&in); err != nil {
		t.Fatal(err)
	}
	if row := getJob(t, pool, id); row.State != queue.StateQueued || row.Attempts != 1 || calls.Load() != 1 || in < 59*time.Minute || in > time.Hour {
		t.Errorf("row %+v, handler calls %d, retry in %v; want queued after 1 attempt, 1 call, retry in about 1h", row, calls.Load(), in)
	}
}

// A backlog fills every free slot in one round. The poll interval is an hour, so the
// only way four jobs can all be running together is one claim of four: a worker that
// claimed one at a time would sit on the first job (which is waiting for the others)
// until the next poll.
func TestWorker_ClaimsABacklogIntoAllFreeSlotsAtOnce(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("batch")
	const n = 4
	var wg sync.WaitGroup
	wg.Add(n)
	together := make(chan struct{})
	go func() { wg.Wait(); close(together) }()
	var allRan atomic.Bool
	cfg := fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) {
		wg.Done()
		select {
		case <-together:
			allRan.Store(true)
			return nil, nil
		case <-time.After(5 * time.Second):
			return nil, errors.New("the other jobs never ran alongside this one")
		}
	})
	cfg.Concurrency = n
	cfg.PollInterval = time.Hour

	var ids []uuid.UUID
	for range n {
		ids = append(ids, enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})) // before the worker starts
	}
	start(t, cfg)
	for _, id := range ids {
		waitState(t, pool, id, queue.StateSucceeded)
	}
	if !allRan.Load() {
		t.Error("the jobs did not run together")
	}
}

// ---- retry-after ----

type askToWait struct{ d time.Duration }

func (e askToWait) Error() string             { return "429 rate limited" }
func (e askToWait) RetryAfter() time.Duration { return e.d }

type zeroRand struct{}

func (zeroRand) Int64N(int64) int64 { return 0 }

// retryIn runs one failing attempt whose error is err and returns how far away the retry
// was scheduled, measured on the database clock.
func retryIn(t *testing.T, mutate func(*worker.Config), err error) time.Duration {
	t.Helper()
	pool := testutil.Pool(t)
	kind := newKind("retryafter")
	cfg := fastConfig(pool, kind, func(context.Context, queue.Claimed) (worker.Commit, error) { return nil, err })
	cfg.Rand = zeroRand{} // the backoff delay is 0, so the retry-after alone decides
	mutate(&cfg)
	start(t, cfg)
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	waitFor(t, "the first attempt to fail", 10*time.Second, func() bool { return getJob(t, pool, id).LastError != "" })
	time.Sleep(100 * time.Millisecond)
	var in time.Duration
	if e := pool.QueryRow(ctx, `SELECT run_at - now() FROM jobs WHERE id = $1`, id).Scan(&in); e != nil {
		t.Fatal(e)
	}
	if row := getJob(t, pool, id); row.State != queue.StateQueued || row.Attempts != 1 {
		t.Fatalf("row %+v", row)
	}
	return in
}

// An error that says how long to wait delays the retry by at least that long (here the
// backoff would have retried at once), even when wrapped by the handler.
func TestWorker_RetryAfterDelaysTheRetry(t *testing.T) {
	for name, err := range map[string]error{
		"direct":  askToWait{time.Hour},
		"wrapped": fmt.Errorf("extract: %w", fmt.Errorf("call: %w", askToWait{time.Hour})),
	} {
		cfgMax := func(c *worker.Config) { c.MaxRetryAfter = 2 * time.Hour }
		if in := retryIn(t, cfgMax, err); in < 59*time.Minute || in > time.Hour {
			t.Errorf("%s: retry in %v, want about 1h", name, in)
		}
	}
	// Nothing to honor, or a retry-after of zero: the backoff alone decides (an hour here).
	hourBackoff := func(c *worker.Config) {
		c.Backoff = queue.Backoff{Base: time.Hour, Max: time.Hour}
		c.Rand = maxRand{}
	}
	for name, err := range map[string]error{"plain error": errors.New("503"), "zero retry-after": askToWait{0}} {
		if in := retryIn(t, hourBackoff, err); in < 59*time.Minute || in > time.Hour {
			t.Errorf("%s: retry in %v, want the 1h backoff", name, in)
		}
	}
}

// A service cannot park a job for longer than the worker's cap.
func TestWorker_RetryAfterIsCapped(t *testing.T) {
	in := retryIn(t, func(c *worker.Config) { c.MaxRetryAfter = 30 * time.Minute }, askToWait{24 * time.Hour})
	if in < 29*time.Minute || in > 30*time.Minute {
		t.Errorf("retry in %v, want about the 30 minute cap", in)
	}
	// The default cap is 5 minutes.
	in = retryIn(t, func(*worker.Config) {}, askToWait{24 * time.Hour})
	if in < 4*time.Minute || in > 5*time.Minute {
		t.Errorf("retry in %v, want about the 5 minute default cap", in)
	}
}

// The backoff is a floor: a short retry-after never makes the retry sooner than the
// backoff would have.
func TestWorker_RetryAfterNeverShortensTheBackoff(t *testing.T) {
	in := retryIn(t, func(c *worker.Config) {
		c.Backoff = queue.Backoff{Base: time.Hour, Max: time.Hour}
		c.Rand = maxRand{}
	}, askToWait{time.Minute})
	if in < 59*time.Minute || in > time.Hour {
		t.Errorf("retry in %v, want the 1h backoff, not the 1 minute retry-after", in)
	}
}

func TestWorker_NegativeMaxRetryAfterIsRefused(t *testing.T) {
	cfg := fastConfig(testutil.Pool(t), newKind("neg"), func(context.Context, queue.Claimed) (worker.Commit, error) { return nil, nil })
	cfg.MaxRetryAfter = -time.Second
	if err := worker.Run(ctx, cfg); err == nil {
		t.Error("a negative MaxRetryAfter was accepted")
	}
}
