package benchwork

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/mockllm"
)

// LoadParams describes one throughput, latency or retry run: W real worker
// processes draining mock.extract jobs against the in-process mock LLM.
//
// Two ways to offer load, exactly one of which must be set:
//
//   - Closed loop (Jobs > 0): all jobs are enqueued before the workers start and the
//     workers drain them as fast as they can. This measures throughput. End-to-end
//     latency under it is mostly time spent waiting in the backlog, so it says
//     nothing about the queue's own delay.
//   - Open loop (Rate > 0, Duration > 0): after a warm-up, jobs arrive as a Poisson
//     process at Rate per second for Duration, whatever the workers are doing. With
//     Rate below capacity this measures latency: how long a job takes from enqueue
//     to done, and how long it waits before a worker starts it.
type LoadParams struct {
	DatabaseURL string // the app role
	OwnerURL    string // the owner role, for the bench schema and cleanup
	WorkerBin   string // a built cmd/benchworker

	Workers     int
	Concurrency int // jobs in flight per worker
	Lease, Poll time.Duration
	Mock        mockllm.Config
	Lean        bool // no bookkeeping writes: required for max throughput, disables QueueWait
	MaxAttempts int  // default 5

	Jobs     int
	Rate     float64
	Duration time.Duration
	Warmup   time.Duration // open loop: workers run this long before arrivals begin; default 2s
	Seed     uint64        // seeds the arrival process

	DrainTimeout time.Duration // wait for every job to finish; default 5 minutes
	Logf         func(format string, args ...any)
}

// LoadResult is what one run measured. Times are seconds on the database's clock.
type LoadResult struct {
	Run        string `json:"run"`
	Enqueued   int    `json:"enqueued"`
	Succeeded  int    `json:"succeeded"`
	Dead       int    `json:"dead"`
	Unfinished int    `json:"unfinished"`

	DuplicateFx int `json:"duplicate_effects"`
	MissingFx   int `json:"missing_effects"`

	TotalAttempts int         `json:"total_attempts"`   // handler starts: sum of the jobs' attempts counters
	AttemptsHist  map[int]int `json:"jobs_by_attempts"` // how many jobs ended with N attempts (dead ones included)

	// ThroughputPerSec is (succeeded - 1) divided by the time from the first
	// success to the last. It leaves out the ramp-up before the first job finishes
	// and includes the tail where slots go idle, so with few jobs per slot it
	// understates the steady rate.
	ThroughputPerSec float64 `json:"throughput_jobs_per_second"`
	SpanSecs         float64 `json:"first_to_last_success_seconds"`

	// AttemptRows counts the bookkeeping rows the handlers wrote. It is 0 in a lean
	// run, which is how the raw results show that a throughput run really was lean.
	AttemptRows int `json:"bookkeeping_attempt_rows"`

	EndToEnd  Dist `json:"end_to_end_seconds"` // enqueue -> success, succeeded jobs
	QueueWait Dist `json:"queue_wait_seconds"` // enqueue -> first handler start; empty when Lean

	Mock mockllm.Stats `json:"mock_llm_stats"`
}

// RunLoad runs one load experiment and returns what the database says happened.
// It deletes the run's rows afterwards. A job that dead-letters or never finishes is
// a result, not an error.
func RunLoad(ctx context.Context, p LoadParams) (LoadResult, error) {
	closed, open := p.Jobs > 0, p.Rate > 0 && p.Duration > 0
	if closed == open {
		return LoadResult{}, errors.New("benchwork: set either Jobs (closed loop) or Rate and Duration (open loop)")
	}
	if p.Workers < 1 || p.Concurrency < 1 {
		return LoadResult{}, errors.New("benchwork: need at least one worker and one slot")
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 5
	}
	if p.Warmup == 0 {
		p.Warmup = 2 * time.Second
	}
	if p.DrainTimeout == 0 {
		p.DrainTimeout = 5 * time.Minute
	}
	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	owner, err := pgxpool.New(ctx, p.OwnerURL)
	if err != nil {
		return LoadResult{}, err
	}
	defer owner.Close()
	if err := Setup(ctx, owner); err != nil {
		return LoadResult{}, err
	}
	pool, err := pgxpool.New(ctx, p.DatabaseURL)
	if err != nil {
		return LoadResult{}, err
	}
	defer pool.Close()

	mock, err := mockllm.New(p.Mock)
	if err != nil {
		return LoadResult{}, err
	}
	mockSrv := httptest.NewServer(mock.Handler())
	defer mockSrv.Close()

	run := newRunID()
	res := LoadResult{Run: run}
	defer func() {
		if err := cleanup(context.WithoutCancel(ctx), owner, run); err != nil {
			logf("cleanup of run %s failed: %v", run, err)
		}
	}()

	if closed {
		if _, err := Enqueue(ctx, pool, run, p.Jobs, p.MaxAttempts); err != nil {
			return res, err
		}
		res.Enqueued = p.Jobs
	}

	procs := make([]*proc, p.Workers)
	defer stopAll(procs)
	for i := range procs {
		procs[i], err = startWorker(workerSpec{
			Bin: p.WorkerBin, DatabaseURL: p.DatabaseURL, MockURL: mockSrv.URL, ID: "w" + strconv.Itoa(i+1),
			Concurrency: p.Concurrency, Lease: p.Lease, Poll: p.Poll, Lean: p.Lean,
		})
		if err != nil {
			return res, err
		}
	}

	if open {
		if err := sleepCtx(ctx, p.Warmup); err != nil {
			return res, err
		}
		n, err := arrivals(ctx, pool, run, p)
		res.Enqueued = n
		if err != nil {
			return res, err
		}
		logf("enqueued %d jobs over %v", n, p.Duration)
	}

	err = waitFor(ctx, 100*time.Millisecond, p.DrainTimeout, func() (bool, error) {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'run' = $2 AND state NOT IN ('succeeded', 'dead')`,
			Kind, run).Scan(&n)
		return n == 0, err
	})
	if err != nil {
		logf("jobs did not all finish: %v", err) // the counts say how many did not
	}
	res.Mock = mock.Stats()
	return res, collectLoad(ctx, pool, run, p.Lean, &res)
}

// arrivals enqueues one job at a time as a Poisson process and returns how many it
// enqueued. Each job is its own transaction, as a request handler would enqueue it.
func arrivals(ctx context.Context, pool *pgxpool.Pool, run string, p LoadParams) (int, error) {
	rng := rand.New(rand.NewPCG(p.Seed, 2))
	start := time.Now()
	at, n := time.Duration(0), 0
	for {
		at += time.Duration(rng.ExpFloat64() / p.Rate * float64(time.Second))
		if at >= p.Duration {
			return n, nil
		}
		if err := sleepCtx(ctx, time.Until(start.Add(at))); err != nil {
			return n, err
		}
		if _, err := Enqueue(ctx, pool, run, 1, p.MaxAttempts); err != nil {
			return n, err
		}
		n++
	}
}

func collectLoad(ctx context.Context, pool *pgxpool.Pool, run string, lean bool, res *LoadResult) error {
	var span *float64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'succeeded'),
		       count(*) FILTER (WHERE state = 'dead'),
		       count(*) FILTER (WHERE state NOT IN ('succeeded', 'dead')),
		       COALESCE(sum(attempts), 0)::int,
		       extract(epoch FROM max(finished_at) FILTER (WHERE state = 'succeeded') - min(finished_at) FILTER (WHERE state = 'succeeded'))
		FROM jobs WHERE kind = $1 AND payload->>'run' = $2`, Kind, run).
		Scan(&res.Succeeded, &res.Dead, &res.Unfinished, &res.TotalAttempts, &span)
	if err != nil {
		return err
	}
	if span != nil && *span > 0 && res.Succeeded > 1 {
		res.SpanSecs = *span
		res.ThroughputPerSec = float64(res.Succeeded-1) / *span
	}
	if res.DuplicateFx, res.MissingFx, err = effectCounts(ctx, pool, run); err != nil {
		return err
	}

	err = pool.QueryRow(ctx, `SELECT count(*)::int FROM bench.attempts a JOIN jobs j ON j.id = a.job_id WHERE j.kind = $1 AND j.payload->>'run' = $2`,
		Kind, run).Scan(&res.AttemptRows)
	if err != nil {
		return err
	}

	res.AttemptsHist = map[int]int{}
	rows, err := pool.Query(ctx, `SELECT attempts, count(*)::int FROM jobs WHERE kind = $1 AND payload->>'run' = $2 GROUP BY attempts`, Kind, run)
	if err != nil {
		return err
	}
	for rows.Next() {
		var attempts, n int
		if err := rows.Scan(&attempts, &n); err != nil {
			rows.Close()
			return err
		}
		res.AttemptsHist[attempts] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	res.EndToEnd, err = secondsDist(ctx, pool, `
		SELECT extract(epoch FROM finished_at - created_at)::float8 FROM jobs
		WHERE kind = $1 AND payload->>'run' = $2 AND state = 'succeeded'`, Kind, run)
	if err != nil || lean {
		return err
	}
	res.QueueWait, err = secondsDist(ctx, pool, `
		SELECT extract(epoch FROM a.started_at - j.created_at)::float8
		FROM bench.attempts a JOIN jobs j ON j.id = a.job_id
		WHERE j.kind = $1 AND j.payload->>'run' = $2 AND a.attempt = 1`, Kind, run)
	return err
}

// effectCounts returns the effect rows beyond one per job (duplicates) and the
// succeeded jobs with none (missing).
func effectCounts(ctx context.Context, pool *pgxpool.Pool, run string) (dup, missing int, err error) {
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(sum(GREATEST(c - 1, 0)), 0)::int,
		       count(*) FILTER (WHERE c = 0 AND state = 'succeeded')::int
		FROM (SELECT j.state, count(e.id) AS c
		      FROM jobs j LEFT JOIN bench.effects e ON e.job_id = j.id
		      WHERE j.kind = $1 AND j.payload->>'run' = $2 GROUP BY j.id) t`, Kind, run).Scan(&dup, &missing)
	return dup, missing, err
}

func secondsDist(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (Dist, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return Dist{}, err
	}
	defer rows.Close()
	var xs []float64
	for rows.Next() {
		var x float64
		if err := rows.Scan(&x); err != nil {
			return Dist{}, err
		}
		xs = append(xs, x)
	}
	return Distribution(xs), rows.Err()
}

// cleanup deletes a run's jobs and bookkeeping so repeated runs do not pile rows
// into the shared database.
func cleanup(ctx context.Context, owner *pgxpool.Pool, run string) error {
	for _, q := range []string{
		`DELETE FROM bench.effects WHERE job_id IN (SELECT id FROM jobs WHERE kind = $1 AND payload->>'run' = $2)`,
		`DELETE FROM bench.attempts WHERE job_id IN (SELECT id FROM jobs WHERE kind = $1 AND payload->>'run' = $2)`,
		`DELETE FROM jobs WHERE kind = $1 AND payload->>'run' = $2`,
	} {
		if _, err := owner.Exec(ctx, q, Kind, run); err != nil {
			return err
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newRunID() string { return fmt.Sprintf("load-%d", time.Now().UnixNano()) }
