// Package benchwork is the test and benchmark workload for the queue: a job kind
// whose handler calls the mock LLM and then writes one effect row, plus the
// bookkeeping tables and checks the recovery and throughput runs use.
//
// It is not part of the product. Its tables live in their own schema, `bench`,
// created by Setup with the owner role, so the product schema (migrations/) carries
// nothing that exists only for benchmarks, and the production worker binary does not
// register the kind. cmd/benchworker is the worker that does.
//
// The handler follows the two-phase shape the worker documents: the LLM call is
// phase one (no transaction, may run more than once), and the effect row is written
// in phase two, inside the transaction that marks the job succeeded. So "duplicate
// effects" is exactly the question whether that transaction is atomic and fenced.
package benchwork

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// Kind is the job kind of the workload.
const Kind = "mock.extract"

// The tables, with timestamps from clock_timestamp() so they are comparable with
// the job's finished_at and the harness's own readings of the database clock.
//
//	attempts: one row per time a handler started a job (autocommit, so it survives
//	          the worker being killed). commit_started_at is set when the handler
//	          reaches phase two, again outside the completion transaction.
//	effects:  one row per Commit that got as far as inserting. If the completion
//	          transaction is atomic, a job has exactly one row once it has succeeded.
const schemaSQL = `
CREATE SCHEMA IF NOT EXISTS bench;
CREATE TABLE IF NOT EXISTS bench.attempts (
    job_id            uuid        NOT NULL,
    attempt           integer     NOT NULL,
    worker            text        NOT NULL,
    started_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    commit_started_at timestamptz,
    PRIMARY KEY (job_id, attempt)
);
CREATE TABLE IF NOT EXISTS bench.effects (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id     uuid        NOT NULL,
    attempt    integer     NOT NULL,
    worker     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS effects_job_idx ON bench.effects (job_id);
GRANT USAGE ON SCHEMA bench TO fifty_app;
GRANT SELECT, INSERT, UPDATE ON bench.attempts TO fifty_app;
GRANT SELECT, INSERT ON bench.effects TO fifty_app;
`

// Setup creates the bench schema. Call it with the owner role's pool; it is
// idempotent, and safe to run from several processes at once.
func Setup(ctx context.Context, owner *pgxpool.Pool) error {
	tx, err := owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// CREATE ... IF NOT EXISTS is not safe against a concurrent creator.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('fifty-fifty bench schema'))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("create bench schema: %w", err)
	}
	return tx.Commit(ctx)
}

// Payload is the job payload: the run it belongs to, so a run's jobs and effects
// can be told apart from those of any other run in the shared database.
type Payload struct {
	Run string `json:"run"`
}

// Enqueue adds n jobs of Kind for run, in one transaction, and returns their ids.
func Enqueue(ctx context.Context, pool *pgxpool.Pool, run string, n, maxAttempts int) ([]uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	ids := make([]uuid.UUID, 0, n)
	for range n {
		id, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: Kind, Payload: Payload{Run: run}, MaxAttempts: maxAttempts})
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, tx.Commit(ctx)
}

// HandlerConfig configures Handler.
type HandlerConfig struct {
	Pool     *pgxpool.Pool
	MockURL  string // base URL of cmd/mockllm, for example http://127.0.0.1:8089
	WorkerID string // recorded on every attempt and effect row

	// CommitDelay holds phase two open for this long after the effect row is
	// inserted and before the transaction can commit. It exists so a test can kill
	// the worker while an effect is written but not committed. Zero in benchmarks.
	CommitDelay time.Duration

	// CallTimeout bounds one mock call. Default 30s.
	CallTimeout time.Duration
}

// Handler is the worker handler for Kind.
func Handler(c HandlerConfig) worker.Handler {
	if c.CallTimeout == 0 {
		c.CallTimeout = 30 * time.Second
	}
	client := &http.Client{Timeout: c.CallTimeout}
	body := []byte(`{"model":"mock","max_tokens":256,"messages":[{"role":"user","content":"extract"}]}`)

	return func(ctx context.Context, job queue.Claimed) (worker.Commit, error) {
		_, err := c.Pool.Exec(ctx,
			`INSERT INTO bench.attempts (job_id, attempt, worker) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			job.ID, job.Attempt, c.WorkerID)
		if err != nil {
			return nil, fmt.Errorf("record attempt: %w", err)
		}
		if err := callMock(ctx, client, c.MockURL, body); err != nil {
			return nil, err
		}
		return func(ctx context.Context, tx pgx.Tx) error {
			// Outside tx on purpose: the harness must see that phase two began even
			// when this transaction is rolled back by a killed worker.
			if _, err := c.Pool.Exec(ctx,
				`UPDATE bench.attempts SET commit_started_at = clock_timestamp() WHERE job_id = $1 AND attempt = $2`,
				job.ID, job.Attempt); err != nil {
				return fmt.Errorf("record commit start: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO bench.effects (job_id, attempt, worker) VALUES ($1, $2, $3)`,
				job.ID, job.Attempt, c.WorkerID); err != nil {
				return fmt.Errorf("insert effect: %w", err)
			}
			if c.CommitDelay > 0 {
				select {
				case <-time.After(c.CommitDelay):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}, nil
	}
}

// callMock makes one Messages API call and requires a well-formed success. Anything
// else is an error, which fails the attempt and sends the job back through backoff.
func callMock(ctx context.Context, client *http.Client, baseURL string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mock llm request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("mock llm response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mock llm: status %d", resp.StatusCode)
	}
	var m struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Content) == 0 || m.Content[0].Text == "" {
		return fmt.Errorf("mock llm: malformed response")
	}
	return nil
}
