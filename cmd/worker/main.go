// Command worker is the Fifty Fifty job worker and its operator commands.
//
//	worker run [flags]             claim and run jobs until SIGINT/SIGTERM; --metrics-addr serves /metrics
//	worker requeue --kind K        give every dead job of kind K a fresh set of attempts
//	worker requeue --id ID [--id ID ...]   the same for specific jobs
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/payclient"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/worker"
)

const usage = `usage:
  worker run [--concurrency N] [--lease D] [--poll D] [--grace D] [--metrics-addr HOST:PORT]
  worker requeue --kind KIND
  worker requeue --id JOB_ID [--id JOB_ID ...]

run claims jobs and runs their handlers until SIGINT or SIGTERM, then lets the
jobs in flight finish (up to --grace). Defaults: 4 jobs at once, 30s lease, 1s
poll, 20s grace. --metrics-addr (off by default) serves Prometheus metrics at
/metrics; the endpoint has no authentication, so bind it to a private interface
(for example 127.0.0.1:9102).

requeue moves dead-lettered jobs back to the queue with a fresh set of attempts
(their last_error is kept). It needs --kind or --id; there is no "all".
Needs DATABASE_URL. If PAYMENTS_URL (the base URL of the payment provider, for
example http://127.0.0.1:8090) is set, the worker also runs settlement.execute and
settlement.reconcile jobs and the settlement watchdog; without it those jobs wait.`

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		slog.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "requeue" && args[0] != "run") {
		fmt.Fprintln(stderr, usage)
		return errUsage
	}
	// Parse before connecting, so a typo does not need a database.
	var (
		reqParams   queue.RequeueParams
		runCfg      worker.Config
		metricsAddr string
		err         error
	)
	if args[0] == "requeue" {
		reqParams, err = parseRequeue(args[1:], stderr)
	} else {
		runCfg, metricsAddr, err = parseRun(args[1:], stderr)
	}
	if err != nil {
		return err
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	if args[0] == "requeue" {
		return requeue(ctx, pool, reqParams, stdout)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCfg.Pool = pool
	runCfg.Handlers, err = handlers(pool, os.Getenv("PAYMENTS_URL"))
	if err != nil {
		return err
	}
	runCfg.Periodic = []queue.PeriodicJob{worker.CleanupSchedule, worker.PruneSchedule}
	if _, ok := runCfg.Handlers[settlements.KindWatchdog]; ok {
		runCfg.Periodic = append(runCfg.Periodic, settlements.WatchdogSchedule)
	}

	if metricsAddr != "" {
		kinds := make([]string, 0, len(runCfg.Handlers))
		for k := range runCfg.Handlers {
			kinds = append(kinds, k)
		}
		runCfg.Metrics = worker.NewMetrics()
		ln, err := net.Listen("tcp", metricsAddr)
		if err != nil {
			return fmt.Errorf("metrics listener: %w", err)
		}
		slog.Info("metrics listening", "addr", ln.Addr().String())
		served := make(chan struct{})
		go func() { defer close(served); serveMetrics(ctx, ln, runCfg.Metrics.Handler(pool, kinds)) }()
		defer func() { <-served }() // Run returns when ctx is canceled; the server stops on the same signal
	}
	return worker.Run(ctx, runCfg)
}

// serveMetrics serves h at /metrics on ln until ctx is canceled, then shuts down.
// Anything else is a 404. The timeouts are short: a scrape is a small local request.
func serveMetrics(ctx context.Context, ln net.Listener, h http.Handler) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 14,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("metrics server stopped", "err", err)
	}
}

// paymentsTimeout bounds one call to the payment provider. Running out of it is an
// ambiguous result (the settlement becomes unknown and is reconciled), not a failure.
const paymentsTimeout = 10 * time.Second

// handlers is every job kind this worker can run. Settlement execution, reconciliation
// and the watchdog are registered only when paymentsURL (the PAYMENTS_URL environment variable, the base URL of the
// payment provider) is set: without a provider the worker must not claim settlement
// jobs it cannot run, and they wait in the queue for a worker that can. Receipts
// arrive in M6.
func handlers(pool *pgxpool.Pool, paymentsURL string) (map[string]worker.Handler, error) {
	h := map[string]worker.Handler{
		worker.KindIdempotencyCleanup: worker.IdempotencyCleanup(pool),
		worker.KindJobsPrune:          worker.JobsPrune(pool, worker.JobRetention),
	}
	if paymentsURL != "" {
		c, err := payclient.New(paymentsURL, paymentsTimeout)
		if err != nil {
			return nil, fmt.Errorf("PAYMENTS_URL: %w", err)
		}
		h[settlements.KindExecute] = settlements.ExecuteHandler(pool, c)
		h[settlements.KindReconcile] = settlements.ReconcileHandler(pool, c)
		h[settlements.KindWatchdog] = settlements.WatchdogHandler(pool, settlements.WatchdogConfig{})
	}
	return h, nil
}

func parseRun(args []string, stderr io.Writer) (cfg worker.Config, metricsAddr string, err error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&cfg.Concurrency, "concurrency", 4, "jobs in flight at once")
	fs.DurationVar(&cfg.Lease, "lease", 30*time.Second, "lease per claimed job; heartbeats run every third of it")
	fs.DurationVar(&cfg.PollInterval, "poll", time.Second, "wait between polls when the queue is empty")
	fs.DurationVar(&cfg.ShutdownGrace, "grace", 20*time.Second, "how long jobs in flight may finish after SIGTERM")
	fs.StringVar(&metricsAddr, "metrics-addr", "", "serve Prometheus metrics at /metrics on this address (off by default; no authentication)")
	if err := fs.Parse(args); err != nil {
		return cfg, "", errUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return cfg, "", errUsage
	}
	return cfg, metricsAddr, nil
}

type idList []uuid.UUID

func (l *idList) String() string { return fmt.Sprint(*l) }
func (l *idList) Set(v string) error {
	id, err := uuid.Parse(v)
	if err != nil {
		return fmt.Errorf("not a job id: %q", v)
	}
	*l = append(*l, id)
	return nil
}

func parseRequeue(args []string, stderr io.Writer) (queue.RequeueParams, error) {
	var ids idList
	fs := flag.NewFlagSet("requeue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("kind", "", "requeue every dead job of this kind")
	fs.Var(&ids, "id", "requeue this job (repeatable)")
	if err := fs.Parse(args); err != nil {
		return queue.RequeueParams{}, errUsage
	}
	if fs.NArg() != 0 || (*kind == "" && len(ids) == 0) {
		fmt.Fprintln(stderr, usage)
		return queue.RequeueParams{}, errUsage
	}
	return queue.RequeueParams{IDs: ids, Kind: *kind}, nil
}

// requeue prints one requeued job id per line on stdout; the count goes to the log.
func requeue(ctx context.Context, pool *pgxpool.Pool, p queue.RequeueParams, stdout io.Writer) error {
	ids, err := queue.Requeue(ctx, pool, p)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(stdout, id)
	}
	slog.Info("requeued dead jobs", "count", len(ids), "kind", p.Kind, "ids_requested", len(p.IDs))
	return nil
}
