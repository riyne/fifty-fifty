package benchwork

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/mockllm"
)

// KillPoint says when in a job's life the victim worker is killed.
type KillPoint string

const (
	// KillDuringCall kills the victim while its jobs are waiting on the mock LLM
	// (phase one), so nothing of theirs has been written.
	KillDuringCall KillPoint = "call"
	// KillDuringCommit kills it while a job's effect row is inserted in a
	// transaction that has not committed (needs CommitDelay > 0), so the effect
	// exists only in an open transaction when the process dies.
	KillDuringCommit KillPoint = "commit"
)

// RecoveryParams describes one trial of "kill -9 a worker mid-job".
type RecoveryParams struct {
	DatabaseURL string // the app role: workers and the harness's reads
	OwnerURL    string // the owner role: creates the bench schema
	WorkerBin   string // path to a built cmd/benchworker

	Workers     int // worker processes; one is killed. At least 2: the survivors do the recovery.
	Concurrency int // jobs in flight per worker
	Jobs        int // jobs in the trial; at least 2 * Workers * Concurrency so every worker stays busy
	Lease       time.Duration
	Poll        time.Duration
	CommitDelay time.Duration
	Kill        KillPoint
	// KillJitter delays the kill by a random time in [0, KillJitter) after the kill
	// point is reached (seeded by Mock.Seed), so across trials the victim dies at
	// different ages of its leases instead of always just after its first claim.
	KillJitter time.Duration
	Mock       mockllm.Config // the in-process mock LLM the workers call

	// DrainTimeout bounds the wait for every job to finish after the kill. Default
	// 2 minutes.
	DrainTimeout time.Duration
	Logf         func(format string, args ...any)
}

// RecoveryResult is what one trial measured. Durations are seconds.
type RecoveryResult struct {
	Run           string `json:"run"`
	Jobs          int    `json:"jobs"`
	Succeeded     int    `json:"succeeded"`
	Dead          int    `json:"dead"`
	Unfinished    int    `json:"unfinished"`
	LostJobs      int    `json:"lost_jobs"`         // jobs - succeeded
	DuplicateFx   int    `json:"duplicate_effects"` // effect rows beyond the first, summed over jobs
	MissingFx     int    `json:"missing_effects"`   // succeeded jobs with no effect row
	TotalAttempts int    `json:"total_attempts"`    // handler starts, equal to LLM calls begun
	ExtraAttempts int    `json:"extra_attempts"`    // total_attempts - jobs

	Victim         string    `json:"victim"`
	KilledBySignal string    `json:"killed_by_signal"`
	Orphans        int       `json:"orphans"` // jobs the victim had started and not committed when it died
	OrphanAttempts []int     `json:"orphan_attempts"`
	RecoverySecs   []float64 `json:"recovery_seconds"` // kill until each orphan succeeded (database clock)
	DrainSecs      float64   `json:"drain_seconds"`    // kill until the last job of the trial succeeded
}

// RunRecovery runs one trial: enqueue the jobs, start the workers, kill one with
// SIGKILL at the requested point, let the others finish, then check what is in the
// database. It returns an error for a harness failure; a lost job or a duplicate
// effect is a result, not an error, so the caller decides what is acceptable.
func RunRecovery(ctx context.Context, p RecoveryParams) (RecoveryResult, error) {
	if p.Workers < 2 || p.Concurrency < 1 || p.Jobs < 2*p.Workers*p.Concurrency {
		return RecoveryResult{}, errors.New("benchwork: need at least 2 workers and jobs >= 2 * workers * concurrency")
	}
	if p.Kill == KillDuringCommit && p.CommitDelay <= 0 {
		return RecoveryResult{}, errors.New("benchwork: KillDuringCommit needs a CommitDelay")
	}
	if p.DrainTimeout == 0 {
		p.DrainTimeout = 2 * time.Minute
	}
	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	owner, err := pgxpool.New(ctx, p.OwnerURL)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer owner.Close()
	if err := Setup(ctx, owner); err != nil {
		return RecoveryResult{}, err
	}
	pool, err := pgxpool.New(ctx, p.DatabaseURL)
	if err != nil {
		return RecoveryResult{}, err
	}
	defer pool.Close()

	mock, err := mockllm.New(p.Mock)
	if err != nil {
		return RecoveryResult{}, err
	}
	mockSrv := httptest.NewServer(mock.Handler())
	defer mockSrv.Close()

	run := uuid.NewString()
	res := RecoveryResult{Run: run, Jobs: p.Jobs}
	if _, err := Enqueue(ctx, pool, run, p.Jobs, 5); err != nil {
		return res, err
	}

	procs := make([]*proc, p.Workers)
	for i := range procs {
		id := "w" + strconv.Itoa(i+1)
		procs[i], err = startWorker(workerSpec{
			Bin: p.WorkerBin, DatabaseURL: p.DatabaseURL, MockURL: mockSrv.URL, ID: id,
			Concurrency: p.Concurrency, Lease: p.Lease, Poll: p.Poll, CommitDelay: p.CommitDelay,
		})
		if err != nil {
			stopAll(procs)
			return res, err
		}
	}
	defer stopAll(procs)
	victim := procs[0]
	res.Victim = victim.id

	// Wait for the moment to kill at.
	need := p.Concurrency // a saturated victim
	extra := ""
	if p.Kill == KillDuringCommit {
		need, extra = 1, " AND a.commit_started_at IS NOT NULL"
	}
	err = waitFor(ctx, 10*time.Millisecond, p.DrainTimeout, func() (bool, error) {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM jobs j JOIN bench.attempts a ON a.job_id = j.id AND a.attempt = j.attempts
			WHERE j.kind = $1 AND j.payload->>'run' = $2 AND j.state = 'running' AND a.worker = $3`+extra,
			Kind, run, victim.id).Scan(&n)
		return n >= need, err
	})
	if err != nil {
		return res, fmt.Errorf("waiting for the victim to reach the kill point (%s): %w", p.Kill, err)
	}

	if p.KillJitter > 0 {
		jitter := time.Duration(rand.New(rand.NewPCG(p.Mock.Seed, 1)).Int64N(int64(p.KillJitter)))
		select {
		case <-time.After(jitter):
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
	if err := victim.cmd.Process.Kill(); err != nil { // SIGKILL: no handlers, no deferred cleanup
		return res, fmt.Errorf("kill: %w", err)
	}
	var killAt time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&killAt); err != nil {
		return res, err
	}
	werr := victim.wait()
	var ee *exec.ExitError
	if !errors.As(werr, &ee) {
		return res, fmt.Errorf("the victim did not die from the kill: %v", werr)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return res, fmt.Errorf("the victim did not die from SIGKILL: %v", werr)
	}
	res.KilledBySignal = "SIGKILL"
	logf("killed %s (%s) at the %s point", victim.id, p.Kill, p.Kill)

	// Let the survivors finish everything.
	err = waitFor(ctx, 50*time.Millisecond, p.DrainTimeout, func() (bool, error) {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'run' = $2 AND state NOT IN ('succeeded', 'dead')`,
			Kind, run).Scan(&n)
		return n == 0, err
	})
	if err != nil {
		logf("jobs did not all finish: %v", err) // the counts below say how many did not
	}

	return res, collect(ctx, pool, run, victim.id, killAt, &res)
}

// collect reads the outcome of the trial from the database.
func collect(ctx context.Context, pool *pgxpool.Pool, run, victim string, killAt time.Time, res *RecoveryResult) error {
	err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'succeeded'),
		       count(*) FILTER (WHERE state = 'dead'),
		       count(*) FILTER (WHERE state NOT IN ('succeeded', 'dead')),
		       COALESCE(extract(epoch FROM max(finished_at) FILTER (WHERE state = 'succeeded') - $3::timestamptz), 0)
		FROM jobs WHERE kind = $1 AND payload->>'run' = $2`, Kind, run, killAt).
		Scan(&res.Succeeded, &res.Dead, &res.Unfinished, &res.DrainSecs)
	if err != nil {
		return err
	}
	res.LostJobs = res.Jobs - res.Succeeded

	if res.DuplicateFx, res.MissingFx, err = effectCounts(ctx, pool, run); err != nil {
		return err
	}
	err = pool.QueryRow(ctx, `
		SELECT count(*)::int FROM bench.attempts a JOIN jobs j ON j.id = a.job_id
		WHERE j.kind = $1 AND j.payload->>'run' = $2`, Kind, run).Scan(&res.TotalAttempts)
	if err != nil {
		return err
	}
	res.ExtraAttempts = res.TotalAttempts - res.Jobs

	// Orphans: the victim started an attempt that never produced a committed effect.
	// Recovery time: from the kill to the job's success, on the database's clock.
	rows, err := pool.Query(ctx, `
		SELECT j.attempts, COALESCE(extract(epoch FROM j.finished_at - $3::timestamptz), -1)
		FROM bench.attempts a
		JOIN jobs j ON j.id = a.job_id
		WHERE j.kind = $1 AND j.payload->>'run' = $2 AND a.worker = $4
		  AND NOT EXISTS (SELECT 1 FROM bench.effects e WHERE e.job_id = a.job_id AND e.attempt = a.attempt)
		ORDER BY j.finished_at NULLS LAST`, Kind, run, killAt, victim)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var attempts int
		var secs float64
		if err := rows.Scan(&attempts, &secs); err != nil {
			return err
		}
		res.Orphans++
		res.OrphanAttempts = append(res.OrphanAttempts, attempts)
		res.RecoverySecs = append(res.RecoverySecs, secs) // -1: the job never finished
	}
	return rows.Err()
}

type proc struct {
	id   string
	cmd  *exec.Cmd
	once sync.Once
	done chan struct{}
	err  error
}

// workerSpec is how to start one benchworker process.
type workerSpec struct {
	Bin, DatabaseURL, MockURL, ID string
	Concurrency                   int
	Lease, Poll, CommitDelay      time.Duration
	Lean                          bool
}

func startWorker(w workerSpec) (*proc, error) {
	args := []string{
		"--mock-url", w.MockURL, "--worker-id", w.ID,
		"--concurrency", strconv.Itoa(w.Concurrency),
		"--lease", w.Lease.String(), "--poll", w.Poll.String(),
		"--commit-delay", w.CommitDelay.String(), "--grace", "2s",
	}
	if w.Lean {
		args = append(args, "--lean")
	}
	cmd := exec.Command(w.Bin, args...)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+w.DatabaseURL)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", w.ID, err)
	}
	pr := &proc{id: w.ID, cmd: cmd, done: make(chan struct{})}
	go func() { pr.err = cmd.Wait(); close(pr.done) }()
	return pr, nil
}

// wait returns the process's exit error once it has exited.
func (p *proc) wait() error { <-p.done; return p.err }

// stopAll asks the live workers to stop, and kills any that do not.
func stopAll(procs []*proc) {
	for _, p := range procs {
		if p == nil {
			continue
		}
		select {
		case <-p.done:
			continue
		default:
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
}

func waitFor(ctx context.Context, every, timeout time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %v", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
