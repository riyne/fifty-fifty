// Command worker is the Fifty Fifty job worker and its operator commands.
//
//	worker run [flags]             claim and run jobs until SIGINT/SIGTERM; --metrics-addr serves /metrics
//	worker requeue --kind K        give every dead job of kind K a fresh set of attempts
//	worker requeue --id ID [--id ID ...]   the same for specific jobs
//	worker delete-dead [--kind K] [--id ID ...] [--older-than D] [--dry-run]
//	                               delete dead-lettered jobs for good (needs at least one filter)
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

	"github.com/riyne/fifty-fifty/internal/anthropic"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/ocr"
	"github.com/riyne/fifty-fifty/internal/payclient"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/worker"
)

const usage = `usage:
  worker run [--concurrency N] [--lease D] [--poll D] [--grace D] [--metrics-addr HOST:PORT]
  worker requeue --kind KIND
  worker requeue --id JOB_ID [--id JOB_ID ...]
  worker delete-dead [--kind KIND] [--id JOB_ID ...] [--older-than DURATION] [--dry-run]

run claims jobs and runs their handlers until SIGINT or SIGTERM, then lets the
jobs in flight finish (up to --grace). Defaults: 4 jobs at once, 30s lease, 1s
poll, 20s grace. --metrics-addr (off by default) serves Prometheus metrics at
/metrics; the endpoint has no authentication, so bind it to a private interface
(for example 127.0.0.1:9102).

requeue moves dead-lettered jobs back to the queue with a fresh set of attempts
(their last_error is kept). It needs --kind or --id; there is no "all".

delete-dead removes dead-lettered jobs for good, so the dead-letter queue does not only grow. It
needs at least one of --kind, --id or --older-than (a Go duration such as 720h, measured from
when the job died); there is no "all", and a job must match every filter given. It touches only
dead jobs, and never one that a requeue has just revived. Run it with --dry-run first: that
prints the ids it would delete and deletes nothing. Deleting a dead job does not undo what it
was about (a receipt still waiting to be read, a settlement still pending: the sweep and the
watchdog deal with those), and its last_error goes with it; read it first, or requeue instead.
Both commands print one job id per line on stdout.
Needs DATABASE_URL, which must name the fifty_worker role (migration 00015), not the
application role fifty_app. If PAYMENTS_URL (the base URL of the payment provider, for
example http://127.0.0.1:8090) is set, the worker also runs settlement.execute and
settlement.reconcile jobs and the settlement watchdog; without it those jobs wait.

receipt.parse jobs (reading an uploaded receipt) are run only when RECEIPT_DIR (the
directory the api stores images in; the same one) and a model are configured: either
ANTHROPIC_API_KEY (the real API, https://api.anthropic.com unless ANTHROPIC_BASE_URL says
otherwise) or ANTHROPIC_BASE_URL alone for a loopback address (the mock LLM, which needs no
key). ANTHROPIC_MODEL overrides the default model. The tesseract program must be installed
(TESSERACT_PATH if it is not on the PATH); the worker refuses to start without it. Without
the configuration those jobs wait in the queue for a worker that can run them.`

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
	if len(args) == 0 || (args[0] != "requeue" && args[0] != "run" && args[0] != "delete-dead") {
		fmt.Fprintln(stderr, usage)
		return errUsage
	}
	// Parse before connecting, so a typo does not need a database.
	var (
		reqParams   queue.RequeueParams
		delParams   queue.DeleteDeadParams
		runCfg      worker.Config
		metricsAddr string
		err         error
	)
	switch args[0] {
	case "requeue":
		reqParams, err = parseRequeue(args[1:], stderr)
	case "delete-dead":
		delParams, err = parseDeleteDead(args[1:], stderr)
	default:
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

	switch args[0] {
	case "requeue":
		return requeue(ctx, pool, reqParams, stdout)
	case "delete-dead":
		return deleteDead(ctx, pool, delParams, stdout)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCfg.Pool = pool
	runCfg.Handlers, err = handlers(pool, os.Getenv("PAYMENTS_URL"), receiptConfigFromEnv())
	if err != nil {
		return err
	}
	runCfg.Periodic = []queue.PeriodicJob{worker.CleanupSchedule, worker.PruneSchedule, receiptparse.SweepSchedule}
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
// jobs it cannot run, and they wait in the queue for a worker that can.
func handlers(pool *pgxpool.Pool, paymentsURL string, rc receiptConfig) (map[string]worker.Handler, error) {
	h := map[string]worker.Handler{
		worker.KindIdempotencyCleanup: worker.IdempotencyCleanup(pool),
		worker.KindJobsPrune:          worker.JobsPrune(pool, worker.JobRetention),
		// The sweep needs no outside service: it only gives up on receipts whose parse
		// job died. The parse handler itself needs OCR and a model (see receiptHandler).
		receiptparse.KindSweep: receiptparse.SweepHandler(pool, receiptparse.SweepConfig{}),
	}
	parse, err := receiptHandler(pool, rc)
	if err != nil {
		return nil, err
	}
	if parse != nil {
		h[receiptparse.KindParse] = parse
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

// receiptConfig is the environment that decides whether this worker reads receipts.
type receiptConfig struct {
	Dir           string // RECEIPT_DIR: where the api stored the images
	APIKey        string // ANTHROPIC_API_KEY
	BaseURL       string // ANTHROPIC_BASE_URL
	Model         string // ANTHROPIC_MODEL
	TesseractPath string // TESSERACT_PATH
}

func receiptConfigFromEnv() receiptConfig {
	return receiptConfig{
		Dir: os.Getenv("RECEIPT_DIR"), APIKey: os.Getenv("ANTHROPIC_API_KEY"), BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		Model: os.Getenv("ANTHROPIC_MODEL"), TesseractPath: os.Getenv("TESSERACT_PATH"),
	}
}

// receiptHandler builds the receipt.parse handler, or returns nil when this worker is not
// set up to read receipts: it needs the image directory and a model. A half-configured
// worker logs what is missing and leaves the jobs for one that is complete. A fully
// configured one that cannot start (a bad URL, a key with a space in it, tesseract not
// installed) is an error: it must not look healthy and then fail every receipt.
func receiptHandler(pool *pgxpool.Pool, rc receiptConfig) (worker.Handler, error) {
	haveModel := rc.APIKey != "" || rc.BaseURL != ""
	switch {
	case rc.Dir == "" && !haveModel:
		return nil, nil
	case rc.Dir == "":
		slog.Warn("receipt.parse is not enabled: ANTHROPIC_API_KEY or ANTHROPIC_BASE_URL is set but RECEIPT_DIR is not")
		return nil, nil
	case !haveModel:
		slog.Warn("receipt.parse is not enabled: RECEIPT_DIR is set but neither ANTHROPIC_API_KEY nor ANTHROPIC_BASE_URL is")
		return nil, nil
	}
	store, err := objstore.NewFS(rc.Dir)
	if err != nil {
		return nil, fmt.Errorf("RECEIPT_DIR: %w", err)
	}
	client, err := anthropic.New(anthropic.Config{BaseURL: rc.BaseURL, APIKey: rc.APIKey, Model: rc.Model})
	if err != nil {
		return nil, fmt.Errorf("ANTHROPIC_*: %w", err)
	}
	reader, err := ocr.New(ocr.Config{Path: rc.TesseractPath})
	if err != nil {
		return nil, fmt.Errorf("receipt.parse needs tesseract: %w", err)
	}
	slog.Info("receipt.parse enabled", "dir", rc.Dir, "base_url", client.BaseURL(), "model", client.Model())
	return receiptparse.Handler(pool, store, reader, client, receiptparse.Config{}), nil
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

func parseDeleteDead(args []string, stderr io.Writer) (queue.DeleteDeadParams, error) {
	var ids idList
	fs := flag.NewFlagSet("delete-dead", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("kind", "", "delete the dead jobs of this kind")
	fs.Var(&ids, "id", "delete this dead job (repeatable)")
	olderThan := fs.Duration("older-than", 0, "delete the dead jobs that died at least this long ago (for example 720h)")
	dryRun := fs.Bool("dry-run", false, "print the ids that would be deleted and delete nothing")
	if err := fs.Parse(args); err != nil {
		return queue.DeleteDeadParams{}, errUsage
	}
	// No filter, or a negative age, is a usage error: there is no form that deletes everything.
	if fs.NArg() != 0 || *olderThan < 0 || (*kind == "" && len(ids) == 0 && *olderThan == 0) {
		fmt.Fprintln(stderr, usage)
		return queue.DeleteDeadParams{}, errUsage
	}
	return queue.DeleteDeadParams{IDs: ids, Kind: *kind, OlderThan: *olderThan, DryRun: *dryRun}, nil
}

// deleteDead prints one deleted (or, with --dry-run, would-be-deleted) job id per line on
// stdout; the count goes to the log.
func deleteDead(ctx context.Context, pool *pgxpool.Pool, p queue.DeleteDeadParams, stdout io.Writer) error {
	ids, err := queue.DeleteDead(ctx, pool, p)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(stdout, id)
	}
	msg := "deleted dead jobs"
	if p.DryRun {
		msg = "dry run: would delete dead jobs (nothing was deleted)"
	}
	slog.Info(msg, "count", len(ids), "kind", p.Kind, "ids_requested", len(p.IDs), "older_than", p.OlderThan.String())
	return nil
}
