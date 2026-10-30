// Package worker runs queue jobs: it claims them, calls the handler registered
// for their kind, keeps their lease alive while they run, and reports the result.
//
// The mechanism (leases, fencing, retries) lives in internal/queue; this package
// is the loop around it. One Run call is one worker process; any number can run
// against the same database.
//
// A handler is two-phase:
//
//	func(ctx, job) (Commit, error)
//
// Phase one does the slow or external work (an LLM call, a provider call) with NO
// database transaction open, and returns what should be written. Phase two, the
// returned Commit, runs inside the transaction that also marks the job succeeded,
// so the handler's database effects and "this job is done" commit together or not
// at all (queue.CompleteWith). If the lease was lost meanwhile, the transaction
// rolls back and the Commit's writes never happen. Only phase two is
// exactly-once; phase one may run more than once (a retry after a crash, a lease
// that expired during a slow call), so it must not have effects of its own that
// matter, or must make them idempotent (a provider call keyed by the job's id).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
)

// MinLease is the shortest lease Run accepts. Heartbeats fire every Lease/3, so
// a lease shorter than a database round trip could never be kept alive.
const MinLease = 30 * time.Millisecond

// Commit writes a handler's database effects inside the completion transaction.
// It must not commit or roll back tx. A nil Commit means "no database effects".
type Commit func(ctx context.Context, tx pgx.Tx) error

// Handler does the work for one job. Its context is canceled when the worker
// loses the job's lease (another worker took it over) or, after the grace period,
// when the worker is shutting down; a handler doing slow work should honor it.
// Returning an error fails the attempt (retried with backoff, or dead-lettered
// on the last one); a panic is treated the same way.
type Handler func(ctx context.Context, job queue.Claimed) (Commit, error)

// Config configures Run. Zero values take the defaults noted.
type Config struct {
	Pool     *pgxpool.Pool
	Handlers map[string]Handler // by job kind; the worker claims only these kinds
	Periodic []queue.PeriodicJob

	Concurrency      int           // jobs in flight at once; default 4
	PollInterval     time.Duration // wait when the queue is empty; default 1s
	Lease            time.Duration // lease per claim; heartbeats every Lease/3; default 30s
	ReapInterval     time.Duration // how often to dead-letter exhausted jobs; default 10s
	PeriodicInterval time.Duration // how often to make sure each periodic job exists; default 1m
	ShutdownGrace    time.Duration // how long in-flight jobs may finish after shutdown begins; default 20s

	Metrics *Metrics // optional; counts job outcomes for the metrics endpoint

	Backoff queue.Backoff // retry delay policy; default queue.DefaultBackoff
	Rand    queue.Rand    // default queue.SystemRand
	Logger  *slog.Logger  // default slog.Default()
}

func (c *Config) setDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.Concurrency == 0 {
		c.Concurrency = 4
	}
	def(&c.PollInterval, time.Second)
	def(&c.Lease, 30*time.Second)
	def(&c.ReapInterval, 10*time.Second)
	def(&c.PeriodicInterval, time.Minute)
	def(&c.ShutdownGrace, 20*time.Second)
	if c.Backoff == (queue.Backoff{}) {
		c.Backoff = queue.DefaultBackoff
	}
	if c.Rand == nil {
		c.Rand = queue.SystemRand
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

func (c *Config) validate() error {
	switch {
	case c.Pool == nil:
		return errors.New("worker: Pool is required")
	case len(c.Handlers) == 0:
		return errors.New("worker: at least one handler is required")
	case c.Concurrency < 1 || c.Concurrency > queue.MaxClaimBatch:
		return fmt.Errorf("worker: concurrency must be 1-%d", queue.MaxClaimBatch)
	case c.PollInterval < 0 || c.ReapInterval < 0 || c.PeriodicInterval < 0 || c.ShutdownGrace < 0:
		return errors.New("worker: durations must not be negative")
	case c.Lease < MinLease:
		return fmt.Errorf("worker: lease must be at least %v (heartbeats run every third of it)", MinLease)
	}
	for kind, h := range c.Handlers {
		if h == nil {
			return fmt.Errorf("worker: nil handler for %q", kind)
		}
	}
	for _, p := range c.Periodic {
		if _, ok := c.Handlers[p.Kind]; !ok {
			return fmt.Errorf("worker: periodic job %q has no handler", p.Kind)
		}
	}
	return nil
}

// Run claims and runs jobs until ctx is canceled, then lets in-flight jobs finish
// (up to ShutdownGrace) and returns nil. Anything still running after the grace
// period has its context canceled and is handed back to the queue for another
// worker. It returns an error only for an invalid Config.
func Run(ctx context.Context, cfg Config) error {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return err
	}
	w := &worker{
		cfg:  cfg,
		log:  cfg.Logger,
		slot: make(chan struct{}, cfg.Concurrency),
		wake: make(chan struct{}, 1),
	}
	for kind := range cfg.Handlers {
		w.kinds = append(w.kinds, kind)
	}
	sort.Strings(w.kinds)

	// Jobs get their own context, detached from ctx: canceling ctx means "stop
	// taking work", not "abandon what you are doing". It is canceled only when the
	// grace period runs out.
	var cancelJobs context.CancelFunc
	w.jobsCtx, cancelJobs = context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()

	var housekeeping sync.WaitGroup
	housekeeping.Add(1)
	go func() { defer housekeeping.Done(); w.housekeep(ctx) }()

	w.log.Info("worker started", "kinds", w.kinds, "concurrency", cfg.Concurrency, "lease", cfg.Lease)
	w.poll(ctx)

	w.log.Info("worker stopping", "in_flight", len(w.slot), "grace", cfg.ShutdownGrace)
	done := make(chan struct{})
	go func() { w.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(cfg.ShutdownGrace):
		w.log.Warn("grace period over; canceling jobs still running", "in_flight", len(w.slot))
		cancelJobs()
		<-done
	}
	housekeeping.Wait()
	w.log.Info("worker stopped")
	return nil
}

type worker struct {
	cfg     Config
	log     *slog.Logger
	kinds   []string
	jobsCtx context.Context
	slot    chan struct{} // holds one token per job in flight
	wake    chan struct{} // signaled when a slot frees up
	jobs    sync.WaitGroup
}

// poll claims jobs into free slots until ctx is canceled.
func (w *worker) poll(ctx context.Context) {
	for ctx.Err() == nil {
		free := cap(w.slot) - len(w.slot)
		if free > 0 {
			claimed, err := queue.ClaimJobs(ctx, w.cfg.Pool, queue.ClaimParams{Kinds: w.kinds, Limit: free, Lease: w.cfg.Lease})
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				w.log.Error("claim failed", "err", err)
			default:
				for _, c := range claimed {
					w.slot <- struct{}{} // cannot block: only this goroutine adds, and free was counted
					w.jobs.Add(1)
					go w.runJob(c)
				}
				if len(claimed) == free {
					continue // took everything we asked for; there may be more, and next time round we wait for a slot
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

// housekeep runs the periodic chores: dead-lettering exhausted jobs, and making
// sure each periodic job exists for the current period.
func (w *worker) housekeep(ctx context.Context) {
	reap := time.NewTicker(w.cfg.ReapInterval)
	periodic := time.NewTicker(w.cfg.PeriodicInterval)
	defer reap.Stop()
	defer periodic.Stop()
	w.schedulePeriodic(ctx) // do not wait a whole interval for the first one
	for {
		select {
		case <-ctx.Done():
			return
		case <-reap.C:
			ids, err := queue.ReapExhausted(ctx, w.cfg.Pool)
			if err != nil && ctx.Err() == nil {
				w.log.Error("reap failed", "err", err)
			}
			if len(ids) > 0 {
				w.log.Warn("dead-lettered jobs whose last attempt's lease expired", "count", len(ids), "job_ids", ids)
			}
		case <-periodic.C:
			w.schedulePeriodic(ctx)
		}
	}
}

func (w *worker) schedulePeriodic(ctx context.Context) {
	for _, p := range w.cfg.Periodic {
		created, err := queue.EnqueuePeriodic(ctx, w.cfg.Pool, p)
		if err != nil && ctx.Err() == nil {
			w.log.Error("schedule periodic job failed", "kind", p.Kind, "err", err)
		}
		if created {
			w.log.Info("scheduled periodic job", "kind", p.Kind, "every", p.Every)
		}
	}
}

// runJob runs one claimed job to a result and reports it.
func (w *worker) runJob(c queue.Claimed) {
	defer func() {
		<-w.slot
		w.jobs.Done()
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}()
	log := w.log.With("job_id", c.ID, "kind", c.Kind, "attempt", c.Attempt, "max_attempts", c.MaxAttempts)

	// jobCtx ends when the handler is done, when the lease is lost (cause
	// queue.ErrLeaseLost) or when the grace period runs out.
	jobCtx, cancel := context.WithCancelCause(w.jobsCtx)
	defer cancel(nil)
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); w.heartbeat(jobCtx, cancel, c, log) }()

	start := time.Now()
	err := w.runHandler(jobCtx, c)
	cancel(nil) // stop the heartbeat; the completion below is one short transaction
	<-hbDone

	switch {
	case err == nil:
		w.cfg.Metrics.finish(c.Kind, OutcomeSucceeded, c.Attempt)
		log.Info("job succeeded", "took", time.Since(start))
	case errors.Is(err, queue.ErrLeaseLost) || errors.Is(context.Cause(jobCtx), queue.ErrLeaseLost):
		// Someone else holds the job now and nothing of ours was committed. Not a
		// failure to report: the other holder's attempt is the one that counts.
		w.cfg.Metrics.finish(c.Kind, OutcomeLeaseLost, c.Attempt)
		log.Warn("lease lost; abandoning job", "took", time.Since(start))
	default:
		w.fail(c, err, log)
	}
}

// runHandler calls the handler and, if it succeeds, completes the job together
// with the handler's Commit. A panic becomes an error.
func (w *worker) runHandler(ctx context.Context, c queue.Claimed) (err error) {
	h, ok := w.cfg.Handlers[c.Kind]
	if !ok {
		return fmt.Errorf("no handler for kind %q", c.Kind) // unreachable: only registered kinds are claimed
	}
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("handler panicked", "job_id", c.ID, "kind", c.Kind, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()

	commit, err := h(ctx, c)
	if err != nil {
		return err
	}
	// Complete with the lease still ours or not at all. If ctx is already
	// canceled (lease lost, or shutdown past the grace period) this fails and the
	// caller sorts out why.
	return queue.CompleteWith(ctx, w.cfg.Pool, c, func(ctx context.Context, tx pgx.Tx) error {
		if commit == nil {
			return nil
		}
		return commit(ctx, tx)
	})
}

// heartbeat extends the lease every Lease/3 until ctx ends. If the job has been
// taken over it cancels the handler's context, so slow work stops early.
func (w *worker) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, c queue.Claimed, log *slog.Logger) {
	t := time.NewTicker(w.cfg.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := queue.Heartbeat(ctx, w.cfg.Pool, c, w.cfg.Lease)
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, queue.ErrLeaseLost):
			log.Warn("heartbeat found the lease taken over; canceling the handler")
			cancel(queue.ErrLeaseLost)
			return
		default:
			// A database hiccup. Keep trying: the lease has two more intervals of slack.
			log.Warn("heartbeat failed", "err", err)
		}
	}
}

// fail reports a failed attempt: retried after a backoff delay, or dead-lettered
// on the last attempt. A job interrupted by shutdown is handed back at once and
// with its attempt already counted (claim counted it); the next worker is not made
// to wait out a backoff for something that was not the job's fault.
func (w *worker) fail(c queue.Claimed, cause error, log *slog.Logger) {
	p := queue.FailParams{Error: cause.Error(), RetryIn: w.cfg.Backoff.Delay(c.Attempt, w.cfg.Rand)}
	if w.jobsCtx.Err() != nil {
		p = queue.FailParams{Error: "worker shut down mid-job: " + cause.Error()}
	}
	// Report even if shutdown canceled the job's context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.jobsCtx), 10*time.Second)
	defer cancel()
	state, err := queue.Fail(ctx, w.cfg.Pool, c, p)
	switch {
	case errors.Is(err, queue.ErrLeaseLost):
		w.cfg.Metrics.finish(c.Kind, OutcomeLeaseLost, c.Attempt)
		log.Warn("job failed but the lease was already lost", "err", cause)
	case err != nil:
		// The lease will expire and the job will be claimed again: no job is lost.
		// Not counted: what happens to the job is decided by the next claim.
		log.Error("could not report job failure", "err", err, "cause", cause)
	case state == queue.StateDead:
		w.cfg.Metrics.finish(c.Kind, OutcomeDead, c.Attempt)
		log.Error("job failed on its last attempt; dead-lettered", "err", cause)
	default:
		w.cfg.Metrics.finish(c.Kind, OutcomeRetry, c.Attempt)
		log.Warn("job failed; will retry", "err", cause, "retry_in", p.RetryIn)
	}
}
