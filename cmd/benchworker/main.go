// Command benchworker is a worker for the benchmark workload (the mock.extract job
// kind of internal/benchwork) and nothing else. It exists for the recovery test and
// the benchmarks, which start several of these as separate processes and kill them.
// The production worker (cmd/worker) does not register this kind.
//
//	DATABASE_URL=... benchworker --mock-url http://127.0.0.1:8089 --worker-id w1
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("benchworker exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mockURL, workerID string
		commitDelay       time.Duration
		lean              bool
		cfg               worker.Config
	)
	flag.StringVar(&mockURL, "mock-url", "", "base URL of the mock LLM (required)")
	flag.StringVar(&workerID, "worker-id", "", "name recorded on this worker's attempts and effects (required)")
	flag.DurationVar(&commitDelay, "commit-delay", 0, "hold each completion transaction open this long after the effect insert (tests only)")
	flag.BoolVar(&lean, "lean", false, "skip the attempt and commit-start bookkeeping writes (throughput runs)")
	flag.IntVar(&cfg.Concurrency, "concurrency", 4, "jobs in flight at once")
	flag.DurationVar(&cfg.Lease, "lease", 30*time.Second, "lease per claimed job")
	flag.DurationVar(&cfg.PollInterval, "poll", time.Second, "wait between polls when the queue is empty")
	flag.DurationVar(&cfg.ShutdownGrace, "grace", 20*time.Second, "how long jobs in flight may finish after SIGTERM")
	flag.Parse()
	dbURL := os.Getenv("DATABASE_URL")
	if mockURL == "" || workerID == "" || dbURL == "" {
		return errors.New("--mock-url, --worker-id and DATABASE_URL are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pcfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// The handler's phase two uses a second connection while the completion
	// transaction holds one, so a pool no bigger than the concurrency could have every
	// connection held by a transaction waiting for another.
	pcfg.MaxConns = int32(3*cfg.Concurrency + 4)
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	cfg.Pool = pool
	cfg.Handlers = map[string]worker.Handler{
		benchwork.Kind: benchwork.Handler(benchwork.HandlerConfig{
			Pool: pool, MockURL: mockURL, WorkerID: workerID, CommitDelay: commitDelay, Lean: lean,
		}),
	}
	return worker.Run(ctx, cfg)
}
