// Command worker is the Fifty Fifty job worker and its operator commands.
//
//	worker requeue --kind K        give every dead job of kind K a fresh set of attempts
//	worker requeue --id ID [--id ID ...]   the same for specific jobs
//
// (M3.5 adds `worker run`, the consumer loop.)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
)

const usage = `usage:
  worker requeue --kind KIND
  worker requeue --id JOB_ID [--id JOB_ID ...]

requeue moves dead-lettered jobs back to the queue with a fresh set of attempts
(their last_error is kept). It needs --kind or --id; there is no "all".
Needs DATABASE_URL.`

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
	if len(args) == 0 || args[0] != "requeue" {
		fmt.Fprintln(stderr, usage)
		return errUsage
	}
	// Parse before connecting, so a typo does not need a database.
	p, err := parseRequeue(args[1:], stderr)
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
	return requeue(ctx, pool, p, stdout)
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
